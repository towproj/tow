package core

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeadm"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// Swappable so tests run without machines.
var (
	reconfigureControlPlane = kubeadm.ReconfigureControlPlane
	kubeletFlags            = kubeadm.KubeletFlags
)

// kubeadmChange is what changes on one kubeadm node.
type kubeadmChange struct {
	n                providers.Node
	cp               kubeadm.Reconfigure // a control plane's static pods, certificate, kube-vip
	removed, added   []string            // its kubelet's flags, as kubeadm-flags.env writes them
	kubeletConfig    bool                // the cluster's KubeletConfiguration changed: pulled again
	what             []string            // for the plan
	controlPlaneWork bool
}

// planKubeadmSettings plans kubeadm's new settings as its reconfigure page says, one machine
// at a time, control planes first, nothing drained: the cluster's stored configuration
// (on the first control plane), each control plane's static pods written again for the
// components whose settings changed (a new API server certificate for new certSANs,
// kube-vip's static pod for its Services), each kubelet's flags and configuration, then the
// node checked before the next.
func (o *Orchestrator) planKubeadmSettings(ctx context.Context, plan *updatePlan, cfg, old *config.Config, prev *state.ClusterState) error {
	was, is := runningKubeadm(old, prev), runningKubeadm(cfg, prev)
	var components []string
	for _, c := range []struct {
		name     string
		old, new []providers.Flag
	}{{"apiserver", was.APIServer, is.APIServer}, {"controller-manager", was.ControllerManager, is.ControllerManager},
		{"scheduler", was.Scheduler, is.Scheduler}, {"etcd", was.Etcd, is.Etcd}} {
		if !slices.Equal(rendered(c.old), rendered(c.new)) {
			components = append(components, c.name)
		}
	}
	sans := !slices.Equal(sortedCopy(old.Spec.Kubernetes.APIServer.CertSANs), sortedCopy(cfg.Spec.Kubernetes.APIServer.CertSANs))
	kubeVIP := is.KubeVIP != "" && was.KubeVIPServices != is.KubeVIPServices
	kubeletConfig := was.ServerTLSBootstrap != is.ServerTLSBootstrap
	cpWork := len(components) > 0 || sans || kubeVIP || kubeletConfig

	// Secrets, read now: kubelet flags carry their values in kubeadm-flags.env.
	if !o.dryRun {
		var err error
		if was.Secrets, err = readKubeadmSecrets(old, was); err != nil {
			return err
		}
		if is.Secrets, err = readKubeadmSecrets(cfg, is); err != nil {
			return err
		}
	}

	nodes := slices.Clone(prev.Outputs.Nodes)
	slices.SortStableFunc(nodes, func(a, b providers.Node) int {
		return map[bool]int{true: 0, false: 1}[a.Role == config.RoleControlPlane] - map[bool]int{true: 0, false: 1}[b.Role == config.RoleControlPlane]
	})
	var changes []kubeadmChange
	firstCP := true
	for _, n := range nodes {
		c := kubeadmChange{n: n, kubeletConfig: kubeletConfig}
		if n.Role == config.RoleControlPlane && cpWork {
			c.controlPlaneWork = true
			c.cp = kubeadm.Reconfigure{Components: components, SANs: sans, KubeVIP: kubeVIP, Upload: firstCP, Kubelet: kubeletConfig}
			firstCP = false
		}
		// The plan shows flags as the file writes them (a secret by its variable).
		shownWas, shownIs := kubeletFlagsShown(was, n), kubeletFlagsShown(is, n)
		addedShown, removedShown := flagChanges(shownWas, shownIs)
		c.added, c.removed = flagChanges(was.KubeletArgs(n), is.KubeletArgs(n))
		if len(c.added)+len(c.removed) == 0 && len(addedShown)+len(removedShown) > 0 {
			// A dry run reads no secret: the shown change is the change.
			c.added, c.removed = addedShown, removedShown
		}
		if c.controlPlaneWork {
			var parts []string
			if len(components) > 0 {
				parts = append(parts, strings.Join(components, ", ")+" written again (kubeadm init phase)")
			}
			if sans {
				parts = append(parts, "a new API server certificate with the file's certSANs")
			}
			if kubeVIP {
				parts = append(parts, fmt.Sprintf("kube-vip's static pod: it announces LoadBalancer Services too: %v", is.KubeVIPServices))
			}
			if c.cp.Upload {
				parts = append(parts, "the cluster's stored configuration first (kubeadm-config, kubelet-config)")
			}
			if len(parts) > 0 {
				c.what = append(c.what, strings.Join(parts, "; "))
			}
		}
		if len(addedShown)+len(removedShown) > 0 {
			var parts []string
			for _, f := range addedShown {
				parts = append(parts, "+ "+f)
			}
			for _, f := range removedShown {
				parts = append(parts, "− "+f)
			}
			c.what = append(c.what, "kubelet "+strings.Join(parts, ", "))
		}
		if kubeletConfig {
			c.what = append(c.what, fmt.Sprintf("kubelet: serving certificates signed by the cluster (for metrics-server): %v", is.ServerTLSBootstrap))
		}
		if len(c.what) > 0 {
			changes = append(changes, c)
		}
	}
	if len(changes) == 0 {
		return nil
	}

	lines := []string{"kubeadm's new settings, as its reconfigure page says, one machine at a time (control planes first): no drain, pods keep running; each node checked (a control plane: the API ready, its components running, etcd healthy) before the next"}
	for _, c := range changes {
		lines = append(lines, c.n.Name+": "+strings.Join(c.what, "; "))
	}
	release := prev.K8sVersion
	plan.cluster = append(plan.cluster, planStep{lines: lines, run: func(ctx context.Context) error {
		var done []string
		for _, c := range changes {
			if c.controlPlaneWork {
				if err := ui.StepRun(fmt.Sprintf("Writing %s's control plane again with its new settings", c.n.Label()), func() error {
					return reconfigureControlPlane(ctx, *is, c.n, c.cp)
				}); err != nil {
					return settingsStopped(err, done)
				}
			}
			if len(c.added)+len(c.removed) > 0 || c.kubeletConfig {
				if err := ui.StepRun(fmt.Sprintf("Restarting the kubelet on %s with its new settings", c.n.Label()), func() error {
					return kubeletFlags(ctx, *is, c.n, c.removed, c.added, c.kubeletConfig)
				}); err != nil {
					return settingsStopped(err, done)
				}
			}
			if err := o.nodeReady(ctx, cfg, prev, release, c.n); err != nil {
				return settingsStopped(err, done)
			}
			done = append(done, c.n.Name)
		}
		return nil
	}})
	return nil
}

func settingsStopped(err error, done []string) error {
	if len(done) > 0 {
		return fmt.Errorf("%w\nAlready on the new settings: %s (running it again applies them there again, which changes nothing)", err, strings.Join(done, ", "))
	}
	return err
}

// kubeletFlagsShown are a node's kubelet flags as the plan shows them: a secret by its
// variable, never its value.
func kubeletFlagsShown(o *kubeadm.Options, n providers.Node) []string {
	s, ok := o.Node(n)
	if !ok {
		return nil
	}
	var out []string
	for _, f := range rendered(s.KubeletArgs) {
		out = append(out, "--"+f)
	}
	return out
}

func sortedCopy(s []string) []string {
	out := slices.Clone(s)
	slices.Sort(out)
	return out
}
