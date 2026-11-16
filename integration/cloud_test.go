package integration

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/aws"
	"github.com/towproj/tow/internal/providers/tofu/azure"
	"github.com/towproj/tow/internal/providers/tofu/gcp"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// cloudRun is one real cluster test on a public cloud (AWS, GCP, Azure).
type cloudRun struct {
	where         target
	prefix        string // the cluster's name starts with it
	controlPlanes int
	workers       int           // in the small dev-test cluster
	schedulable   bool          // the control planes run workloads (GCP's HA test has no worker)
	deadline      time.Duration // stays well under 'go test -timeout', whose panic would skip cleanup
	kubeadmOnly   bool          // k3s on AWS machines is tested by make test-byoh-aws
	failover      bool          // take the first control plane away once the cluster works
	configEnv     string        // the variable naming a cluster file to build instead
}

// runCloud builds a real cluster through 'apply' (the dev-test flavor unless the run's config
// variable names a file), proves the machines are usable as cluster nodes (SSH, sudo, internet,
// node-to-node traffic) and set up as the distribution asks, proves the cluster is what the
// file asks for and works from this machine (checkCluster), optionally takes the first control
// plane away, then deletes it all and confirms nothing is left.
//
// Deletion is guaranteed in layers: the test always runs 'delete' (even after a failure, Ctrl+C
// or its own deadline), delete proves nothing tagged is left, the test then waits for the
// cloud to report every machine gone (exit code 3 with what to run, if not), and the machines
// go away by themselves after TOW_RESOURCE_TTL if this process dies before cleaning up.
func runCloud(t *testing.T, r cloudRun) {
	region, where := r.where.region, r.where
	onGCP, onAzure := where.cloud == config.ProviderGCP, where.cloud == config.ProviderAzure
	defaultSize := map[string]string{config.ProviderAWS: "t3.small", config.ProviderGCP: "e2-medium", config.ProviderAzure: "Standard_B2s_v2"}[where.cloud]
	instanceType := envOr("TOW_TEST_INSTANCE_TYPE", defaultSize)
	name := testName(r.prefix)

	t.Setenv("TOW_RESOURCE_TTL", "1h")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig")) // the user's own is never touched
	ui.SetNonInteractive(true)

	// The cluster: the run's config variable's file when set (not for failover), under the
	// throwaway name, its region (and project) from the file; else a small dev-test one.
	var cfg *config.Config
	if path := os.Getenv(r.configEnv); path != "" && !r.failover {
		cfg = loadWithKubernetes(t, path)
		if cfg.Provider() != where.cloud {
			t.Fatalf("%s must use provider %s", path, where.cloud)
		}
		cfg.Metadata.Name, cfg.Spec.Kubernetes.Kubeconfig.Context = name, name
		region = cfg.Location()
		switch {
		case onGCP:
			where.project = cfg.Spec.Infrastructure.GCP.Project
		case onAzure:
			where.subscription = cfg.Spec.Infrastructure.Azure.Subscription
		}
		where.region = region
	} else {
		infra := fmt.Sprintf("{provider: aws, aws: {region: %s}}", region)
		switch {
		case onGCP:
			infra = fmt.Sprintf("{provider: gcp, gcp: {project: %s, region: %s}}", where.project, region)
		case onAzure:
			infra = fmt.Sprintf("{provider: azure, azure: {subscription: %s, location: %s}}", where.subscription, region)
		}
		if system := os.Getenv("TOW_TEST_OS"); system != "" {
			infra = strings.Replace(infra, "{provider:", "{os: "+system+", provider:", 1)
		}
		workers := fmt.Sprintf("[{name: general, count: %d, instanceType: %s}]", r.workers, instanceType)
		if r.workers == 0 {
			workers = "[]"
		}
		file := []byte(fmt.Sprintf(`
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: %s}
spec:
  flavor: dev-test
  infrastructure: %s
  controlPlane: {count: %d, instanceType: %s, schedulable: %t}
  workers: %s
`, name, infra, r.controlPlanes, instanceType, r.schedulable, workers))
		if extra := os.Getenv("TOW_TEST_KUBERNETES"); extra != "" {
			file = withKubernetes(t, file, extra)
		}
		var err error
		if cfg, err = config.Parse(file); err != nil {
			t.Fatal(err)
		}
	}
	labSecrets(t, cfg)
	if r.kubeadmOnly && cfg.Distribution() != config.DistributionKubeadm {
		t.Fatal("make test-aws tests kubeadm; k3s on AWS machines is make test-byoh-aws")
	}
	ensureTools(t, cfg.Spec.Kubernetes.Release()) // the CNI is installed with helm (and kubectl)
	ensureTofu(t)

	orch := core.NewOrchestrator(false).WithVolumes() // a test cluster's volumes never outlive it
	yes := func(string) bool { return true }
	deleted := false
	var deleteErr error
	t.Cleanup(func() {
		if !deleted {
			if !state.Exists(name) {
				return // nothing was created
			}
			// Fresh context: the test's own may already be cancelled (deadline or Ctrl+C).
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
			defer cancel()
			deleteErr = orch.Delete(ctx, name, yes)
		}
		confirmTerminated(t, where, name, deleteErr)
	})

	// Ctrl+C cancels the work instead of killing the process, so cleanup still runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, r.deadline)
	defer cancel()

	if err := orch.Apply(ctx, cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	st, err := state.Load(name)
	if err != nil || st.Outputs == nil || len(st.Outputs.Nodes) != cfg.MachineCount() {
		t.Fatalf("state after apply: %+v, %v", st, err)
	}
	nodes := st.Outputs.Nodes
	knownHosts := filepath.Join(state.Dir(name), "known_hosts")

	for _, n := range nodes {
		t.Run(n.Name, func(t *testing.T) {
			c, err := remote.Dial(ctx, remote.Target{Host: n.Host, Port: n.Port, User: n.User, KeyPath: n.KeyPath}, knownHosts)
			if err != nil {
				t.Fatalf("ssh: %v", err)
			}
			defer c.Close()

			run := func(what, cmd string) string {
				t.Helper()
				out, err := c.Run(ctx, cmd)
				if err != nil {
					t.Errorf("%s (%s): %v\n%s", what, cmd, err, out)
				}
				return strings.TrimSpace(out)
			}

			// The file's system: ubuntu-24.04 → ubuntu, almalinux-9 → almalinux.
			if id, want := systemOf(t, run), strings.SplitN(cfg.Spec.Infrastructure.OS, "-", 2)[0]; id != want {
				t.Errorf("os = %q, want %s", id, want)
			}
			// GCE's AlmaLinux images have no cloud-init: Google's guest agent sets them up.
			run("boot finished", "if command -v cloud-init >/dev/null; then cloud-init status; fi")
			run("passwordless sudo", "sudo -n true")

			// Big enough for kubeadm's preflight: 2 CPUs and ~2 GB of memory.
			if cpus, _ := strconv.Atoi(run("cpus", "nproc")); cpus < 2 {
				t.Errorf("nproc = %d, kubeadm needs 2", cpus)
			}
			// kubeadm's preflight checks memory on control planes only.
			if kb, _ := strconv.Atoi(run("memory", "awk '/MemTotal/ {print $2}' /proc/meminfo")); n.Role == config.RoleControlPlane && kb < 1700*1024 {
				t.Errorf("memory = %d kB, a kubeadm control plane needs about 1700 MB", kb)
			}

			// Internet: DNS, HTTPS out, and the package mirrors Phase 2 installs from.
			run("dns", "getent hosts pkgs.k8s.io")
			run("https egress", "curl -fsS --max-time 15 -o /dev/null https://checkip.amazonaws.com")
			run("package mirrors", packageMirrors)

			if onGCP || onAzure {
				// The metadata server answers only requests that say they mean it (the header),
				// which a forwarded request from a pod can't smuggle in: GCP says 403, Azure 400.
				url, refused := "http://metadata.google.internal/computeMetadata/v1/", "403"
				if onAzure {
					url, refused = "'http://169.254.169.254/metadata/instance?api-version=2021-02-01'", "400"
				}
				if code := run("metadata", "curl -s -o /dev/null -w '%{http_code}' --max-time 5 "+url); code != refused {
					t.Errorf("the metadata server answered %s without its header, want %s", code, refused)
				}
				// The API name, on an HA cluster: a worker resolves it to the internal load
				// balancer, a control plane to itself.
				if st.Outputs.NodeAPIName != "" {
					want := st.Outputs.NodeAPIAddress()
					if n.Role == config.RoleControlPlane {
						want = n.PrivateIP
					}
					if got := run("API name", "getent hosts "+st.Outputs.NodeAPIName+" | awk '{print $1}'"); got != want {
						t.Errorf("%s resolves to %q, want %s", st.Outputs.NodeAPIName, got, want)
					}
				}
			} else {
				// IMDSv2 only: a request without a session token must be refused.
				if code := run("imds", "curl -s -o /dev/null -w '%{http_code}' --max-time 5 http://169.254.169.254/latest/meta-data/"); code != "401" {
					t.Errorf("IMDSv1 answered with %s, want 401 (tokens required)", code)
				}
				// Safety net is armed: a shutdown is scheduled for the TTL. (On GCP Compute
				// Engine deletes the machine itself, from its max run duration; on Azure its
				// auto-shutdown schedule deallocates it.)
				run("self-destruct scheduled", "test -f /run/systemd/shutdown/scheduled")
			}

			// Prepared as the docs ask, and recorded; with kubeadm, the container runtime and
			// the Kubernetes packages too.
			checkPrepared(t, ctx, c, name, cfg.Distribution())
			if cfg.Distribution() == config.DistributionKubeadm {
				checkKubeadmMachine(t, ctx, c, cfg)
			}

			// Every other node over private IPs: ICMP and TCP.
			for _, o := range nodes {
				if o.Name == n.Name {
					continue
				}
				run("ping "+o.Name, "ping -c 3 -W 2 "+o.PrivateIP)
				run("tcp to "+o.Name, tcpProbe(o.PrivateIP, "22"))
			}
		})
	}

	// The cluster is what the file asks for, and works from this machine: the API (through
	// the load balancer, with several control planes) and a NodePort on a machine's public
	// address, never the one taken away.
	nodePortHost := controlPlaneHost(nodes)
	if r.failover {
		nodePortHost = workerHost(nodes)
		if nodePortHost == "" {
			nodePortHost = nodes[1].Host // control planes only: the second one stays
		}
	}
	present, absent := systemPods(cfg)
	check := clusterCheck{cfg: cfg, st: st, present: present, absent: absent, nodePortHost: nodePortHost}
	checkCluster(t, ctx, check)

	// Machines taken away and back, by name.
	off := func(ctx context.Context, machine string) error {
		switch {
		case onGCP:
			return gcp.Cloud{}.PowerOff(ctx, where.project, name, machine)
		case onAzure:
			return azure.Cloud{}.PowerOff(ctx, where.subscription, name, machine)
		}
		return aws.Cloud{}.PowerOff(ctx, region, name, machine)
	}
	on := func(ctx context.Context, machine string) error {
		switch {
		case onGCP:
			// Compute Engine keeps the machine's max run duration: nothing to arm again.
			_, err := gcp.Cloud{}.PowerOn(ctx, where.project, name, machine)
			return err
		case onAzure:
			// The auto-shutdown schedule stays; the address is static.
			_, err := azure.Cloud{}.PowerOn(ctx, where.subscription, name, machine)
			return err
		}
		ip, err := aws.Cloud{}.PowerOn(ctx, region, name, machine)
		if err != nil {
			return err
		}
		// Its self-destruct was armed at first boot only: armed again, at the new public
		// address AWS gave it (the cluster itself only uses its private one).
		i := slices.IndexFunc(nodes, func(n providers.Node) bool { return n.Name == machine })
		if i < 0 {
			return fmt.Errorf("no machine %s", machine)
		}
		n := nodes[i]
		c, err := remote.WaitDial(ctx, remote.Target{Host: ip, Port: n.Port, User: n.User, KeyPath: n.KeyPath}, knownHosts)
		if err != nil {
			return err
		}
		defer c.Close()
		_, err = c.Run(ctx, "sudo -n shutdown -P +60 'tow test TTL reached' 2>/dev/null")
		return err
	}

	if r.failover {
		victim := nodes[0].Name // the first control plane: kubeadm init ran there, etcd's first member
		checkFailover(t, ctx, check, victim, func(ctx context.Context) error { return off(ctx, victim) }, func(ctx context.Context) error { return on(ctx, victim) })
	}

	// CloudNativePG (examples/cnpg-aws.yaml): the primary's machine, so its zone, goes away; a
	// replica in another zone takes over, and what was written is still there.
	if usesChart(cfg, "cloudnative-pg") {
		checkPostgres(t, ctx, check, off, on)
	}

	// A cloud volume in use: delete without -v refuses; the delete below (-v) removes it, and
	// the leftover check (volumes labeled with the cluster) proves it's gone.
	keepCloudVolume(t, ctx, check, func() error { return core.NewOrchestrator(false).Delete(ctx, name, yes) })

	// Delete fails by itself if the cloud still lists anything of the cluster's. Either way the
	// cleanup then waits for the cloud to report the machines gone.
	deleteErr, deleted = orch.Delete(ctx, name, yes), true
	if deleteErr == nil && state.Exists(name) {
		t.Error("state still present after delete")
	}
}

// controlPlaneHost is the first control plane's address, as this machine reaches it.
func controlPlaneHost(nodes []providers.Node) string {
	for _, n := range nodes {
		if n.Role == config.RoleControlPlane {
			return n.Host
		}
	}
	return ""
}

// workerHost is the first worker's address, as this machine reaches it; "" without workers.
func workerHost(nodes []providers.Node) string {
	for _, n := range nodes {
		if n.Role != config.RoleControlPlane {
			return n.Host
		}
	}
	return ""
}

// testName is a throwaway cluster's name: the prefix, the time, and a random part, so runs
// started in the same second never share a cluster (one's cleanup would delete the other's).
func testName(prefix string) string {
	return fmt.Sprintf("%s-%d-%04x", prefix, time.Now().Unix(), rand.IntN(1<<16))
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
