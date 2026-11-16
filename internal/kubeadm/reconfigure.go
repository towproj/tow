package kubeadm

import (
	"cmp"
	"context"
	"fmt"
	"strings"

	"github.com/towproj/tow/internal/kubevip"
	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/ui"
)

// Sources (read 2026-11-13), followed here:
//   - reconfiguring a kubeadm cluster: the ClusterConfiguration in the kubeadm-config
//     ConfigMap is updated (kubeadm init phase upload-config kubeadm), then on each control
//     plane 'kubeadm init phase control-plane <component>' / 'init phase etcd local' write its
//     static pods again (the kubelet restarts them); a certSANs change needs 'init phase certs
//     apiserver' (an existing certificate is kept, so the old one goes first). The kubelet:
//     flags in /var/lib/kubelet/kubeadm-flags.env, or the kubelet-config ConfigMap (init phase
//     upload-config kubelet) pulled with 'kubeadm upgrade node phase kubelet-config', then the
//     kubelet restarted. One node at a time
//     https://kubernetes.io/docs/tasks/administer-cluster/kubeadm/kubeadm-reconfigure/

const flagsFile = "/var/lib/kubelet/kubeadm-flags.env"

// Reconfigure is what changes on one control plane: its components' static pods, the API
// server's certificate, kube-vip's, the cluster's stored configuration (the first one
// uploads it).
type Reconfigure struct {
	Components []string // apiserver, controller-manager, scheduler, etcd
	SANs       bool     // the API server's certificate names change
	KubeVIP    bool     // kube-vip's static pod changes (its Services part)
	Upload     bool     // upload the ClusterConfiguration (and the KubeletConfiguration)
	Kubelet    bool     // the KubeletConfiguration changed: upload it too
}

// controlPlaneConfig is a control plane's kubeadm config for kubeadm's init phases: how it
// registers, its address, the cluster and the kubelet's configuration, as at init, without
// any token.
func (o Options) controlPlaneConfig(n providers.Node) ([]byte, error) {
	s, _ := o.Node(n)
	in := initConfiguration{APIVersion: kubeadmAPI, Kind: "InitConfiguration", NodeRegistration: o.registration(n, s, true)}
	in.LocalAPIEndpoint = apiEndpoint{cmp.Or(n.PrivateIP, n.Host), apiPort}
	if o.SkipKubeProxy {
		// As at init: kubeadm records it in the ClusterConfiguration it uploads (proxy.disabled).
		in.SkipPhases = []string{"addon/kube-proxy"}
	}
	c := clusterConfiguration{APIVersion: kubeadmAPI, Kind: "ClusterConfiguration", ClusterName: o.Cluster,
		KubernetesVersion: o.Version, ControlPlaneEndpoint: o.ControlPlaneEndpoint}
	c.Networking.PodSubnet, c.Networking.ServiceSubnet, c.Networking.DNSDomain = o.PodCIDR, o.ServiceCIDR, o.DNSDomain
	c.APIServer.CertSANs = o.sans()
	c.APIServer.ExtraArgs = o.args(o.APIServer)
	c.ControllerManager.ExtraArgs = o.args(o.ControllerManager)
	c.Scheduler.ExtraArgs = o.args(o.Scheduler)
	c.Etcd.Local.ExtraArgs = o.args(o.Etcd)
	k := kubeletConfiguration{APIVersion: kubeletAPI, Kind: "KubeletConfiguration", CgroupDriver: "systemd", ServerTLSBootstrap: o.ServerTLSBootstrap}
	return documents(in, c, k)
}

// reconfigureScript runs kubeadm's init phases for what changed, with the config written
// root-only and removed after.
func (o Options) reconfigureScript(n providers.Node, r Reconfigure) (string, error) {
	cfg, err := o.controlPlaneConfig(n)
	if err != nil {
		return "", err
	}
	doc, err := heredoc(cfg)
	if err != nil {
		return "", err
	}
	// heredoc writes it root-only to "$cfg", removed when the script ends.
	var b strings.Builder
	b.WriteString("set -e\numask 077\n" + doc)
	if r.Upload {
		b.WriteString("kubeadm init phase upload-config kubeadm --config \"$cfg\"\n")
		if r.Kubelet {
			b.WriteString("kubeadm init phase upload-config kubelet --config \"$cfg\"\n")
		}
	}
	if r.SANs {
		b.WriteString("for f in apiserver.crt apiserver.key; do\n  if [ -f /etc/kubernetes/pki/$f ]; then mv /etc/kubernetes/pki/$f /etc/kubernetes/pki/$f.before-tow; fi\ndone\n")
		b.WriteString("kubeadm init phase certs apiserver --config \"$cfg\"\n")
		if !containsString(r.Components, "apiserver") {
			r.Components = append(r.Components, "apiserver") // it reads its certificate when it starts
		}
	}
	for _, c := range r.Components {
		if c == "etcd" {
			b.WriteString("kubeadm init phase etcd local --config \"$cfg\"\n")
			continue
		}
		fmt.Fprintf(&b, "kubeadm init phase control-plane %s --config \"$cfg\"\n", c)
	}
	return b.String(), nil
}

func containsString(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ReconfigureControlPlane writes one control plane's changed static pods (and kube-vip's) as
// kubeadm's reconfigure page says; the first uploads the cluster's configuration first. The
// kubelet restarts the static pods itself.
func ReconfigureControlPlane(ctx context.Context, o Options, n providers.Node, r Reconfigure) error {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	conn, err := o.Connect(ctx, n, o.KnownHosts)
	if err != nil {
		return err
	}
	defer conn.Close()
	if len(r.Components) > 0 || r.SANs || r.Upload {
		script, err := o.reconfigureScript(n, r)
		if err != nil {
			return err
		}
		if out, err := conn.RunInput(ctx, asRoot, script); err != nil {
			return fmt.Errorf("%s: kubeadm init phase failed: %w\n%s", n.Label(), err, ui.LastLines(out, 20))
		}
	}
	if r.KubeVIP && o.KubeVIP != "" {
		iface, err := kubevip.Interface(ctx, conn, n.Label(), cmp.Or(n.PrivateIP, n.Host), o.KubeVIP)
		if err != nil {
			return err
		}
		pod, err := kubevip.StaticPod(o.KubeVIP, iface, kubevip.AdminConf, o.KubeVIPServices)
		if err != nil {
			return err
		}
		script, err := kubevip.WriteScript(kubevip.StaticPodFile, pod)
		if err != nil {
			return err
		}
		if out, err := conn.RunInput(ctx, asRoot, "set -e\n"+script); err != nil {
			return fmt.Errorf("%s: could not put kube-vip in %s: %w\n%s", n.Label(), kubevip.StaticPodFile, err, ui.LastLines(out, 10))
		}
	}
	return nil
}

// KubeletFlags gives a node's kubelet the file's new flags: those tow set before are
// taken out of kubeadm-flags.env (kubeadm's own stay), the new ones added; with config, the
// kubelet's configuration is pulled again from the cluster (its kubelet-config ConfigMap).
// The kubelet restarts.
func KubeletFlags(ctx context.Context, o Options, n providers.Node, removed, added []string, config bool) error {
	if o.Connect == nil {
		o.Connect = prepare.Connect
	}
	ctx, cancel := context.WithTimeout(ctx, nodeTimeout)
	defer cancel()
	conn, err := o.Connect(ctx, n, o.KnownHosts)
	if err != nil {
		return err
	}
	defer conn.Close()
	var script strings.Builder
	script.WriteString("set -e\n")
	if len(removed)+len(added) > 0 {
		out, err := conn.RunInput(ctx, asRoot, "cat "+flagsFile)
		if err != nil {
			return fmt.Errorf("%s: read %s: %w\n%s", n.Label(), flagsFile, err, ui.LastLines(out, 5))
		}
		edited, err := editKubeletFlags(out, removed, added)
		if err != nil {
			return fmt.Errorf("%s: %w", n.Label(), err)
		}
		doc, err := heredoc([]byte(edited))
		if err != nil {
			return err
		}
		// Written in place: the file keeps its owner and mode.
		fmt.Fprintf(&script, "%scat \"$cfg\" > %s\n", doc, flagsFile)
	}
	if config {
		script.WriteString("kubeadm upgrade node phase kubelet-config\n")
	}
	script.WriteString("systemctl daemon-reload\nsystemctl restart kubelet\n")
	if out, err := conn.RunInput(ctx, asRoot, script.String()); err != nil {
		return fmt.Errorf("%s: the kubelet's new settings: %w\n%s", n.Label(), err, ui.LastLines(out, 15))
	}
	return nil
}

// editKubeletFlags rewrites KUBELET_KUBEADM_ARGS in kubeadm-flags.env: removed flags out
// ("--max-pods=110"), added ones in; the rest of the file, and kubeadm's own flags, as they are.
func editKubeletFlags(content string, removed, added []string) (string, error) {
	lines := strings.Split(content, "\n")
	for i, l := range lines {
		value, ok := strings.CutPrefix(l, "KUBELET_KUBEADM_ARGS=")
		if !ok {
			continue
		}
		value = strings.Trim(value, `"`)
		var keep []string
		for _, f := range strings.Fields(value) {
			if !containsString(removed, f) {
				keep = append(keep, f)
			}
		}
		for _, f := range added {
			if !containsString(keep, f) {
				keep = append(keep, f)
			}
		}
		lines[i] = `KUBELET_KUBEADM_ARGS="` + strings.Join(keep, " ") + `"`
		return strings.Join(lines, "\n"), nil
	}
	return "", fmt.Errorf("%s has no KUBELET_KUBEADM_ARGS line", flagsFile)
}

// KubeletArgs are a node's kubelet flags as the file sets them, as kubeadm-flags.env writes
// them: "--max-pods=200" (a secret's value included).
func (o Options) KubeletArgs(n providers.Node) []string {
	s, ok := o.Node(n)
	if !ok {
		return nil
	}
	var out []string
	for _, a := range o.args(s.KubeletArgs) {
		out = append(out, "--"+a.Name+"="+a.Value)
	}
	return out
}
