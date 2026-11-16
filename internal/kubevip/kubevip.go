// Package kubevip puts kube-vip in front of several control planes: a virtual IP (the file's
// apiEndpoint) that one control plane at a time holds and announces on the local network
// (ARP), moved to another by leader election when it goes away. Only the API: Services get
// their addresses from the service load balancer the file names.
//
// Sources (read 2026-11-11), followed here:
//   - static pods for kubeadm: the manifest in /etc/kubernetes/manifests before kubeadm init on
//     the first control plane, then on the others; since Kubernetes 1.29 admin.conf isn't
//     usable during init, so the first control plane starts with super-admin.conf, then
//     switches to admin.conf  https://kube-vip.io/docs/installation/static/ (kube-vip#684)
//   - k3s: a DaemonSet in /var/lib/rancher/k3s/server/manifests before k3s starts, with the
//     virtual IP in tls-san  https://kube-vip.io/docs/usage/k3s/
//   - the manifests are what 'kube-vip manifest pod' and 'kube-vip manifest daemonset --taint'
//     print at v1.3.1 for --address <vip> --interface <name> --controlplane --arp
//     --leaderElection (pkg/kubevip/config_generator.go, cmd/kube-vip.go defaults)
package kubevip

import (
	"bytes"
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/towproj/tow/internal/prepare"
	"github.com/towproj/tow/internal/remote"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

const (
	Version = "v1.3.1"
	// Image is pinned by its digest too: the tag alone could be moved.
	Image = "ghcr.io/kube-vip/kube-vip:" + Version + "@sha256:1962f9daf226503f7e6b107f595cb28b7917fd2d0113957075e19a3e5a6c1e33"
	// LeaseName is the Lease (kube-system) whose holder has the virtual IP.
	LeaseName = "plndr-cp-lock"

	StaticPodFile  = "/etc/kubernetes/manifests/kube-vip.yaml"
	K3sManifest    = "/var/lib/rancher/k3s/server/manifests/kube-vip.yaml"
	AdminConf      = "/etc/kubernetes/admin.conf"
	SuperAdminConf = "/etc/kubernetes/super-admin.conf"
	K3sKubeconfig  = "/etc/rancher/k3s/k3s.yaml"

	apiPort   = "6443"
	namespace = "kube-system"
	// kube-vip reads the kubeconfig mounted here and, as a control plane, reaches its own API
	// server at kubernetes:6443, which the pod's host alias makes 127.0.0.1: it never needs
	// the virtual IP it's about to hold, nor the cluster's Service network.
	mountPath = "/etc/kubernetes/admin.conf"
)

// StaticPod is the kubeadm control plane's manifest: kube-vip reads kubeconfig (AdminConf, or
// SuperAdminConf on the first control plane while kubeadm init runs). services has it
// announce LoadBalancer Services' addresses too (kube-vip-cloud-provider hands them out).
func StaticPod(vip, iface, kubeconfig string, services bool) ([]byte, error) {
	p := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vip", Namespace: namespace},
		Spec:       podSpec(vip, iface, kubeconfig, services),
	}
	return yaml.Marshal(p)
}

// DaemonSet is k3s's: kube-vip on every control plane, reading the k3s admin kubeconfig
// there. Not the in-cluster account kube-vip's k3s page uses: that reaches the API through
// the Service network, which a CNI replacing kube-proxy only brings once the API answers on
// the virtual IP.
func DaemonSet(vip, iface string, services bool) ([]byte, error) {
	labels := map[string]string{"app.kubernetes.io/name": "kube-vip-ds", "app.kubernetes.io/version": Version}
	spec := podSpec(vip, iface, K3sKubeconfig, services)
	spec.Tolerations = []corev1.Toleration{
		{Effect: corev1.TaintEffectNoSchedule, Operator: corev1.TolerationOpExists},
		{Effect: corev1.TaintEffectNoExecute, Operator: corev1.TolerationOpExists},
	}
	spec.Affinity = &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{NodeSelectorTerms: []corev1.NodeSelectorTerm{{
			MatchExpressions: []corev1.NodeSelectorRequirement{{Key: "node-role.kubernetes.io/control-plane", Operator: corev1.NodeSelectorOpExists}},
		}}},
	}}
	d := appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: "kube-vip-ds", Namespace: namespace, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": "kube-vip-ds"}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: spec},
		},
	}
	return withoutStatus(d)
}

// withoutStatus marshals an object without the empty status the API server would ignore.
func withoutStatus(obj any) ([]byte, error) {
	out, err := yaml.Marshal(obj)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		return nil, err
	}
	delete(m, "status")
	return yaml.Marshal(m)
}

func env(name, value string) corev1.EnvVar { return corev1.EnvVar{Name: name, Value: value} }

// leaderElection is kube-vip's defaults for its Kubernetes leader election.
var leaderElection = []corev1.EnvVar{
	env("vip_leaderelection", "true"),
	env("vip_leasename", LeaseName),
	env("vip_leaseduration", "15"),
	env("vip_renewdeadline", "10"),
	env("vip_retryperiod", "2"),
}

// servicesEnv turns on Services: one kube-vip at a time (its own Lease) announces them.
var servicesEnv = []corev1.EnvVar{env("svc_enable", "true"), env("svc_leasename", ServicesLeaseName)}

// ServicesLeaseName is the Lease whose holder announces the Services' addresses.
const ServicesLeaseName = "plndr-svcs-lock"

var capabilities = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{
	Add:  []corev1.Capability{"NET_ADMIN", "NET_RAW"},
	Drop: []corev1.Capability{"ALL"},
}}

func podSpec(vip, iface, kubeconfig string, services bool) corev1.PodSpec {
	e := []corev1.EnvVar{
		env("vip_arp", "true"),
		env("port", apiPort),
		{Name: "vip_nodename", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		env("vip_interface", iface),
		env("vip_subnet", "32"),
		env("dns_mode", "first"),
		env("cp_enable", "true"),
		env("cp_namespace", namespace),
	}
	if services {
		e = append(e, servicesEnv...)
	}
	e = append(e, leaderElection...)
	e = append(e, env("address", vip), env("prometheus_server", ":2112"))
	return corev1.PodSpec{
		HostNetwork: true,
		HostAliases: []corev1.HostAlias{{IP: "127.0.0.1", Hostnames: []string{"kubernetes"}}},
		Containers: []corev1.Container{{
			Name:            "kube-vip",
			Image:           Image,
			ImagePullPolicy: corev1.PullIfNotPresent,
			Args:            []string{"manager"},
			Env:             e,
			SecurityContext: capabilities,
			VolumeMounts:    []corev1.VolumeMount{{Name: "kubeconfig", MountPath: mountPath}},
		}},
		Volumes: []corev1.Volume{{Name: "kubeconfig", VolumeSource: corev1.VolumeSource{
			HostPath: &corev1.HostPathVolumeSource{Path: kubeconfig},
		}}},
	}
}

// ServicesDaemonSet is kube-vip for Services alone, on a cluster whose API it doesn't serve:
// what 'kube-vip manifest daemonset --services --inCluster --arp --leaderElection' prints,
// with its service account and the ClusterRole 'kube-vip manifest rbac' gives it. Each node
// announces on the interface of its default route (kube-vip's own choice without
// --interface).
func ServicesDaemonSet() ([]byte, error) {
	const name, account = "kube-vip-svc", "kube-vip"
	labels := map[string]string{"app.kubernetes.io/name": name, "app.kubernetes.io/version": Version}
	e := []corev1.EnvVar{
		env("vip_arp", "true"),
		env("port", apiPort),
		{Name: "vip_nodename", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "spec.nodeName"}}},
		env("dns_mode", "first"),
	}
	e = append(e, servicesEnv...)
	e = append(e, leaderElection...)
	e = append(e, env("prometheus_server", ":2112"))
	ds := appsv1.DaemonSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: "apps/v1", Kind: "DaemonSet"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app.kubernetes.io/name": name}},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: labels}, Spec: corev1.PodSpec{
				ServiceAccountName: account,
				HostNetwork:        true,
				Containers: []corev1.Container{{Name: "kube-vip", Image: Image, ImagePullPolicy: corev1.PullIfNotPresent,
					Args: []string{"manager"}, Env: e, SecurityContext: capabilities}},
			}},
		},
	}
	sa := corev1.ServiceAccount{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "ServiceAccount"}, ObjectMeta: metav1.ObjectMeta{Name: account, Namespace: namespace}}
	role := rbacv1.ClusterRole{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRole"},
		ObjectMeta: metav1.ObjectMeta{Name: "system:kube-vip-role"},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"services/status"}, Verbs: []string{"update"}},
			{APIGroups: []string{""}, Resources: []string{"services", "endpoints"}, Verbs: []string{"list", "get", "watch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"nodes"}, Verbs: []string{"list", "get", "watch", "update", "patch"}},
			{APIGroups: []string{"coordination.k8s.io"}, Resources: []string{"leases"}, Verbs: []string{"list", "get", "watch", "update", "create"}},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list", "get", "watch", "update"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"list"}},
			{APIGroups: []string{"networking.k8s.io"}, Resources: []string{"servicecidrs"}, Verbs: []string{"list", "get", "watch"}},
		},
	}
	binding := rbacv1.ClusterRoleBinding{
		TypeMeta:   metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "ClusterRoleBinding"},
		ObjectMeta: metav1.ObjectMeta{Name: "system:kube-vip-binding"},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: role.Name},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: account, Namespace: namespace}},
	}
	var docs [][]byte
	for _, obj := range []any{sa, role, binding} {
		b, err := yaml.Marshal(obj)
		if err != nil {
			return nil, err
		}
		docs = append(docs, b)
	}
	b, err := withoutStatus(ds)
	if err != nil {
		return nil, err
	}
	return bytes.Join(append(docs, b), []byte("---\n")), nil
}

// Interface finds the network interface holding the machine's private address, where kube-vip
// announces the virtual IP. The virtual IP must be on that interface's network: ARP doesn't
// cross routers.
func Interface(ctx context.Context, r prepare.Runner, label, privateIP, vip string) (string, error) {
	out, err := r.RunInput(ctx, remote.AsRoot, "ip -o -4 addr show\n")
	if err != nil {
		return "", fmt.Errorf("%s: could not list its network interfaces: %w", label, err)
	}
	return parseInterface(out, label, privateIP, vip)
}

// parseInterface reads 'ip -o -4 addr show': "2: ens18    inet 192.168.100.20/24 brd ... ".
func parseInterface(out, label, privateIP, vip string) (string, error) {
	v, err := netip.ParseAddr(vip)
	if err != nil {
		return "", fmt.Errorf("kube-vip's address %q isn't an IP address", vip)
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != "inet" {
			continue
		}
		p, err := netip.ParsePrefix(f[3])
		if err != nil || p.Addr().String() != privateIP {
			continue
		}
		iface, _, _ := strings.Cut(f[1], "@") // a VLAN shows as eth0.10@eth0
		if !p.Masked().Contains(v) {
			return "", fmt.Errorf("%s: the API address %s isn't on its network %s (%s); kube-vip announces it there (ARP), which doesn't cross routers: pick a free address in %s", label, vip, p.Masked(), iface, p.Masked())
		}
		return iface, nil
	}
	return "", fmt.Errorf("%s: no network interface holds its address %s, where kube-vip would announce the API address", label, privateIP)
}

// WriteScript puts a manifest in place, root-only, unless it's already there as it is: a
// static pod or k3s manifest rewritten restarts what it runs.
func WriteScript(path string, content []byte) (string, error) {
	const end = "TOW_KUBE_VIP_EOF"
	if bytes.Contains(content, []byte(end)) {
		return "", fmt.Errorf("the kube-vip manifest contains the heredoc marker %s", end)
	}
	return fmt.Sprintf(`new=$(mktemp)
cat > "$new" <<'%[1]s'
%[2]s%[1]s
if cmp -s "$new" '%[3]s'; then rm -f "$new"; else install -D -m 0600 "$new" '%[3]s' && rm -f "$new"; fi
`, end, content, path), nil
}
