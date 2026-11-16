package integration

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/kubevip"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/proxmox"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/clientcmd"
)

// TestProxmoxInfra creates real VMs on a Proxmox VE server through 'apply', proves they're
// usable as cluster nodes (SSH, sudo, internet, node-to-node traffic), then deletes them and
// confirms no VM tagged for the cluster is left. Setup: docs/testing/proxmox.md.
//
// The cluster comes from TOW_TEST_PROXMOX_CONFIG (default examples/dev-test-proxmox.yaml,
// which matches the lab in the doc) under a throwaway name, with TOW_TEST_KUBERNETES
// merged into its spec.kubernetes when set. Every cluster gets checkCluster. Cleanup: the test always runs
// 'delete' (even after a failure, Ctrl+C or its own deadline), and delete checks for leftovers.
// VMs carry a tow-expires tag, but nothing on Proxmox deletes them by itself.
func TestProxmoxInfra(t *testing.T) {
	if os.Getenv("INTEGRATION_PROXMOX") == "" {
		t.Skip("set INTEGRATION_PROXMOX=1 to create real VMs on your Proxmox server")
	}
	if os.Getenv("PROXMOX_VE_API_TOKEN") == "" {
		t.Fatal("PROXMOX_VE_API_TOKEN is not set; see docs/testing/proxmox.md")
	}
	file := envOr("TOW_TEST_PROXMOX_CONFIG", "../examples/dev-test-proxmox.yaml")

	t.Setenv("TOW_RESOURCE_TTL", "1h")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig")) // the user's own is never touched
	ui.SetNonInteractive(true)
	cfg := loadWithKubernetes(t, file)
	labSecrets(t, cfg)
	if cfg.Provider() != config.ProviderProxmox {
		t.Fatalf("%s must use provider proxmox", file)
	}
	ensureTools(t, cfg.Spec.Kubernetes.Release()) // the CNIs are installed with helm (and kubectl)
	ensureTofu(t)
	name := testName("test-pve")
	cfg.Metadata.Name = name
	cfg.Spec.Kubernetes.Kubeconfig.Context = name
	want := cfg.MachineCount()

	orch := core.NewOrchestrator(false).WithVolumes() // a test cluster's volumes never outlive it
	yes := func(string) bool { return true }
	deleted := false
	t.Cleanup(func() {
		if deleted || !state.Exists(name) {
			return
		}
		// Fresh context: the test's own may already be cancelled (deadline or Ctrl+C).
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if err := orch.Delete(ctx, name, yes); err != nil {
			t.Errorf("cleanup failed; delete the VMs tagged tow-cluster.%s in the Proxmox web UI: %v", name, err)
		}
	})

	// Ctrl+C cancels the work instead of killing the process, so cleanup still runs. The
	// deadline stays well under 'go test -timeout', whose panic would skip cleanup.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	deadline := 35 * time.Minute // room for three kubeadm VMs and a failover
	if conformance() {
		deadline += 4 * time.Hour
	}
	ctx, cancel := context.WithTimeout(ctx, deadline)
	defer cancel()

	if err := orch.Apply(ctx, cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	st, err := state.Load(name)
	if err != nil || st.Outputs == nil || len(st.Outputs.Nodes) != want {
		t.Fatalf("state after apply: %+v, %v (want %d nodes)", st, err, want)
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

			systemOf(t, run) // the template's: Ubuntu, Rocky, ...
			// Exit code 2 is "done with recoverable errors", which tow accepts (waitReady)
			// and Proxmox's generated cloud-init config triggers; show them rather than fail.
			if out, err := c.Run(ctx, "cloud-init status --long"); err != nil && remote.ExitCode(err) != 2 {
				t.Errorf("cloud-init did not finish: %v\n%s", err, out)
			} else if !strings.Contains(out, "status: done") {
				t.Errorf("cloud-init status:\n%s", out)
			} else if err != nil {
				t.Logf("cloud-init finished with recoverable errors:\n%s", strings.TrimSpace(out))
			}
			run("passwordless sudo", "sudo -n true")
			if h := run("hostname", "hostname"); h != n.Name {
				t.Errorf("hostname = %q, want %q", h, n.Name)
			}

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

			// Prepared for the file's distribution, as the docs ask, and recorded; with kubeadm,
			// the container runtime and the Kubernetes packages too.
			checkPrepared(t, ctx, c, name, cfg.Distribution())
			if cfg.Distribution() == config.DistributionKubeadm {
				checkKubeadmMachine(t, ctx, c, cfg)
			}

			// Every other node, on its fixed address: ICMP and TCP.
			for _, o := range nodes {
				if o.Name == n.Name {
					continue
				}
				run("ping "+o.Name, "ping -c 3 -W 2 "+o.PrivateIP)
				run("tcp to "+o.Name, tcpProbe(o.PrivateIP, "22"))
			}
		})
	}

	// Kubernetes runs on the VMs: the cluster is what the file asks for, and works from here.
	present, absent := systemPods(cfg)
	check := clusterCheck{cfg: cfg, st: st, present: present, absent: absent, nodePortHost: nodes[0].Host}
	checkCluster(t, ctx, check)
	if conformance() {
		checkConformance(t, ctx, st)
	}

	// k3s with several control planes: CoreDNS keeps two replicas, also once k3s (restarted on
	// the last control plane) applied its own CoreDNS manifest again.
	if cfg.Distribution() == config.DistributionK3s && cfg.ControlPlaneCount() > 1 {
		last := nodes[cfg.ControlPlaneCount()-1] // control planes come first
		checkK3sCoreDNS(t, ctx, check, func(ctx context.Context) error {
			c, err := remote.Dial(ctx, remote.Target{Host: last.Host, Port: last.Port, User: last.User, KeyPath: last.KeyPath}, knownHosts)
			if err != nil {
				return err
			}
			defer c.Close()
			out, err := c.Run(ctx, "sudo -n systemctl restart k3s")
			if err != nil {
				return fmt.Errorf("%s: %w\n%s", last.Name, err, out)
			}
			return nil
		})
	}

	// Several control planes behind kube-vip: the one holding the virtual IP goes away, and
	// another must take the address over.
	if k := cfg.Spec.Kubernetes; k.APILoadBalancer == config.APILBKubeVIP && cfg.ControlPlaneCount() > 1 {
		checkKubeVIPFailover(t, ctx, check, nodes, &providers.MachineRequest{URL: cfg.Spec.Infrastructure.Proxmox.URL})
	}

	// A Proxmox disk in use: delete without -v refuses; the delete below (-v) removes it.
	keepCloudVolume(t, ctx, check, func() error { return core.NewOrchestrator(false).Delete(ctx, name, yes) })

	// Delete fails by itself if Proxmox still lists a VM tagged for the cluster.
	if err := orch.Delete(ctx, name, yes); err != nil {
		t.Fatalf("delete: %v", err)
	}
	deleted = true
	if state.Exists(name) {
		t.Error("state still present after delete")
	}
	if ok, _ := kubecfg.HasContext(name); ok {
		t.Error("delete left the kubeconfig context behind")
	}
}

// checkKubeVIPFailover proves kube-vip serves the API: the kubeconfig uses the virtual IP,
// one control plane holds it (kube-vip's Lease), and when that one is powered off another
// takes the address over while the cluster keeps working (checkFailover).
func checkKubeVIPFailover(t *testing.T, ctx context.Context, c clusterCheck, nodes []providers.Node, m *providers.MachineRequest) {
	t.Helper()
	want := "https://" + c.cfg.Spec.Kubernetes.APIEndpoint
	kc, err := clientcmd.LoadFromFile(c.st.Outputs.Kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	for name, cl := range kc.Clusters {
		if cl.Server != want {
			t.Errorf("kubeconfig cluster %s points at %s, want the virtual IP %s", name, cl.Server, want)
		}
	}

	cs := clientset(t, c.st)
	holder := func() string {
		l, err := cs.CoordinationV1().Leases("kube-system").Get(ctx, kubevip.LeaseName, metav1.GetOptions{})
		if err != nil || l.Spec.HolderIdentity == nil {
			return ""
		}
		return *l.Spec.HolderIdentity
	}
	var victim string
	eventually(t, ctx, "a control plane holding the virtual IP (Lease kube-system/"+kubevip.LeaseName+")", func() bool {
		victim = holder()
		return victim != ""
	})
	t.Logf("%s holds %s", victim, c.cfg.Spec.Kubernetes.APIEndpoint)
	for _, n := range nodes {
		if n.Name != victim {
			c.nodePortHost = n.Host // the NodePort is tried on a machine that stays up
			break
		}
	}

	cloud := proxmox.Cloud{}
	off := func(ctx context.Context) error { return cloud.PowerOff(ctx, m, c.st.Name, victim) }
	on := func(ctx context.Context) error { return cloud.PowerOn(ctx, m, c.st.Name, victim) }
	checkFailover(t, ctx, c, victim, off, on)
	if now := holder(); now == "" || now == victim {
		t.Errorf("the virtual IP's holder is %q; another control plane should have taken it from %s", now, victim)
	} else {
		t.Logf("%s took %s over", now, c.cfg.Spec.Kubernetes.APIEndpoint)
	}
}
