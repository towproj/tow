package core

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/k3s"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
)

// What the file asks is an upgrade only when it names a newer release; what can't be done
// is refused with the reason.
func TestUpgradeOf(t *testing.T) {
	k3sLab := fmt.Sprintf(lab, "", "")
	file := func(version string) *config.Config {
		return parse(t, "lab", strings.Replace(k3sLab, "distribution: k3s", "distribution: k3s"+version, 1))
	}
	prev := &state.ClusterState{K8sVersion: "v1.36.6"}
	for _, tc := range []struct {
		version string
		changes []string
		want    string // the release, or the refusal's words
	}{
		{version: ""},
		{version: ", version: stable"},
		{version: ", version: v1.36.6"},
		{version: ", version: v1.36.6+k3s1"},
		{version: ", version: v1.36.7", want: "v1.36.7+k3s1"},
		{version: ", version: v1.36.6+k3s2", want: "v1.36.6+k3s2"},
		{version: ", version: v1.37.2", changes: []string{"spec.kubernetes.version: v1.36.6 → v1.37.2"}, want: "v1.37.2+k3s1"},
		{version: ", version: v1.36.5", want: "v1.36.5+k3s1 is older than v1.36.6+k3s1, which runs"},
		{version: ", version: v1.39.0", want: "skips v1.37, v1.38: Kubernetes upgrades one minor release at a time, so upgrade to v1.37 first"},
		{version: ", version: v1.37.2", changes: []string{"spec.workers[general].labels.tier: web → batch"}, want: "an upgrade goes alone; apply the file's other changes first, with version v1.36.6, then the new version (changed too: spec.workers[general].labels.tier)"},
		{version: ", version: v1.37.2", changes: []string{"spec.kubernetes.networking.cilium.version: 1.18.4 → 1.19.1"}, want: "v1.37.2+k3s1"},
		{version: ", version: v1.37.2", changes: []string{"spec.kubernetes.addons.helmCharts[podinfo].version: 6.9.2 → 6.9.3"}, want: "goes alone"},
	} {
		up, refused := upgradeOf(file(tc.version), prev, tc.changes)
		switch {
		case tc.want == "" && (up != nil || refused != nil):
			t.Errorf("%q: %+v %v, want no upgrade", tc.version, up, refused)
		case strings.HasPrefix(tc.want, "v1.") && !strings.Contains(tc.want, " ") && (up == nil || up.release != tc.want):
			t.Errorf("%q: %+v %v, want an upgrade to %s", tc.version, up, refused, tc.want)
		case strings.Contains(tc.want, " ") && (len(refused) != 1 || !strings.Contains(refused[0], tc.want)):
			t.Errorf("%q: %v, want %q", tc.version, refused, tc.want)
		}
	}
	kubeadmFile := parse(t, "lab", strings.Replace(k3sLab, "distribution: k3s", "distribution: kubeadm, version: v1.37.2", 1))
	if up, refused := upgradeOf(kubeadmFile, prev, nil); up == nil || up.release != "v1.37.2" || refused != nil {
		t.Errorf("kubeadm: %+v %v", up, refused)
	}
}

// upgradeWorld is the nodes' releases, and what the upgrade did, in order.
type upgradeWorld struct {
	versions map[string]string
	calls    []string
	waitErr  map[string]error
}

func (w *upgradeWorld) NodeMetas(context.Context) ([]kubeapi.NodeMeta, error) {
	var out []kubeapi.NodeMeta
	for _, n := range []string{"cp", "w1"} {
		out = append(out, kubeapi.NodeMeta{Name: n, Version: w.versions[n], Ready: true})
	}
	out[0].Taints = []kubeapi.Taint{controlPlaneTaint}
	return out, nil
}
func (w *upgradeWorld) Relabel(context.Context, string, map[string]string, []string) error {
	return nil
}
func (w *upgradeWorld) Retaint(context.Context, string, []kubeapi.Taint, []string) error { return nil }

// useUpgrade fakes the nodes (cp and w1, running v1.37.2+k3s1) and the steps of a rolling
// upgrade; k3s's install calls Rolling for each node that doesn't run the release yet.
func useUpgrade(t *testing.T) *upgradeWorld {
	t.Helper()
	w := &upgradeWorld{versions: map[string]string{"cp": "v1.37.2+k3s1", "w1": "v1.37.2+k3s1"}, waitErr: map[string]error{}}
	origAPI, origInstall, origDrain, origWait, origUncordon, origHealthy := nodeAPI, installK3s, drainNode, waitNodeAt, uncordonNode, controlPlanesHealthy
	nodeAPI = func(string) (nodeClient, error) { return w, nil }
	installK3s = func(ctx context.Context, o k3s.Options) ([]byte, error) {
		w.calls = append(w.calls, "install "+o.Version)
		release := k3s.Release(o.Version)
		for _, n := range o.Nodes {
			if o.Rolling == nil || w.versions[n.Name] == release {
				continue
			}
			if err := o.Rolling(ctx, n, func() error {
				w.calls = append(w.calls, "upgrade "+n.Name)
				w.versions[n.Name] = release
				return nil
			}); err != nil {
				return nil, err
			}
		}
		return []byte(k3sKubeconfig), nil
	}
	drainNode = func(_ context.Context, _, _, node string, _ time.Duration) error {
		w.calls = append(w.calls, "drain "+node)
		return nil
	}
	waitNodeAt = func(_ context.Context, _, node, release string) error {
		w.calls = append(w.calls, "ready "+node+" "+release)
		return w.waitErr[node]
	}
	uncordonNode = func(_ context.Context, _, _, node string) error {
		w.calls = append(w.calls, "uncordon "+node)
		return nil
	}
	controlPlanesHealthy = func(_ context.Context, _ *config.Config, cps []providers.Node, _ string) error {
		var names []string
		for _, n := range cps {
			names = append(names, n.Name)
		}
		w.calls = append(w.calls, "readyz "+strings.Join(names, ","))
		return nil
	}
	// kubeadm: each node upgraded, in turn; containerd moves on every node.
	origRuntimes, origNode, origSpread := runtimeChanges, upgradeKubeadm, spreadCoreDNS
	runtimeChanges = func(_ context.Context, o kubeadm.Options) (map[string]string, error) {
		w.calls = append(w.calls, "runtimes "+o.Version)
		return map[string]string{"cp": "containerd 2.2.1 → 2.3.7", "w1": "containerd 2.2.1 → 2.3.7"}, nil
	}
	upgradeKubeadm = func(_ context.Context, o kubeadm.Options, n providers.Node, first bool) error {
		w.calls = append(w.calls, fmt.Sprintf("kubeadm %s first=%v", n.Name, first))
		w.versions[n.Name] = o.Version
		return nil
	}
	spreadCoreDNS = func(context.Context, string) error {
		w.calls = append(w.calls, "spread coredns")
		return nil
	}
	t.Cleanup(func() {
		nodeAPI, installK3s, drainNode, waitNodeAt, uncordonNode, controlPlanesHealthy = origAPI, origInstall, origDrain, origWait, origUncordon, origHealthy
		runtimeChanges, upgradeKubeadm, spreadCoreDNS = origRuntimes, origNode, origSpread
	})
	return w
}

// kubeadm: the runtime changes are planned, the first control plane runs upgrade apply, then
// the worker; CoreDNS is spread again; the release is recorded.
func TestUpgradeKubeadm(t *testing.T) {
	useFakes(t)
	w := useUpgrade(t)
	useSnapshots(t, "etcd data", false)
	w.versions = map[string]string{"cp": "v1.37.2", "w1": "v1.37.2"}
	kubeadmLab := strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: kubeadm", 1)
	running(t, parse(t, "lab", kubeadmLab))
	file := parse(t, "lab", strings.Replace(kubeadmLab, "distribution: kubeadm", "distribution: kubeadm, version: v1.38.0", 1))
	if err := NewOrchestrator(false).AssumeYes().Apply(context.Background(), file); err != nil {
		t.Fatal(err)
	}
	want := []string{"runtimes v1.38.0", "drain cp", "kubeadm cp first=true", "ready cp v1.38.0", "readyz cp", "uncordon cp",
		"drain w1", "kubeadm w1 first=false", "ready w1 v1.38.0", "uncordon w1", "spread coredns"}
	if !slices.Equal(w.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(w.calls, "\n  "), strings.Join(want, "\n  "))
	}
	if st, _ := state.Load("lab"); st.K8sVersion != "v1.38.0" || len(st.Cordoned) != 0 {
		t.Errorf("state: %s %v", st.K8sVersion, st.Cordoned)
	}
}

// An upgrade: a snapshot first, then the control plane, then the worker, each drained,
// upgraded, back at the release and uncordoned. A run that stops says what's done; the next
// finishes the node it left cordoned, takes no second snapshot, and records the release.
func TestUpgradeK3s(t *testing.T) {
	useFakes(t)
	w := useUpgrade(t)
	took := useSnapshots(t, "etcd data", false)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	file := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: k3s, version: v1.38.0", 1))
	ctx := context.Background()

	if err := NewOrchestrator(true).Apply(ctx, file); err != nil || len(w.calls) != 0 || len(*took) != 0 {
		t.Fatalf("a dry run: %v, %v, snapshots %v", err, w.calls, *took)
	}

	w.waitErr["w1"] = errors.New("w1 isn't Ready at v1.38.0+k3s1")
	err := NewOrchestrator(false).AssumeYes().Apply(ctx, file)
	if err == nil || !strings.Contains(err.Error(), "w1 stays cordoned") || !strings.Contains(err.Error(), "Already on v1.38.0+k3s1, so skipped when you run it again: cp") {
		t.Fatalf("err = %v", err)
	}
	want := []string{"install v1.38.0", "drain cp", "upgrade cp", "ready cp v1.38.0+k3s1", "readyz cp", "uncordon cp", "drain w1", "upgrade w1", "ready w1 v1.38.0+k3s1"}
	if !slices.Equal(w.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(w.calls, "\n  "), strings.Join(want, "\n  "))
	}
	st, _ := state.Load("lab")
	if st.K8sVersion != "v1.37.2" || !slices.Equal(st.Cordoned, []string{"w1"}) {
		t.Errorf("state: release %s, cordoned %v", st.K8sVersion, st.Cordoned)
	}
	if list, _ := ListBackups("lab"); len(list) != 1 || list[0].BeforeUpgradeTo != "v1.38.0" {
		t.Errorf("the snapshot before: %+v", list)
	}

	// The file back to the release it had: cp runs a newer one, which can't go back.
	err = NewOrchestrator(false).AssumeYes().Apply(ctx, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	if err == nil || !strings.Contains(err.Error(), "an upgrade stopped partway, and cp (v1.38.0+k3s1), w1 (v1.38.0+k3s1) already run a newer release than v1.37.2+k3s1") {
		t.Errorf("back: %v", err)
	}

	// Again: w1 is finished first; nothing is left to roll; the release is recorded.
	delete(w.waitErr, "w1")
	w.calls = nil
	if err := NewOrchestrator(false).AssumeYes().Apply(ctx, file); err != nil {
		t.Fatal(err)
	}
	if want := []string{"ready w1 v1.38.0+k3s1", "uncordon w1", "install v1.38.0"}; !slices.Equal(w.calls, want) {
		t.Errorf("resumed: %v, want %v", w.calls, want)
	}
	st, _ = state.Load("lab")
	if st.K8sVersion != "v1.38.0" || st.Spec.Version != "v1.38.0" || len(st.Cordoned) != 0 || st.Config["spec.kubernetes.version"] != "v1.38.0" {
		t.Errorf("state: release %s/%s, cordoned %v, file %q", st.K8sVersion, st.Spec.Version, st.Cordoned, st.Config["spec.kubernetes.version"])
	}
	if list, _ := ListBackups("lab"); len(list) != 1 {
		t.Errorf("a resumed upgrade takes no second snapshot: %d", len(list))
	}

	// Up to date now.
	w.calls = nil
	if err := NewOrchestrator(false).AssumeYes().Apply(ctx, file); err != nil || len(w.calls) != 0 {
		t.Errorf("again: %v, %v", err, w.calls)
	}
}

// A drain that stops leaves its node cordoned and not upgraded: the next run drains and
// upgrades it, rather than waiting for a release it doesn't run.
func TestUpgradeDrainStops(t *testing.T) {
	useFakes(t)
	w := useUpgrade(t)
	useSnapshots(t, "etcd data", false)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	file := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: k3s, version: v1.38.0", 1))
	ctx := context.Background()
	drainNode = func(_ context.Context, _, _, node string, _ time.Duration) error {
		w.calls = append(w.calls, "drain "+node)
		if node == "w1" {
			return errors.New("w1 couldn't be drained within 10m0s")
		}
		return nil
	}
	if err := NewOrchestrator(false).AssumeYes().Apply(ctx, file); err == nil || !strings.Contains(err.Error(), "couldn't be drained") {
		t.Fatalf("err = %v", err)
	}
	if st, _ := state.Load("lab"); !slices.Equal(st.Cordoned, []string{"w1"}) || w.versions["w1"] != "v1.37.2+k3s1" {
		t.Fatalf("cordoned %v, w1 runs %s", st.Cordoned, w.versions["w1"])
	}

	drainNode = func(_ context.Context, _, _, node string, _ time.Duration) error {
		w.calls = append(w.calls, "drain "+node)
		return nil
	}
	w.calls = nil
	if err := NewOrchestrator(false).AssumeYes().Apply(ctx, file); err != nil {
		t.Fatal(err)
	}
	if want := []string{"install v1.38.0", "drain w1", "upgrade w1", "ready w1 v1.38.0+k3s1", "uncordon w1"}; !slices.Equal(w.calls, want) {
		t.Errorf("resumed: %v, want %v", w.calls, want)
	}
	if st, _ := state.Load("lab"); len(st.Cordoned) != 0 || st.K8sVersion != "v1.38.0" {
		t.Errorf("state: cordoned %v, release %s", st.Cordoned, st.K8sVersion)
	}
}

// containerd's pin may change with an upgrade (2.2 doesn't go with 1.37), and only then.
func TestUpgradeMovesTheRuntimePin(t *testing.T) {
	useFakes(t)
	w := useUpgrade(t)
	useSnapshots(t, "etcd data", false)
	w.versions = map[string]string{"cp": "v1.36.6", "w1": "v1.36.6"}
	kubeadmLab := strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: kubeadm, version: v1.36.6, containerd: {version: 2.2.10}", 1)
	running(t, parse(t, "lab", kubeadmLab))
	st, _ := state.Load("lab")
	st.K8sVersion = "v1.36.6"
	state.Save("lab", *st)

	// Without an upgrade: refused.
	samePin := strings.Replace(kubeadmLab, "2.2.10", "2.2.9", 1)
	if err := applyYes(t, parse(t, "lab", samePin)); err == nil || !strings.Contains(err.Error(), "spec.kubernetes.containerd.version: 2.2.10 → 2.2.9: "+whyRuntime) {
		t.Errorf("without an upgrade: %v", err)
	}
	// With one: it moves.
	upgraded := strings.Replace(kubeadmLab, "version: v1.36.6, containerd: {version: 2.2.10}", "version: v1.37.2", 1)
	if err := applyYes(t, parse(t, "lab", upgraded)); err != nil {
		t.Fatal(err)
	}
	if st, _ := state.Load("lab"); st.K8sVersion != "v1.37.2" || st.Config["spec.kubernetes.containerd.version"] != "" {
		t.Errorf("state: %s, containerd pin %q", st.K8sVersion, st.Config["spec.kubernetes.containerd.version"])
	}
}

// With an upgrade, the CNI and the built-in add-ons move to this tow's releases, after
// the nodes; Cilium's first upgrade records the release it was installed at. Never
// backwards, never further than their docs allow in one go.
func TestBuiltinsMoveWithUpgrade(t *testing.T) {
	useFakes(t)
	w := useUpgrade(t)
	useSnapshots(t, "etcd data", false)
	w.versions = map[string]string{"cp": "v1.37.2", "w1": "v1.37.2"}
	running(t, builtinsLab(t, labCilium+", "+labStorage))
	f := useBuiltins(t, &fakeBuiltins{releases: map[string]string{"longhorn-system/longhorn": "1.12.1", "kube-system/csi-driver-nfs": addons.NFSVersion, "kube-system/cilium": "1.19.2"}})
	upgraded := builtinsLab(t, labCilium+", "+labStorage+", version: v1.38.0")
	if err := applyYes(t, upgraded); err != nil {
		t.Fatal(err)
	}
	if want := []string{"cni cilium " + addons.CiliumVersion, "install longhorn"}; !slices.Equal(f.calls, want) {
		t.Errorf("add-ons: %v, want %v", f.calls, want)
	}
	if v := f.installed["longhorn"].Version(); v != addons.LonghornVersion {
		t.Errorf("longhorn at %s", v)
	}
	if st, _ := state.Load("lab"); st.CiliumCompat != "1.19" || st.K8sVersion != "v1.38.0" {
		t.Errorf("state: compat %q, release %s", st.CiliumCompat, st.K8sVersion)
	}

	for _, tc := range []struct{ running, file, want string }{
		{"1.18.4", labCilium, "cilium: 1.18.4 runs, and " + addons.CiliumVersion + " is 2 minor releases on, more than its docs allow in one go (1): pin 1.19"},
		{"1.19.2", `networking: {cni: cilium, cilium: {kubeProxyReplacement: true, version: 1.19.1}}`, "cilium: 1.19.2 runs, and 1.19.1 is older: an add-on isn't downgraded"},
	} {
		os.RemoveAll(state.Dir("lab"))
		running(t, builtinsLab(t, labCilium))
		w.versions = map[string]string{"cp": "v1.37.2", "w1": "v1.37.2"}
		useBuiltins(t, &fakeBuiltins{releases: map[string]string{"kube-system/cilium": tc.running}})
		err := applyYes(t, builtinsLab(t, tc.file+", version: v1.38.0"))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s → %s: %v", tc.running, tc.file, err)
		}
	}
}

// An upgrade changes nothing else, and stops before anything when the release doesn't exist.
func TestUpgradeRefusals(t *testing.T) {
	useFakes(t)
	w := useUpgrade(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	ctx := context.Background()
	both := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ", labels: {tier: web}"), "distribution: k3s", "distribution: k3s, version: v1.38.0", 1))
	if err := NewOrchestrator(false).AssumeYes().Apply(ctx, both); err == nil || !strings.Contains(err.Error(), "an upgrade goes alone") {
		t.Errorf("with a label: %v", err)
	}
	releaseExists = func(_ context.Context, _, release string) error { return fmt.Errorf("%s isn't a release", release) }
	missing := parse(t, "lab", strings.Replace(fmt.Sprintf(lab, "", ""), "distribution: k3s", "distribution: k3s, version: v1.37.99", 1))
	if err := NewOrchestrator(false).AssumeYes().Apply(ctx, missing); err == nil || !strings.Contains(err.Error(), "v1.37.99+k3s1 isn't a release") {
		t.Errorf("missing: %v", err)
	}
	if len(w.calls) != 0 {
		t.Errorf("touched: %v", w.calls)
	}
}

// A newer patch of the running minor is noted; none, or no answer, says nothing.
func TestNewerKubernetes(t *testing.T) {
	useFakes(t)
	newestPatch = func(_ context.Context, engine string, minor int) (string, error) {
		if engine == config.DistributionK3s {
			return fmt.Sprintf("v1.%d.4+k3s1", minor), nil
		}
		return fmt.Sprintf("v1.%d.2", minor), nil
	}
	ctx := context.Background()
	if got := newerKubernetes(ctx, "k3s", "v1.37.2"); got != "Kubernetes v1.37.4+k3s1 is out (v1.37.2+k3s1 runs): set spec.kubernetes.version: v1.37.4+k3s1 and apply to upgrade" {
		t.Errorf("k3s: %q", got)
	}
	if got := newerKubernetes(ctx, "kubeadm", "v1.37.2"); got != "" {
		t.Errorf("up to date: %q", got)
	}
	newestPatch = func(context.Context, string, int) (string, error) { return "", errors.New("offline") }
	if got := newerKubernetes(ctx, "k3s", "v1.37.2"); got != "" {
		t.Errorf("offline: %q", got)
	}
}

// Pods run on the workers, and on the control planes only when they're schedulable.
func TestPodNodes(t *testing.T) {
	nodes := []providers.Node{{Name: "cp", Role: config.RoleControlPlane}, {Name: "w1", Role: config.RoleWorker}}
	if got := podNodes(parse(t, "lab", fmt.Sprintf(lab, "", "")), nodes); !slices.Equal(got, []string{"w1"}) {
		t.Errorf("control planes not schedulable: %v", got)
	}
	if got := podNodes(parse(t, "lab", fmt.Sprintf(lab, ", schedulable: \"true\"", "")), nodes); !slices.Equal(got, []string{"cp", "w1"}) {
		t.Errorf("schedulable: %v", got)
	}
}

func TestMixedReleases(t *testing.T) {
	nodes := []kubeapi.Node{{Name: "cp", Version: "v1.38.0+k3s1"}, {Name: "w1", Version: "v1.37.2+k3s1"}, {Name: "w2", Version: "v1.37.2+k3s1"}}
	if got := mixedReleases(nodes); got != "cp run v1.38.0+k3s1; w1, w2 run v1.37.2+k3s1 (an upgrade that stopped partway: 'tow apply' finishes it)" {
		t.Errorf("%q", got)
	}
	if got := mixedReleases(nodes[1:]); got != "" {
		t.Errorf("one release: %q", got)
	}
}
