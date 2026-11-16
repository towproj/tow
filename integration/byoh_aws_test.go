package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/remote"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// TestBYOHOnAWS checks real machines over a real network, which localhost can't: node-to-node
// traffic over private IPs, and the API port reached from this machine. Throwaway AWS servers
// play the user's own machines. They come from the OpenTofu module, used here only as a
// fixture: provisioning is TestAWSInfra's job, and that test never runs byoh. On them, byoh
// builds k3s as it ships (the homelab flavor), then kubeadm (dev-test), each checked by
// checkCluster and then deleted with --reset: the machines must then be as they were.
//
// Cost: two t3.small for ~30 minutes, about $0.05. Cleanup works as in TestAWSInfra: delete
// always runs and checks for leftovers, the test waits for AWS to report the machines
// terminated (exit code 3 with what to run, if not), and they self-destruct after an hour.
func TestBYOHOnAWS(t *testing.T) {
	if os.Getenv("INTEGRATION_BYOH_AWS") == "" {
		t.Skip("set INTEGRATION_BYOH_AWS=1 to check real AWS servers as your own machines")
	}
	region := envOr("TOW_TEST_REGION", "eu-west-3")
	instanceType := envOr("TOW_TEST_INSTANCE_TYPE", "t3.small")
	stamp := time.Now().Unix()
	fixture := fmt.Sprintf("test-byoh-machines-%d", stamp)

	t.Setenv("TOW_RESOURCE_TTL", "1h")
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig")) // the user's own is never touched
	ui.SetNonInteractive(true)
	ensureTofu(t) // for the fixture's machines

	orch := core.NewOrchestrator(false)
	yes := func(string) bool { return true }
	t.Cleanup(func() {
		if !state.Exists(fixture) {
			return // nothing was created
		}
		// Fresh context: the test's own may already be cancelled (deadline or Ctrl+C).
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		confirmTerminated(t, target{cloud: config.ProviderAWS, region: region}, fixture, orch.Delete(ctx, fixture, yes))
	})

	// Same safety as TestAWSInfra: Ctrl+C cancels instead of killing, so cleanup still runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 50*time.Minute)
	defer cancel()

	// Fixture: one control plane and one worker, sized for kubeadm (dev-test). Its own apply
	// stops once the machines exist: nothing prepared, no Kubernetes on them, as on a user's
	// own machines.
	// TOW_TEST_OS picks the machines' system (spec.infrastructure.os): almalinux-9 runs the
	// same cases on RHEL's tools (dnf, firewalld, SELinux).
	system := envOr("TOW_TEST_OS", config.OSUbuntu2404)
	rhel := config.RHELFamily(system)
	// AlmaLinux keeps memory aside for kdump: a 2 GiB machine shows about 1650 MB, under
	// kubeadm's 1700 MB on the control plane and byoh's 2 GB check for a kubeadm worker. Both
	// get c7i-flex.large (4 GiB, and allowed on AWS's free plan, unlike t3.medium).
	cpType, workerType := instanceType, instanceType
	if rhel && instanceType == "t3.small" {
		cpType, workerType = "c7i-flex.large", "c7i-flex.large"
	}
	machinesCfg, err := config.Parse([]byte(fmt.Sprintf(`
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: %s}
spec:
  flavor: dev-test
  infrastructure: {provider: aws, os: %s, aws: {region: %s}}
  controlPlane: {instanceType: %s}
  workers: [{name: general, count: 1, instanceType: %s}]
`, fixture, system, region, cpType, workerType)))
	if err != nil {
		t.Fatal(err)
	}
	// The byoh cases prepare the machines themselves, as on a user's own; the question is
	// covered by unit tests, and here --yes stands in for the answer.
	if err := core.NewOrchestrator(false).StopAfter(state.StageMachines).Apply(ctx, machinesCfg); err != nil {
		t.Fatalf("create fixture machines: %v", err)
	}
	st, err := state.Load(fixture)
	if err != nil || st.Outputs == nil || len(st.Outputs.Nodes) != 2 {
		t.Fatalf("fixture state: %+v, %v", st, err)
	}
	machines := st.Outputs.Nodes
	orch.AssumeYes()

	// tow refuses to adopt a machine another cluster owns, and the fixture cluster owns
	// these servers. Forget its machine list so byoh may adopt them here, as a user would their
	// own machines. Deleting the fixture doesn't need the list: it uses OpenTofu's state and
	// the cluster tag.
	st.Outputs = nil
	if err := state.Save(fixture, *st); err != nil {
		t.Fatal(err)
	}

	// byoh gets only what a user would write: public addresses, roles and an SSH user. The
	// homelab flavor is k3s as it ships; dev-test is kubeadm. t is the case's: its cleanup runs
	// when the case ends, before the next one uses the machines.
	byoh := func(t *testing.T, name, flavor string) *config.Config {
		var cps, workers []string
		for _, n := range machines {
			if n.Role == config.RoleControlPlane {
				cps = append(cps, "{host: "+n.Host+"}")
			} else {
				workers = append(workers, "{host: "+n.Host+"}")
			}
		}
		cfg, err := config.Parse([]byte(fmt.Sprintf(`
apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: %s}
spec:
  flavor: %s
  infrastructure: {provider: byoh, access: {ssh: {user: %s}}}
  controlPlane: {machines: [%s]}
  workers: [{machines: [%s]}]
`, name, flavor, machines[0].User, strings.Join(cps, ", "), strings.Join(workers, ", "))))
		if err != nil {
			t.Fatal(err)
		}
		// After a failure: deleted with --reset so the next case finds the machines clean, or
		// else forgotten (the machines go with the fixture). A failed apply leaves just pinned
		// host keys.
		t.Cleanup(func() {
			if state.Exists(name) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				if err := core.NewOrchestrator(false).WithReset().WithVolumes().Delete(ctx, name, yes); err != nil {
					t.Logf("cleanup: delete --reset: %v", err)
					core.NewOrchestrator(false).Forget().Delete(ctx, name, yes)
				}
			}
			os.RemoveAll(state.Dir(name))
		})
		return cfg
	}

	// onMachine runs a script as root on a fixture machine.
	onMachine := func(t *testing.T, n providers.Node, script string) string {
		t.Helper()
		c, err := remote.Dial(ctx, remote.Target{Host: n.Host, Port: n.Port, User: n.User, KeyPath: n.KeyPath}, filepath.Join(state.Dir(fixture), "known_hosts"))
		if err != nil {
			t.Fatalf("%s: %v", n.Host, err)
		}
		defer c.Close()
		out, err := c.RunInput(ctx, "sudo -n sh -s", script)
		if err != nil {
			t.Fatalf("%s: %v\n%s", n.Host, err, out)
		}
		return out
	}

	// First, while nothing listens on the machines and their firewall is off.
	t.Run("blocked node-to-node port is caught", func(t *testing.T) {
		var cp, worker providers.Node
		for _, n := range machines {
			if n.Role == config.RoleControlPlane {
				cp = n
			} else {
				worker = n
			}
		}
		// The worker drops the control plane's traffic to its kubelet, as a misconfigured
		// firewall would. Only this throwaway machine is changed.
		c, err := remote.Dial(ctx, remote.Target{Host: worker.Host, Port: worker.Port, User: worker.User, KeyPath: worker.KeyPath},
			filepath.Join(state.Dir(fixture), "known_hosts"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		if rhel { // RHEL's cloud images may come without the iptables command
			onMachine(t, worker, "command -v iptables >/dev/null || dnf install -y -q iptables-nft >/dev/null\n")
		}
		rule := fmt.Sprintf("INPUT -p tcp -s %s --dport 10250 -j DROP", cp.PrivateIP)
		if out, err := c.Run(ctx, "sudo -n iptables -I "+rule); err != nil {
			t.Fatalf("add firewall rule: %v\n%s", err, out)
		}
		// Taken out again for the next case, which needs the port.
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if out, err := c.Run(ctx, "sudo -n iptables -D "+rule); err != nil {
				t.Errorf("remove firewall rule: %v\n%s", err, out)
			}
		}()

		err = orch.Apply(ctx, byoh(t, fmt.Sprintf("test-byoh-blocked-%d", stamp), "homelab"))
		if err == nil {
			t.Fatal("byoh apply passed although the worker drops port 10250")
		}
		msg := err.Error()
		blocked := fmt.Sprintf("(%s) port 10250", worker.PrivateIP)
		if !strings.Contains(msg, "node-to-node: can't reach") || !strings.Contains(msg, blocked) {
			t.Errorf("want a node-to-node failure for %s:\n%v", blocked, msg)
		}
		// Only the dropped port fails: the worker still reaches the API server.
		if strings.Contains(msg, fmt.Sprintf("(%s) port 6443", cp.PrivateIP)) {
			t.Errorf("port 6443 should still be reachable:\n%v", msg)
		}
	})

	// The system's firewall on, as on many servers (ufw; firewalld on RHEL): preparation lets
	// the cluster through, and --reset must take only that away, and leave the firewall on.
	// RHEL's SELinux enforces as it ships: kubeadm's preparation sets it permissive, --reset
	// enforcing again.
	firewallOn := "ufw allow 22/tcp >/dev/null\nufw --force enable >/dev/null\n"
	if rhel {
		firewallOn = "dnf install -y -q firewalld >/dev/null\nsystemctl enable --now firewalld >/dev/null 2>&1\n"
	}
	enforcing := map[string]bool{}
	for _, n := range machines {
		onMachine(t, n, firewallOn)
		enforcing[n.Host] = strings.TrimSpace(onMachine(t, n, "getenforce 2>/dev/null || true\n")) == "Enforcing"
	}

	// k3s, then kubeadm on the machines k3s's delete handed back: each is checked, then deleted
	// with --reset, after which nothing tow added may be left. Swap is on before kubeadm,
	// whose preparation turns it off: --reset must turn it on again.
	swapOn := "fallocate -l 256M /swapfile\nchmod 600 /swapfile\nmkswap /swapfile >/dev/null\nswapon /swapfile\necho '/swapfile none swap sw 0 0' >> /etc/fstab\n"
	for _, tc := range []struct{ distribution, flavor, before, recorded string }{
		{"k3s", "homelab", "", "firewall"},
		{"kubeadm", "dev-test", swapOn, "swap"},
	} {
		t.Run(tc.distribution+", then delete --reset", func(t *testing.T) {
			if tc.before != "" {
				for _, n := range machines {
					onMachine(t, n, tc.before)
				}
			}
			name := fmt.Sprintf("test-byoh-%s-%d", tc.distribution, stamp)
			cfg := byoh(t, name, tc.flavor)
			ensureTools(t, cfg.Spec.Kubernetes.Release()) // kubeadm's CNI and charts need helm and kubectl
			if err := orch.Apply(ctx, cfg); err != nil {
				t.Fatalf("byoh apply: %v", err)
			}
			got, err := state.Load(name)
			if err != nil || got.Outputs == nil || len(got.Outputs.Nodes) != len(machines) {
				t.Fatalf("byoh state: %+v, %v", got, err)
			}
			// byoh found on its own the private IPs OpenTofu reported, and a node name for each.
			for i, n := range got.Outputs.Nodes {
				if want := machines[i]; n.PrivateIP != want.PrivateIP || n.Name == "" {
					t.Errorf("node %s: found name %q, ip %q; want ip %q", n.Host, n.Name, n.PrivateIP, want.PrivateIP)
				}
			}

			// The cluster is what the file asks for, and works from this machine: the API on the
			// control plane's public address, a NodePort Service on it too; status agrees.
			present, absent := systemPods(cfg)
			checkCluster(t, ctx, clusterCheck{cfg: cfg, st: got, present: present, absent: absent, nodePortHost: machines[0].Host})

			// What each machine's record says tow changed there, before it's undone.
			packages := map[string][]string{}
			for _, n := range machines {
				var l struct {
					Changes []prepare.Record `json:"changes"`
				}
				out := onMachine(t, n, "cat "+prepare.RecordFile+"\n")
				if err := json.Unmarshal([]byte(out), &l); err != nil {
					t.Fatalf("%s: %s: %v\n%s", n.Host, prepare.RecordFile, err, out)
				}
				var whats []string
				for _, r := range l.Changes {
					whats = append(whats, r.What)
					if r.What == "packages" {
						packages[n.Host] = append(packages[n.Host], strings.Fields(strings.TrimPrefix(r.Detail, "installed "))...)
					}
				}
				if !slices.ContainsFunc(l.Changes, func(r prepare.Record) bool { return r.Kubernetes && r.Revert != "" }) || !slices.Contains(whats, tc.recorded) {
					t.Errorf("%s: recorded %v, want Kubernetes and %s", n.Host, whats, tc.recorded)
				}
			}

			// -v: the storage check's local-path volumes may still be on their way out.
			if err := core.NewOrchestrator(false).WithReset().WithVolumes().Delete(ctx, name, yes); err != nil {
				t.Fatalf("delete --reset: %v", err)
			}
			if ok, _ := kubecfg.HasContext(name); ok {
				t.Error("delete left the kubeconfig context behind")
			}
			for _, n := range machines {
				if left := strings.TrimSpace(onMachine(t, n, leftovers(packages[n.Host], tc.before != "", rhel, enforcing[n.Host]))); left != "" {
					t.Errorf("%s after delete --reset:\n%s", n.Host, left)
				}
			}
		})
	}
}

// leftovers prints what tow added that's still on a machine after delete --reset: nothing,
// when the reset worked. swap: the machine had swap on before. rhel: a RHEL-family machine
// (dnf, firewalld); enforcing: its SELinux enforced before.
func leftovers(packages []string, swap, rhel, enforcing bool) string {
	s := `for f in /usr/local/bin/k3s /usr/local/bin/kubectl /usr/local/bin/crictl /usr/bin/kubeadm /usr/bin/kubelet /usr/bin/kubectl \
  /usr/local/bin/containerd /usr/local/bin/ctr /usr/local/sbin/runc /etc/containerd /etc/rancher /var/lib/rancher /etc/cni/net.d \
  /etc/apt/sources.list.d/kubernetes.list /etc/yum.repos.d/kubernetes.repo /etc/sysctl.d/k8s.conf /etc/modules-load.d/k8s.conf /etc/tow; do
  [ ! -e "$f" ] || echo "left: $f"
done
for u in k3s k3s-agent kubelet containerd; do systemctl is-active --quiet "$u" && echo "still running: $u"; done
`
	if rhel {
		s += `firewall-cmd -q --state || echo "firewalld isn't on anymore"
firewall-cmd --permanent --get-zones | tr ' ' '\n' | grep -x tow-nodes | sed 's/^/firewalld zone left: /'
firewall-cmd --permanent --get-policies | tr ' ' '\n' | grep -x tow-admin | sed 's/^/firewalld policy left: /'
`
		if enforcing {
			s += `[ "$(getenforce)" = Enforcing ] || echo "SELinux doesn't enforce anymore"
grep -qx 'SELINUX=enforcing' /etc/selinux/config || echo "SELinux won't enforce at boot anymore"
`
		}
		for _, p := range packages {
			s += "rpm -q --quiet " + p + " && echo 'package left: " + p + "'\n"
		}
	} else {
		s += `ufw status | grep -q '^Status: active' || echo "ufw isn't on anymore"
ufw status | grep 'tow:' | sed 's/^/ufw rule left: /'
`
		for _, p := range packages {
			s += "dpkg-query -W -f='${Status}' " + p + " 2>/dev/null | grep -q 'ok installed' && echo 'package left: " + p + "'\n"
		}
	}
	if swap {
		s += "swapon --show=NAME --noheadings | grep -qx /swapfile || echo 'swap is off'\n" +
			"grep -q '^/swapfile ' /etc/fstab || echo 'the swap line of /etc/fstab is still commented out'\n"
	}
	return s + "true\n"
}
