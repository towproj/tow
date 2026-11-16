package integration

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// lifecycleRun is where a lifecycle run builds its cluster.
type lifecycleRun struct {
	provider     string // proxmox (the lab), aws, gcp or azure
	dist         string
	region       string // aws, gcp, azure
	project      string // gcp
	subscription string // azure
}

// where is a cloud run's target.
func (r lifecycleRun) where() target {
	return target{cloud: r.provider, region: r.region, project: r.project, subscription: r.subscription}
}

// lifecycleWorkers is the worker count the cluster starts with: TOW_TEST_LIFECYCLE_WORKERS,
// else two for the conformance tests, else one. With two, Postgres's instances sit on two
// workers drained one after the other: the case tow's wait before each drain is for.
func lifecycleWorkers() int {
	if n, err := strconv.Atoi(os.Getenv("TOW_TEST_LIFECYCLE_WORKERS")); err == nil && n > 0 {
		return n
	}
	if conformance() {
		return 2
	}
	return 1
}

// workersLine is the file's worker pool line with its count, which stages edit.
func workersLine(count int) string { return fmt.Sprintf("    - name: workers\n      count: %d", count) }

// lifecycleFile is the cluster a lifecycle run starts from. On the Proxmox lab: k3s with three
// control planes behind kube-vip (schedulable: Postgres needs room) and a worker; kubeadm with
// one control plane behind kube-vip (so a second can join) and a worker, Cilium replacing
// kube-proxy. On AWS: kubeadm, one control plane and a worker, t3.small (the smallest size
// kubeadm accepts), Cilium replacing kube-proxy.
func lifecycleFile(r lifecycleRun, name, version string) string {
	kube, cps, worker := `distribution: k3s
    networking: {cni: flannel}
    apiEndpoint: 192.168.100.100:6443
    storage: {csi: [local-path]}`, "count: 3\n    schedulable: true", "resources: {cpu: 1, memory: 1Gi}"
	infra := `provider: proxmox
    proxmox: {url: https://192.168.100.10:8006, node: pve, template: 9000}
    network:
      subnet: 192.168.100.0/24
      addresses: 192.168.100.20-192.168.100.29
      gateway: 192.168.100.1
      dns: [1.1.1.1]`
	switch {
	case r.provider == config.ProviderAWS, r.provider == config.ProviderGCP, r.provider == config.ProviderAzure:
		instanceType := envOr("TOW_TEST_INSTANCE_TYPE", "t3.small")
		infra = "provider: aws\n    aws: {region: " + r.region + "}"
		switch r.provider {
		case config.ProviderGCP:
			instanceType = envOr("TOW_TEST_INSTANCE_TYPE", "e2-medium")
			infra = "provider: gcp\n    gcp: {project: " + r.project + ", region: " + r.region + "}"
		case config.ProviderAzure:
			instanceType = envOr("TOW_TEST_INSTANCE_TYPE", "Standard_B2s_v2")
			infra = "provider: azure\n    azure: {subscription: " + r.subscription + ", location: " + r.region + "}"
		}
		kube = `distribution: kubeadm
    networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}
    storage: {csi: [local-path]}`
		cps, worker = "count: 1\n    instanceType: "+instanceType, "instanceType: "+instanceType
	case r.dist == config.DistributionKubeadm:
		kube, cps, worker = `distribution: kubeadm
    networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}
    apiEndpoint: 192.168.100.100:6443
    storage: {csi: [local-path]}`, "count: 1", ""
	}
	return fmt.Sprintf(`apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata:
  name: %s
spec:
  kubernetes:
    %s
    version: %s
    kubeconfig: {merge: false}
    addons:
      helmCharts:
        - {name: cnpg, repo: https://cloudnative-pg.github.io/charts, chart: cloudnative-pg, version: 0.29.1, namespace: cnpg-system}
  infrastructure:
    %s
  controlPlane:
    %s
  workers:
%s
      %s
`, name, kube, version, infra, cps, workersLine(lifecycleWorkers()), worker)
}

// Stage titles the AWS run shares with the lab's, and the AWS run's only stages: what only
// AWS can show (OpenTofu adding a machine in another zone, the others untouched; a snapshot
// copied over the internet) and the lifecycle's main line. The lab's runs cover the rest.
const (
	stageCreate      = "create"
	stageReapply     = "re-apply unchanged"
	stageWorker      = "a worker added, then removed"
	stageControl     = "the control planes change"
	stageSettings    = "new settings"
	stageUpgrade     = "upgrade: a blocked drain, then resumed, Postgres written to throughout"
	stageCerts       = "certificates renewed"
	stageConformance = "conformance tests on the upgraded cluster"
	stageDelete      = "delete"
)

var cloudStages = []string{stageCreate, stageReapply, stageWorker, stageSettings, stageUpgrade, stageCerts, stageConformance, stageDelete}

// TestProxmoxLifecycle drives one cluster through its whole life on the Proxmox lab, the
// cluster checked after each stage: re-applied unchanged, a dry run, a worker and a control
// plane added and removed, add-ons on and off, the admin CIDRs, new settings, a backup, an
// upgrade (a drain blocked by a PodDisruptionBudget first, then resumed) with Postgres
// written to throughout, the pre-upgrade snapshot restored, certificates renewed.
// TOW_TEST_LIFECYCLE=k3s|kubeadm picks the distribution. With TOW_TEST_CONFORMANCE
// set (kubeadm only, make test-lifecycle-conformance), the cluster starts with two workers,
// keeps one control plane (the lab holds no fourth VM), and the conformance tests run once it
// was upgraded and restored.
func TestProxmoxLifecycle(t *testing.T) {
	dist := os.Getenv("TOW_TEST_LIFECYCLE")
	if os.Getenv("INTEGRATION_PROXMOX") == "" || dist == "" {
		t.Skip("set INTEGRATION_PROXMOX=1 and TOW_TEST_LIFECYCLE=k3s|kubeadm to run a cluster's whole life on your Proxmox server")
	}
	if os.Getenv("PROXMOX_VE_API_TOKEN") == "" {
		t.Fatal("PROXMOX_VE_API_TOKEN is not set; see docs/testing/proxmox.md")
	}
	if conformance() && dist != config.DistributionKubeadm {
		t.Fatal("the conformance tests run on the kubeadm lifecycle (TOW_TEST_LIFECYCLE=kubeadm)")
	}
	runLifecycle(t, lifecycleRun{provider: config.ProviderProxmox, dist: dist})
}

// TestAWSLifecycle drives a kubeadm cluster through the stages only AWS can show, on real
// t3.small servers: created at v1.36.6, re-applied unchanged, a worker added (in a zone the
// others don't use, the machines already there untouched) and removed, new settings, the
// upgrade to v1.37.2 (its snapshot copied here over the internet) with Postgres written to
// throughout, certificates renewed, deleted.
//
// Cost: two t3.small for about 45 minutes, three while the worker is added: about $0.04.
// The cluster is deleted even after a failure (TOW_TEST_LIFECYCLE_KEEP=1 keeps it, to fix
// the test on it), the run then waits for AWS to report every machine terminated (exit code 3
// with what to run, if not), and the machines shut themselves down after 2 hours anyway.
// With TOW_TEST_CONFORMANCE set, as on every cloud, the cluster starts with two workers and
// the conformance tests run once it was upgraded (about 1.5 hours more).
func TestAWSLifecycle(t *testing.T) {
	if os.Getenv("INTEGRATION_AWS_LIFECYCLE") == "" {
		t.Skip("set INTEGRATION_AWS_LIFECYCLE=1 to drive a real kubeadm cluster through its life on AWS (about $0.04)")
	}
	runLifecycle(t, lifecycleRun{provider: config.ProviderAWS, dist: config.DistributionKubeadm, region: envOr("TOW_TEST_REGION", "eu-west-3")})
}

// TestGCPLifecycle is TestAWSLifecycle on GCP (two e2-medium, three while the worker is
// added, in TOW_TEST_GCP_PROJECT): the same stages, the new worker in a zone the others
// don't use, read from the metadata server. Cost: about $0.05.
func TestGCPLifecycle(t *testing.T) {
	if os.Getenv("INTEGRATION_GCP_LIFECYCLE") == "" {
		t.Skip("set INTEGRATION_GCP_LIFECYCLE=1 and TOW_TEST_GCP_PROJECT to drive a real kubeadm cluster through its life on GCP (about $0.05)")
	}
	g := gcpTarget(t)
	runLifecycle(t, lifecycleRun{provider: config.ProviderGCP, dist: config.DistributionKubeadm, region: g.region, project: g.project})
}

// TestAzureLifecycle is TestAWSLifecycle on Azure (two Standard_B2s_v2, three while the worker is
// added, in TOW_TEST_AZURE_SUBSCRIPTION): the same stages, the new worker in a zone the
// others don't use, read from the instance metadata. Cost: about $0.07.
func TestAzureLifecycle(t *testing.T) {
	if os.Getenv("INTEGRATION_AZURE_LIFECYCLE") == "" {
		t.Skip("set INTEGRATION_AZURE_LIFECYCLE=1 and TOW_TEST_AZURE_SUBSCRIPTION to drive a real kubeadm cluster through its life on Azure (about $0.07)")
	}
	a := azureTarget(t)
	runLifecycle(t, lifecycleRun{provider: config.ProviderAzure, dist: config.DistributionKubeadm, region: a.region, subscription: a.subscription})
}

func runLifecycle(t *testing.T, r lifecycleRun) {
	// onCloud: a public cloud (AWS, GCP, Azure), where the run covers the stages only a cloud
	// shows.
	dist, onCloud := r.dist, r.provider == config.ProviderAWS || r.provider == config.ProviderGCP || r.provider == config.ProviderAzure
	ttl, deadline, again := "3h", 2*time.Hour+30*time.Minute, "make test-lifecycle DIST="+dist
	if onCloud {
		ttl, deadline, again = "2h", 75*time.Minute, "TOW_TEST_LIFECYCLE_KEEP=1 make test-"+r.provider+"-lifecycle"
	}
	if conformance() {
		// The machines' own deadline grows with the run's: they'd shut down mid-tests.
		ttl, deadline = "6h", deadline+3*time.Hour
		again = "make test-lifecycle-conformance"
		if onCloud {
			again = "TOW_TEST_CONFORMANCE=1 TOW_TEST_LIFECYCLE_KEEP=1 make test-" + r.provider + "-lifecycle"
		}
	}
	t.Setenv("TOW_RESOURCE_TTL", ttl)
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig"))
	ui.SetNonInteractive(true)
	// A failed run keeps its cluster (on AWS, with TOW_TEST_LIFECYCLE_KEEP=1):
	// TOW_TEST_LIFECYCLE_NAME reuses it, and TOW_TEST_LIFECYCLE_FROM starts at a stage
	// (its title's start), the stages before skipped (their lasting edits to the file still
	// made). That's how the test itself is fixed: on a running cluster, never through whole
	// runs again.
	name := os.Getenv("TOW_TEST_LIFECYCLE_NAME")
	if name == "" {
		name = testName("test-life")
	}
	fromStage := os.Getenv("TOW_TEST_LIFECYCLE_FROM")
	failedAt := ""
	dir := t.TempDir()
	from, to := "v1.36.6", "v1.37.2"
	if dist == config.DistributionK3s {
		from = "v1.36.5"
	}
	// kubectl of the cluster's minor release (tow drains with it); the upgrade's comes later.
	ensureTools(t, from)
	ensureTofu(t)

	yes := func(string) bool { return true }
	orch := func() *core.Orchestrator { return core.NewOrchestrator(false).AssumeYes() }
	t.Cleanup(func() {
		if t.Failed() && state.Exists(name) && (!onCloud || os.Getenv("TOW_TEST_LIFECYCLE_KEEP") != "") {
			t.Logf("the cluster stays, to fix the test on it: TOW_TEST_LIFECYCLE_NAME=%s TOW_TEST_LIFECYCLE_FROM=%q %s (or 'tow delete --name %s --yes -v')",
				name, failedAt, again, name)
			return
		}
		os.RemoveAll(state.BackupsDir(name))
		var deleteErr error
		if state.Exists(name) {
			// Fresh context: the run's own may already be cancelled (deadline or Ctrl+C).
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			if deleteErr = core.NewOrchestrator(false).WithVolumes().Delete(ctx, name, yes); deleteErr != nil && !onCloud {
				t.Errorf("cleanup failed; delete the VMs tagged tow-cluster.%s in the Proxmox web UI: %v", name, deleteErr)
			}
		}
		if onCloud {
			confirmTerminated(t, r.where(), name, deleteErr)
		}
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	// The file as it is now; each stage edits its text and applies it.
	text := lifecycleFile(r, name, from)
	workers := lifecycleWorkers()
	load := func(t *testing.T, s string) *config.Config {
		t.Helper()
		path := filepath.Join(dir, "tow.yaml")
		os.WriteFile(path, []byte(s), 0o600)
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatalf("the file: %v\n%s", err, s)
		}
		return cfg
	}
	apply := func(t *testing.T, o *core.Orchestrator, s string) (string, error) {
		t.Helper()
		var err error
		out := captured(func() { err = o.Apply(ctx, load(t, s)) })
		t.Log(out)
		return out, err
	}
	edit := func(t *testing.T, from, to string) {
		t.Helper()
		if !strings.Contains(text, from) {
			t.Fatalf("the file has no %q", from)
		}
		text = strings.Replace(text, from, to, 1)
	}
	healthy := func(t *testing.T) {
		t.Helper()
		checkStatus(t, ctx, name)
	}
	// notHere says why a stage doesn't run in this run at all: the lab's k3s run covers it on
	// kubeadm (sharedOnly), the lab's runs cover it on a cloud, or the lab has no room for it.
	notHere := func(title string, sharedOnly bool) string {
		switch {
		case onCloud && !slices.Contains(cloudStages, title):
			return "on " + config.CloudName(r.provider) + " (the lab's runs cover it)"
		case !onCloud && sharedOnly && dist == config.DistributionKubeadm:
			return "on kubeadm (k3s's run covers it)"
		case !onCloud && lifecycleWorkers() > 1 && title == stageControl:
			return "with two workers (the lab holds no fourth VM)"
		case !conformance() && title == stageConformance:
			return "without TOW_TEST_CONFORMANCE"
		}
		return ""
	}
	// stage runs one stage; lasting is the edit it leaves in the file, made too when the stage
	// is skipped (a resumed run), so the file matches the cluster. Stages before fromStage are
	// skipped; so are those notHere names.
	started := fromStage == ""
	fresh := false        // a cluster made by this run: the skipped stages never changed it
	markedBefore := false // the upgrade stage left its marker in the pre-upgrade snapshot
	stage := func(title string, sharedOnly bool, lasting func(), f func(t *testing.T)) {
		if why := notHere(title, sharedOnly); why != "" {
			t.Logf("skipped %s: %s", why, title)
			return
		}
		if !started && title == "create" && !state.Exists(name) {
			// A new cluster is created even when starting later: the stages from fromStage
			// on then run on it.
			fresh = true
			if !t.Run(title, f) {
				failedAt = title
				t.FailNow()
			}
			return
		}
		if !started && strings.HasPrefix(title, fromStage) {
			started = true
		}
		if !started {
			t.Logf("skipped (starting at %q): %s", fromStage, title)
			if lasting != nil && !fresh {
				lasting() // the kept cluster has that stage's changes: the file says so too
			}
			return
		}
		if !t.Run(title, f) {
			failedAt = title
			t.FailNow() // each stage builds on the one before
		}
	}

	stage(stageCreate, false, nil, func(t *testing.T) {
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		st, _ := state.Load(name)
		cfg := load(t, text)
		present, absent := systemPods(cfg)
		checkCluster(t, ctx, clusterCheck{cfg: cfg, st: st, present: present, absent: absent, nodePortHost: st.Outputs.Nodes[0].Host})
	})
	cs := clientset(t, mustState(t, name))

	stage(stageReapply, false, nil, func(t *testing.T) {
		out, err := apply(t, orch(), text)
		if err != nil || !strings.Contains(out, "nothing to do") {
			t.Fatalf("%v\n%s", err, out)
		}
	})

	stage(stageWorker, true, nil, func(t *testing.T) {
		var before map[string]placement
		if onCloud {
			before = placements(t, ctx, name, r.provider)
		}
		edit(t, workersLine(workers), workersLine(workers+1))
		out, err := apply(t, core.NewOrchestrator(true), text)
		infra, cluster := strings.Index(out, "Infrastructure (OpenTofu"), strings.Index(out, "Cluster (Kubernetes")
		if err != nil || infra < 0 || cluster < infra || !strings.Contains(out, "Join the new machines") {
			t.Fatalf("the dry run shows Infrastructure, then Cluster: %v\n%s", err, out)
		}
		if strings.Contains(out, "destroy") || strings.Contains(out, "replace") {
			t.Errorf("adding a worker replaces nothing:\n%s", out)
		}
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		healthy(t)
		if onCloud {
			// Each machine keeps its server and zone; the new one goes to a zone with the fewest
			// of them: one none of them uses while the region has one (with two workers, the
			// conformance runs', the three zones are all taken).
			after, added := placements(t, ctx, name, r.provider), fmt.Sprintf("%s-workers-%d", name, workers)
			inZone := map[string]int{}
			for m, p := range before {
				if after[m] != p {
					t.Errorf("%s changed: %+v, then %+v", m, p, after[m])
				}
				inZone[p.zone]++
			}
			fewest := 0
			if len(inZone) >= 3 { // the regions tow's tests use have three zones
				fewest = slices.Min(slices.Collect(maps.Values(inZone)))
			}
			if n := inZone[after[added].zone]; n > fewest {
				t.Errorf("%s went to %s, which already has %d of the machines (one has %d): %+v", added, after[added].zone, n, fewest, before)
			}
			if after[added].zone == "" {
				t.Errorf("no zone known for %s: %+v", added, after)
			}
		}
		edit(t, workersLine(workers+1), workersLine(workers))
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		healthy(t)
	})

	// k3s (three already): one removed and added back; kubeadm: a second added, then removed.
	// The lab's memory holds one more VM at most.
	stage(stageControl, false, nil, func(t *testing.T) {
		count, more := "count: 3\n    schedulable: true", "count: 2\n    schedulable: true"
		if dist == config.DistributionKubeadm {
			count, more = "count: 1", "count: 2"
		}
		edit(t, count, more)
		out, err := apply(t, orch(), text)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out, "control planes: etcd needs a majority") {
			t.Errorf("an even number of control planes is warned about:\n%s", out)
		}
		healthy(t)
		edit(t, more, count)
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		healthy(t)
	})

	stage("add-ons on and off", true, nil, func(t *testing.T) {
		chart := "\n        - {name: podinfo, repo: oci://ghcr.io/stefanprodan/charts, chart: podinfo, version: 6.9.2, namespace: podinfo}"
		edit(t, "namespace: cnpg-system}", "namespace: cnpg-system}"+chart)
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		eventuallyWithin(t, ctx, 5*time.Minute, "podinfo runs", func() bool {
			d, err := cs.AppsV1().Deployments("podinfo").Get(ctx, "podinfo", metav1.GetOptions{})
			return err == nil && d.Status.ReadyReplicas > 0
		})
		edit(t, chart, "")
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		eventuallyWithin(t, ctx, 3*time.Minute, "podinfo removed", func() bool {
			_, err := cs.AppsV1().Deployments("podinfo").Get(ctx, "podinfo", metav1.GetOptions{})
			return err != nil
		})
		healthy(t)
	})

	stage("the admin CIDRs", true, nil, func(t *testing.T) {
		edit(t, "    proxmox: {url:", "    access: {adminCIDRs: [192.168.100.1/32, 198.51.100.0/24]}\n    proxmox: {url:")
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		edit(t, "    access: {adminCIDRs: [192.168.100.1/32, 198.51.100.0/24]}\n", "")
		if _, err := apply(t, orch(), text); err != nil {
			t.Fatal(err)
		}
		healthy(t)
	})

	settings := func() {
		text = strings.Replace(text, "    kubeconfig: {merge: false}", "    apiServer: {extraArgs: {audit-log-maxage: \"45\"}}\n    kubeconfig: {merge: false}", 1)
		text = strings.Replace(text, workersLine(workers), workersLine(workers)+"\n      kubelet: {extraArgs: {max-pods: \"150\"}}", 1)
	}
	stage(stageSettings, true, settings, func(t *testing.T) {
		settings()
		out, err := apply(t, orch(), text)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "Draining") {
			t.Error("settings never drain")
		}
		eventuallyWithin(t, ctx, 3*time.Minute, "the worker takes 150 pods", func() bool {
			n, err := cs.CoreV1().Nodes().Get(ctx, name+"-workers-0", metav1.GetOptions{})
			return err == nil && n.Status.Capacity.Pods().Value() == 150
		})
		healthy(t)
	})

	stage("a marker, then a backup", false, nil, func(t *testing.T) {
		mark(t, ctx, cs, "before-upgrade")
		if _, err := orch().Backup(ctx, name); err != nil {
			t.Fatal(err)
		}
	})

	failedAt = stageUpgrade // a failure here is resumed there
	ensureTools(t, to)      // an upgrade drains with the new release's kubectl, and the cluster runs it after
	upgraded := func() { text = strings.Replace(text, "version: "+from, "version: "+to, 1) }
	stage(stageUpgrade, false, upgraded, func(t *testing.T) {
		// Postgres: CloudNativePG (its operator came with the file), two instances.
		pg := newPostgres(t, ctx, cs, mustState(t, name).Outputs.Kubeconfig)
		// In the snapshot the upgrade takes first; the restore below brings it back.
		mark(t, ctx, cs, "before-upgrade")
		markedBefore = true
		upgraded()
		// The worker's drain is blocked: a pod nothing else can take, kept by a budget.
		block(t, ctx, cs, name+"-workers-0")
		pg.start()
		out, err := apply(t, orch().WithDrainTimeout(2*time.Minute), text)
		if err == nil || !strings.Contains(out+err.Error(), "disruption budget") || !strings.Contains(err.Error(), "so skipped when you run it again") {
			t.Fatalf("the blocked drain: %v\n%s", err, out)
		}
		cs.PolicyV1().PodDisruptionBudgets("default").Delete(ctx, "blocker", metav1.DeleteOptions{})
		out, err = apply(t, orch(), text)
		if err != nil || !strings.Contains(out, "already at") {
			t.Fatalf("the resumed upgrade: %v\n%s", err, out)
		}
		pg.stop(t)
		healthy(t)
		nodes, _ := cs.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
		for _, n := range nodes.Items {
			if !strings.HasPrefix(n.Status.NodeInfo.KubeletVersion, to) {
				t.Errorf("%s runs %s", n.Name, n.Status.NodeInfo.KubeletVersion)
			}
		}
		// The snapshot taken first came here whole (streamed over SSH, its sha256 checked on
		// both ends), and still matches what its .json says.
		backups, _ := core.ListBackups(name)
		i := slices.IndexFunc(backups, func(b core.BackupInfo) bool { return b.BeforeUpgradeTo == to })
		if i < 0 {
			t.Fatalf("the upgrade took no snapshot: %+v", backups)
		}
		if fi, err := os.Stat(backups[i].File); err != nil || fi.Size() != backups[i].Size || fi.Size() < 1<<20 || fi.Mode().Perm() != 0o600 {
			t.Errorf("the pre-upgrade snapshot %s: %v, %v (its .json says %d bytes)", backups[i].File, fi, err, backups[i].Size)
		}
	})

	stage("the pre-upgrade snapshot restored", false, nil, func(t *testing.T) {
		mark(t, ctx, cs, "after-upgrade")
		var pre string
		backups, _ := core.ListBackups(name)
		for _, b := range backups {
			if b.BeforeUpgradeTo == to {
				pre = filepath.Base(b.File)
			}
		}
		if pre == "" {
			t.Fatal("the upgrade took no snapshot")
		}
		var err error
		out := captured(func() { err = orch().WithoutBackup().Restore(ctx, name, pre) })
		t.Log(out)
		if dist == config.DistributionKubeadm && err != nil && strings.Contains(err.Error(), "etcd restores only snapshots of its own minor release") {
			t.Skip("the upgrade moved etcd to another minor release: refused, as etcd's docs say")
		}
		if err != nil {
			t.Fatal(err)
		}
		// The marker from before the upgrade is checked when this run left it (a run that
		// starts at the restore, on a kept cluster, didn't).
		eventuallyWithin(t, ctx, 5*time.Minute, "the marker from after the upgrade gone (and the one from before back)", func() bool {
			_, before := cs.CoreV1().ConfigMaps("default").Get(ctx, "before-upgrade", metav1.GetOptions{})
			_, after := cs.CoreV1().ConfigMaps("default").Get(ctx, "after-upgrade", metav1.GetOptions{})
			return (before == nil || !markedBefore) && after != nil
		})
		healthy(t)
	})

	stage(stageCerts, false, nil, func(t *testing.T) {
		st := mustState(t, name)
		old, _ := os.ReadFile(st.Outputs.Kubeconfig)
		if _, err := apply(t, orch().WithCertRenewal(), text); err != nil {
			t.Fatal(err)
		}
		now, _ := os.ReadFile(st.Outputs.Kubeconfig)
		if bytes.Equal(old, now) {
			t.Error("the kubeconfig wasn't fetched again")
		}
		healthy(t)
	})

	// Kubernetes' conformance tests, on a cluster that went through all of the above.
	stage(stageConformance, false, nil, func(t *testing.T) {
		checkConformance(t, ctx, mustState(t, name))
	})

	stage(stageDelete, false, nil, func(t *testing.T) {
		if err := core.NewOrchestrator(false).WithVolumes().Delete(ctx, name, yes); err != nil {
			t.Fatal(err)
		}
	})
}

// placement is the cloud server a machine runs on, and its zone.
type placement struct{ instance, zone string }

// metadataScripts read the instance's id and zone from its cloud's metadata server, one per
// line: AWS's IMDSv2 (a token first), Compute Engine's (its header; the zone comes as
// projects/<number>/zones/<zone>).
var metadataScripts = map[string]string{
	config.ProviderAWS: `t=$(curl -fsS --max-time 5 -X PUT http://169.254.169.254/latest/api/token -H 'X-aws-ec2-metadata-token-ttl-seconds: 60')
for p in instance-id placement/availability-zone; do
  curl -fsS --max-time 5 -H "X-aws-ec2-metadata-token: $t" http://169.254.169.254/latest/meta-data/$p; echo
done`,
	config.ProviderGCP: `for p in id zone; do
  curl -fsS --max-time 5 -H 'Metadata-Flavor: Google' http://metadata.google.internal/computeMetadata/v1/instance/$p | sed 's|.*/||'; echo
done`,
	config.ProviderAzure: `for p in vmId zone; do
  curl -fsS --max-time 5 -H 'Metadata: true' "http://169.254.169.254/metadata/instance/compute/$p?api-version=2021-02-01&format=text"; echo
done`,
}

// placements asks each of the cluster's machines, over SSH, which server and zone it is.
func placements(t *testing.T, ctx context.Context, name, provider string) map[string]placement {
	t.Helper()
	known := filepath.Join(state.Dir(name), "known_hosts")
	out := map[string]placement{}
	for _, n := range mustState(t, name).Outputs.Nodes {
		c, err := remote.Dial(ctx, remote.Target{Host: n.Host, Port: n.Port, User: n.User, KeyPath: n.KeyPath}, known)
		if err != nil {
			t.Fatalf("ssh %s: %v", n.Name, err)
		}
		got, err := c.Run(ctx, metadataScripts[provider])
		c.Close()
		lines := strings.Fields(got)
		if err != nil || len(lines) != 2 {
			t.Fatalf("%s's metadata: %v\n%s", n.Name, err, got)
		}
		out[n.Name] = placement{instance: lines[0], zone: lines[1]}
	}
	t.Logf("machines: %+v", out)
	return out
}

func mustState(t *testing.T, name string) *state.ClusterState {
	t.Helper()
	st, err := state.Load(name)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// captured runs f and returns what it printed.
func captured(f func()) string {
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		io.Copy(&b, r)
		done <- b.String()
	}()
	f()
	w.Close()
	os.Stdout = orig
	return <-done
}

// mark leaves a ConfigMap: what a restore must bring back, or take away.
func mark(t *testing.T, ctx context.Context, cs kubernetes.Interface, name string) {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name}, Data: map[string]string{"at": time.Now().String()}}
	if _, err := cs.CoreV1().ConfigMaps("default").Create(ctx, cm, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// block keeps a node's drain from finishing: a Deployment's one pod, kept on that node, and
// a budget that allows no disruption of it. Once the budget goes, the drain evicts it.
func block(t *testing.T, ctx context.Context, cs kubernetes.Interface, node string) {
	t.Helper()
	labels := map[string]string{"app": "blocker"}
	one32 := int32(1)
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "blocker"},
		Spec: appsv1.DeploymentSpec{Replicas: &one32, Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{NodeSelector: map[string]string{"kubernetes.io/hostname": node},
					Containers: []corev1.Container{{Name: "pause", Image: "registry.k8s.io/pause:3.10"}}}}}}
	if _, err := cs.AppsV1().Deployments("default").Create(ctx, d, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	eventuallyWithin(t, ctx, 3*time.Minute, "the blocker runs on "+node, func() bool {
		d, err := cs.AppsV1().Deployments("default").Get(ctx, "blocker", metav1.GetOptions{})
		return err == nil && d.Status.ReadyReplicas == 1
	})
	one := intstr.FromInt32(1)
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "blocker"},
		Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: &one, Selector: &metav1.LabelSelector{MatchLabels: labels}}}
	if _, err := cs.PolicyV1().PodDisruptionBudgets("default").Create(ctx, pdb, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
		t.Fatal(err)
	}
}

// anyNode is a node name no node has: runPsql's pods may then go anywhere.
const anyNode = "no-such-node"

// psqlJob runs one psql command against db-rw as a Job: a drain evicts its pod (one with no
// controller would stop the drain, which never forces), and it returns what psql printed.
func psqlJob(ctx context.Context, cs kubernetes.Interface, name, image, sql string) (string, error) {
	const ns = "postgres"
	zero := int32(0)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: batchv1.JobSpec{BackoffLimit: &zero, Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"psql": name}},
			Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever,
				Containers: []corev1.Container{{Name: "psql", Image: image, Command: []string{"psql", "$(URI)", "-tA", "-c", sql},
					Env: []corev1.EnvVar{{Name: "URI", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "db-app"}, Key: "uri"}}}}}}}}}}
	if _, err := cs.BatchV1().Jobs(ns).Create(ctx, job, metav1.CreateOptions{}); err != nil {
		return "", err
	}
	background := metav1.DeletePropagationBackground
	defer cs.BatchV1().Jobs(ns).Delete(context.WithoutCancel(ctx), name, metav1.DeleteOptions{PropagationPolicy: &background})
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	for {
		pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "psql=" + name})
		if err == nil {
			for _, p := range pods.Items {
				if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
					logs, _ := cs.CoreV1().Pods(ns).GetLogs(p.Name, &corev1.PodLogOptions{}).DoRaw(ctx)
					if p.Status.Phase == corev1.PodFailed {
						return string(logs), fmt.Errorf("psql failed: %s", logs)
					}
					return string(logs), nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("psql job %s didn't finish", name)
		case <-time.After(3 * time.Second):
		}
	}
}

// postgres is a CloudNativePG cluster written to while the cluster changes underneath.
type postgres struct {
	ctx      context.Context
	cs       kubernetes.Interface
	image    string
	mu       sync.Mutex
	written  []string // rows psql said were committed
	failures []time.Time
	cancel   context.CancelFunc
	done     chan struct{}
}

// newPostgres starts a two-instance CloudNativePG cluster. Its primary's PodDisruptionBudget
// is off (enablePDB: false): CloudNativePG's docs say a drain of the primary's node is
// otherwise blocked until a switchover; without it, the primary is evicted and a replica
// takes over, which is what this shows. Replication stays CloudNativePG's default,
// asynchronous; the replica's readiness probe is "streaming" with a maximum lag, so it's
// Ready only once it streams from the primary and has caught up. tow drains a node only
// once the workloads with a pod there have their other pods Ready: the primary's node isn't
// drained while the replica, back on the node drained before, is still catching up (the lab
// lost committed rows that way, before tow waited), and the primary's orderly shutdown
// hands what it has to a replica that streams.
func newPostgres(t *testing.T, ctx context.Context, cs kubernetes.Interface, kubeconfig string) *postgres {
	t.Helper()
	const ns = "postgres"
	cs.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{})
	manifest := `apiVersion: postgresql.cnpg.io/v1
kind: Cluster
metadata: {name: db, namespace: postgres}
spec:
  instances: 2
  enablePDB: false
  probes:
    readiness: {type: streaming, maximumLag: 16Mi}
  storage: {size: 1Gi, storageClass: local-path}
`
	cmd := exec.CommandContext(ctx, "kubectl", "apply", "--kubeconfig", kubeconfig, "-f", "-")
	cmd.Stdin = strings.NewReader(manifest)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("CloudNativePG's cluster: %v\n%s", err, out)
	}
	p := &postgres{ctx: ctx, cs: cs}
	eventuallyWithin(t, ctx, 15*time.Minute, "2 Postgres instances Ready", func() bool {
		list, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "cnpg.io/cluster=db,cnpg.io/podRole=instance"})
		if err != nil {
			return false
		}
		ready := slices.DeleteFunc(list.Items, func(p corev1.Pod) bool { return !podReady(p) })
		if len(ready) == 2 {
			p.image = ready[0].Spec.Containers[0].Image
		}
		return len(ready) == 2
	})
	if _, err := runPsql(ctx, cs, ns, "psql-init", p.image, anyNode, "CREATE TABLE IF NOT EXISTS writes (n text)"); err != nil {
		t.Fatal(err)
	}
	return p
}

// start writes a row every few seconds until stop.
func (p *postgres) start() {
	ctx, cancel := context.WithCancel(p.ctx)
	p.cancel, p.done = cancel, make(chan struct{})
	go func() {
		defer close(p.done)
		for i := 0; ctx.Err() == nil; i++ {
			row := fmt.Sprintf("row-%d", i)
			if _, err := psqlJob(ctx, p.cs, fmt.Sprintf("psql-w%d", i), p.image, "INSERT INTO writes VALUES ('"+row+"')"); err == nil {
				p.mu.Lock()
				p.written = append(p.written, row)
				p.mu.Unlock()
			} else if ctx.Err() == nil {
				p.mu.Lock()
				p.failures = append(p.failures, time.Now())
				p.mu.Unlock()
			}
			select {
			case <-ctx.Done():
			case <-time.After(3 * time.Second):
			}
		}
	}()
}

// stop ends the writes, then checks every committed row is there.
func (p *postgres) stop(t *testing.T) {
	t.Helper()
	p.cancel()
	<-p.done
	var out string
	var err error
	eventuallyWithin(t, p.ctx, 5*time.Minute, "Postgres answers after the upgrade", func() bool {
		out, err = runPsql(p.ctx, p.cs, "postgres", fmt.Sprintf("psql-read-%d", time.Now().Unix()), p.image, anyNode, "SELECT n FROM writes")
		return err == nil
	})
	for _, row := range p.written {
		if !strings.Contains(out, row+"\n") && !strings.HasSuffix(strings.TrimSpace(out), row) {
			t.Errorf("committed row %s is missing", row)
		}
	}
	t.Logf("Postgres during the upgrade: %d rows committed, %d writes failed (pods moving)", len(p.written), len(p.failures))
	if len(p.written) == 0 {
		t.Error("no write succeeded during the upgrade")
	}
}
