package kubevip

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func envOf(c corev1.Container) map[string]string {
	m := map[string]string{}
	for _, e := range c.Env {
		m[e.Name] = e.Value
		if e.ValueFrom != nil && e.ValueFrom.FieldRef != nil {
			m[e.Name] = "field:" + e.ValueFrom.FieldRef.FieldPath
		}
	}
	return m
}

// What 'kube-vip manifest pod --address 192.168.100.100 --interface ens18 --controlplane
// --arp --leaderElection' prints at v1.3.1, field for field.
func TestStaticPod(t *testing.T) {
	out, err := StaticPod("192.168.100.100", "ens18", SuperAdminConf, false)
	if err != nil {
		t.Fatal(err)
	}
	var p corev1.Pod
	if err := yaml.UnmarshalStrict(out, &p); err != nil {
		t.Fatalf("not a Pod: %v\n%s", err, out)
	}
	if p.Kind != "Pod" || p.Name != "kube-vip" || p.Namespace != "kube-system" || !p.Spec.HostNetwork {
		t.Errorf("pod: %+v", p.ObjectMeta)
	}
	c := p.Spec.Containers[0]
	if c.Image != Image || !strings.Contains(Image, "@sha256:") || strings.Join(c.Args, " ") != "manager" {
		t.Errorf("container: %s %v", c.Image, c.Args)
	}
	want := map[string]string{
		"vip_arp": "true", "port": "6443", "vip_nodename": "field:spec.nodeName", "vip_interface": "ens18", "vip_subnet": "32",
		"dns_mode": "first", "cp_enable": "true", "cp_namespace": "kube-system", "vip_leaderelection": "true",
		"vip_leasename": LeaseName, "vip_leaseduration": "15", "vip_renewdeadline": "10", "vip_retryperiod": "2",
		"address": "192.168.100.100", "prometheus_server": ":2112",
	}
	got := envOf(c)
	for k, v := range want {
		if got[k] != v {
			t.Errorf("env %s = %q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("env has %d variables, want %d: %v", len(got), len(want), got)
	}
	if p.Spec.Volumes[0].HostPath.Path != SuperAdminConf || c.VolumeMounts[0].MountPath != "/etc/kubernetes/admin.conf" {
		t.Errorf("kubeconfig: %+v %+v", p.Spec.Volumes, c.VolumeMounts)
	}
	if h := p.Spec.HostAliases; len(h) != 1 || h[0].IP != "127.0.0.1" || h[0].Hostnames[0] != "kubernetes" {
		t.Errorf("host alias: %+v", h)
	}
	if caps := c.SecurityContext.Capabilities; len(caps.Add) != 2 || caps.Drop[0] != "ALL" {
		t.Errorf("capabilities: %+v", caps)
	}
}

func TestDaemonSet(t *testing.T) {
	out, err := DaemonSet("192.168.100.100", "eth0", false)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "status") {
		t.Errorf("status left in:\n%s", out)
	}
	var d appsv1.DaemonSet
	if err := yaml.UnmarshalStrict(out, &d); err != nil {
		t.Fatalf("not a DaemonSet: %v\n%s", err, out)
	}
	s := d.Spec.Template.Spec
	if d.Kind != "DaemonSet" || d.Name != "kube-vip-ds" || d.Spec.Selector.MatchLabels["app.kubernetes.io/name"] != d.Spec.Template.Labels["app.kubernetes.io/name"] {
		t.Errorf("daemonset: %+v", d.ObjectMeta)
	}
	if s.Volumes[0].HostPath.Path != K3sKubeconfig || s.ServiceAccountName != "" {
		t.Errorf("reads k3s's kubeconfig, no service account: %+v %q", s.Volumes, s.ServiceAccountName)
	}
	if envOf(s.Containers[0])["vip_interface"] != "eth0" || len(s.Tolerations) != 2 ||
		s.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms[0].MatchExpressions[0].Key != "node-role.kubernetes.io/control-plane" {
		t.Errorf("control planes only: %+v %+v", s.Tolerations, s.Affinity)
	}
}

// With Services, the API's kube-vip also announces LoadBalancer addresses (its own Lease).
func TestServices(t *testing.T) {
	out, err := StaticPod("192.168.100.100", "ens18", AdminConf, true)
	if err != nil {
		t.Fatal(err)
	}
	var p corev1.Pod
	if err := yaml.UnmarshalStrict(out, &p); err != nil {
		t.Fatal(err)
	}
	if e := envOf(p.Spec.Containers[0]); e["svc_enable"] != "true" || e["svc_leasename"] != ServicesLeaseName || e["cp_enable"] != "true" {
		t.Errorf("env: %v", e)
	}

	// Without the API's kube-vip: a DaemonSet for Services alone, with its account and role.
	out, err = ServicesDaemonSet()
	if err != nil {
		t.Fatal(err)
	}
	docs := strings.Split(string(out), "---\n")
	if len(docs) != 4 {
		t.Fatalf("%d documents:\n%s", len(docs), out)
	}
	var d appsv1.DaemonSet
	if err := yaml.UnmarshalStrict([]byte(docs[3]), &d); err != nil {
		t.Fatal(err)
	}
	s := d.Spec.Template.Spec
	e := envOf(s.Containers[0])
	if s.ServiceAccountName != "kube-vip" || len(s.Volumes) != 0 || e["svc_enable"] != "true" || e["cp_enable"] != "" || e["vip_interface"] != "" || e["address"] != "" {
		t.Errorf("services-only kube-vip: account %q, volumes %v, env %v", s.ServiceAccountName, s.Volumes, e)
	}
	for _, want := range []string{"kind: ServiceAccount", "kind: ClusterRole\n", "- services/status", "kind: ClusterRoleBinding", "name: system:kube-vip-role"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("lacks %q", want)
		}
	}
}

func TestParseInterface(t *testing.T) {
	out := `1: lo    inet 127.0.0.1/8 scope host lo\       valid_lft forever preferred_lft forever
2: ens18    inet 192.168.100.20/24 brd 192.168.100.255 scope global ens18\       valid_lft forever preferred_lft forever
2: ens18    inet 192.168.100.100/32 scope global ens18\       valid_lft forever preferred_lft forever
3: eth0.10@eth0    inet 10.0.10.5/24 brd 10.0.10.255 scope global eth0.10
`
	for _, c := range []struct{ ip, vip, want, err string }{
		{"192.168.100.20", "192.168.100.100", "ens18", ""}, // the virtual IP already held: still the machine's own address
		{"10.0.10.5", "10.0.10.100", "eth0.10", ""},
		{"192.168.100.20", "192.168.1.100", "", "isn't on its network 192.168.100.0/24 (ens18)"},
		{"192.168.100.21", "192.168.100.100", "", "no network interface holds its address 192.168.100.21"},
		{"192.168.100.20", "api.lab", "", "isn't an IP address"},
	} {
		got, err := parseInterface(out, "cp", c.ip, c.vip)
		if got != c.want || (c.err == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.err)) {
			t.Errorf("%s/%s: %q, %v", c.ip, c.vip, got, err)
		}
	}
}

// The write script parses, writes the manifest root-only, and leaves an unchanged one alone
// (a static pod rewritten would restart).
func TestWriteScript(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("scripts target Linux nodes")
	}
	path := filepath.Join(t.TempDir(), "manifests", "kube-vip.yaml")
	script, err := WriteScript(path, []byte("kind: Pod\n"))
	if err != nil {
		t.Fatal(err)
	}
	run := func() {
		t.Helper()
		if out, err := exec.Command("sh", "-c", "set -e\n"+script).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	run()
	st, err := os.Stat(path)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("written: %v, %v", st, err)
	}
	if data, _ := os.ReadFile(path); string(data) != "kind: Pod\n" {
		t.Errorf("content = %q", data)
	}
	before := st.ModTime()
	if err := os.Chtimes(path, before.Add(-3600e9), before.Add(-3600e9)); err != nil {
		t.Fatal(err)
	}
	run()
	if st2, _ := os.Stat(path); !st2.ModTime().Equal(before.Add(-3600e9)) {
		t.Error("an unchanged manifest was written again")
	}
	if _, err := WriteScript(path, []byte("TOW_KUBE_VIP_EOF\n")); err == nil {
		t.Error("the heredoc marker must be refused")
	}
}
