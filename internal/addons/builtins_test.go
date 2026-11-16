package addons

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

func named(t *testing.T, bs []Builtin, name string) Builtin {
	t.Helper()
	for _, b := range bs {
		if b.Name() == name {
			return b
		}
	}
	t.Fatalf("no built-in add-on %s in %v", name, bs)
	return Builtin{}
}

// Each built-in add-on says what it is, where its version is read, and what turning it off
// runs: the objects it applied, its release or manifest, its own namespace.
func TestBuiltinUnits(t *testing.T) {
	o, _ := catalog(t, func(k *config.Kubernetes) {
		k.ServiceLoadBalancer = config.ServiceLoadBalancer{Provider: config.ServiceLBMetalLB,
			MetalLB: &config.PoolController{AddressPool: config.AddressPool{Mode: config.ModeL2, Addresses: []string{"192.168.1.200-192.168.1.210"}}}}
		k.Ingress = config.Ingress{Controllers: []string{config.IngressTraefik}, DefaultClass: config.IngressTraefik}
	})
	bs := Builtins(o)
	var names []string
	for _, b := range bs {
		names = append(names, b.Name())
	}
	if want := []string{"local-path", "openebs", "longhorn", "nfs", "smb", "default StorageClass longhorn", "metallb", "traefik", "kubelet-csr-approver", "metrics-server"}; !slices.Equal(names, want) {
		t.Fatalf("units %v, want %v", names, want)
	}

	removes := func(name string) string { return strings.Join(named(t, bs, name).RemoveCommands(), "\n") }
	lh := named(t, bs, "longhorn")
	if !lh.Storage() || lh.CSI() != config.CSILonghorn || lh.Class() != "longhorn" || lh.Release() != "longhorn-system/longhorn" || lh.Pin() != "" || lh.Version() != LonghornVersion {
		t.Errorf("longhorn: %+v", lh)
	}
	if r := removes("longhorn"); !strings.HasPrefix(r, "kubectl patch settings.longhorn.io deleting-confirmation-flag") ||
		!strings.Contains(r, "helm uninstall longhorn --namespace longhorn-system") || !strings.HasSuffix(r, "kubectl delete namespace longhorn-system --ignore-not-found --timeout 10m --kubeconfig <kubeconfig>") {
		t.Errorf("longhorn's removal:\n%s", r)
	}
	if r := removes("nfs"); !strings.Contains(r, "kubectl delete --ignore-not-found") || !strings.Contains(r, "(stdin: --- …)") ||
		!strings.Contains(r, "helm uninstall csi-driver-nfs --namespace kube-system") || strings.Contains(r, "delete namespace") {
		t.Errorf("nfs's removal (its StorageClass, its release, never kube-system):\n%s", r)
	}
	if objs := named(t, bs, "smb").Objects(); len(objs) != 2 || objs[0].Kind != "Secret" || objs[1].String() != "storageclass/smb" {
		t.Errorf("smb applies %v", objs)
	}
	lp := named(t, bs, "local-path")
	if lp.Deployment() != "local-path-storage/local-path-provisioner" || lp.Release() != "" {
		t.Errorf("local-path's version is its Deployment's: %q %q", lp.Deployment(), lp.Release())
	}
	if r := removes("local-path"); !strings.Contains(r, "kubectl delete --ignore-not-found --timeout 10m --kubeconfig <kubeconfig> -f https://raw.githubusercontent.com/rancher/local-path-provisioner/v"+LocalPathVersion+"/") {
		t.Errorf("local-path's removal:\n%s", r)
	}
	if d := named(t, bs, "default StorageClass longhorn"); !d.DefaultClass() || d.Class() != "longhorn" ||
		removes(d.Name()) != "kubectl annotate storageclass longhorn storageclass.kubernetes.io/is-default-class- --overwrite --kubeconfig "+o.Kubeconfig {
		t.Errorf("default class: %+v\n%s", d, removes(d.Name()))
	}
	mlb := named(t, bs, "metallb")
	if r := removes("metallb"); !mlb.ServiceLB() || !strings.HasPrefix(r, "kubectl delete") || !strings.Contains(r, "helm uninstall metallb --namespace metallb-system") ||
		!strings.Contains(r, "delete namespace metallb-system") {
		t.Errorf("metallb's removal:\n%s", r)
	}
	if objs := mlb.Objects(); len(objs) != 2 || objs[0].String() != "ipaddresspool/tow in metallb-system" || objs[1].Kind != "L2Advertisement" {
		t.Errorf("metallb applies %v (its namespace left out)", objs)
	}
	if tr := named(t, bs, "traefik"); !tr.Ingress() || tr.Class() != "traefik" || !strings.Contains(removes("traefik"), "delete namespace traefik") {
		t.Errorf("traefik: %+v", tr)
	}

	// Running versions are kept, unless the file pins one.
	o.Running = map[string]string{"longhorn": "1.12.1", "nfs": "4.12.0"}
	o.Kubernetes.Storage.NFS.Version = "v4.13.6"
	bs = Builtins(o)
	if v := named(t, bs, "longhorn").Version(); v != "1.12.1" {
		t.Errorf("longhorn %s: it keeps the one that runs", v)
	}
	if b := named(t, bs, "nfs"); b.Version() != "4.13.6" || b.Pin() != "4.13.6" || b.Tested() != NFSVersion {
		t.Errorf("nfs %s (pin %s): the file's pin wins", b.Version(), b.Pin())
	}
}

// Two units are the same when they run the same commands with the same input; a StorageClass
// whose settings changed is made again; what's no longer applied is known.
func TestBuiltinChanges(t *testing.T) {
	o, _ := catalog(t, nil)
	before := named(t, Builtins(o), "nfs")
	if !before.Same(named(t, Builtins(o), "nfs")) {
		t.Error("the same file builds the same unit")
	}
	o.Kubernetes.Storage.NFS.Server = "192.168.1.6"
	after := named(t, Builtins(o), "nfs")
	if after.Same(before) {
		t.Error("another server is a change")
	}
	if r := after.Replaced(before); len(r) != 1 || r[0].String() != "storageclass/nfs" {
		t.Errorf("replaced %v", r)
	}
	o.Kubernetes.Storage.Longhorn.Replicas = new(int)
	*o.Kubernetes.Storage.Longhorn.Replicas = 3
	if lh := named(t, Builtins(o), "longhorn"); lh.Replaced(named(t, Builtins(o), "longhorn")) != nil {
		t.Error("a chart's values replace nothing")
	}

	pool := func(mode string) Builtin {
		o, _ := catalog(t, func(k *config.Kubernetes) {
			p := config.AddressPool{Mode: mode, Addresses: []string{"192.168.1.200/29"}}
			if mode == config.ModeBGP {
				p.BGP = &config.BGP{LocalASN: 64512, Peers: []config.BGPPeer{{Address: "192.168.1.1", ASN: 64513}}}
			}
			k.ServiceLoadBalancer = config.ServiceLoadBalancer{Provider: config.ServiceLBMetalLB, MetalLB: &config.PoolController{AddressPool: p}}
		})
		return named(t, Builtins(o), "metallb")
	}
	l2, bgp := pool(config.ModeL2), pool(config.ModeBGP)
	if gone := Missing(l2.Objects(), bgp.Objects()); len(gone) != 1 || gone[0].Kind != "L2Advertisement" {
		t.Errorf("l2 → bgp drops %v", gone)
	}
}

// Cilium's settings: compared like a unit, its version kept, its release named.
func TestNetworkChanges(t *testing.T) {
	on := true
	n := Network{Networking: config.Networking{CNI: config.CNICilium, PodCIDR: "10.42.0.0/16", Cilium: &config.Cilium{KubeProxyReplacement: &on}},
		APIHost: "10.0.1.10", APIPort: "6443", Running: "1.19.2"}
	if n.Version() != "1.19.2" || n.Pin() != "" || n.Tested() != CiliumVersion || n.Release() != "kube-system/cilium" {
		t.Errorf("cilium %s (pin %q), release %s", n.Version(), n.Pin(), n.Release())
	}
	m := n
	c := *n.Networking.Cilium
	c.Encryption = "wireguard"
	m.Networking.Cilium = &c
	if !n.Same(n) || n.Same(m) {
		t.Error("encryption is a change")
	}
	m.Networking.Cilium.Version = "v1.20.4"
	if m.Version() != "1.20.4" {
		t.Errorf("the file's pin wins: %s", m.Version())
	}
}

func TestVersions(t *testing.T) {
	for chart, want := range map[string]string{"longhorn-1.13.0": "1.13.0", "csi-driver-nfs-v4.13.6": "4.13.6", "aws-ebs-csi-driver-2.68.0": "2.68.0",
		"kubelet-csr-approver-1.2.15": "1.2.15", "flannel-v0.28.10": "0.28.10", "weird": ""} {
		if got := chartVersion(chart); got != want {
			t.Errorf("chartVersion(%s) = %q, want %q", chart, got, want)
		}
	}
	for image, want := range map[string]string{"docker.io/rancher/local-path-provisioner:v0.0.37": "0.0.37", "localhost:5000/kube-vip-cloud-provider:v0.0.13@sha256:abc": "0.0.13",
		"localhost:5000/x": "", "ghcr.io/kube-vip/kube-vip-cloud-provider": ""} {
		if got := ImageVersion(image); got != want {
			t.Errorf("ImageVersion(%s) = %q, want %q", image, got, want)
		}
	}
	if !Older("1.12.1", "1.13.0") || Older("1.13.0", "1.13.0") || Older("1.14.0", "1.13.0") || !Older("v0.0.9", "0.0.10") || Older("x", "1.0") {
		t.Error("Older")
	}
}

// Removing a unit runs its commands in order; the first failure stops it, with the tool's
// output.
func TestRemoveBuiltin(t *testing.T) {
	var calls []string
	orig := run
	run = func(_ context.Context, program string, args []string, _ string) (string, string, error) {
		calls = append(calls, program+" "+args[0])
		return "", "", nil
	}
	t.Cleanup(func() { run = orig })
	o, _ := catalog(t, nil)
	if err := o.RemoveBuiltin(context.Background(), named(t, Builtins(o), "longhorn")); err != nil {
		t.Fatal(err)
	}
	if want := []string{"/bin/kubectl patch", "/bin/helm uninstall", "/bin/kubectl delete"}; !slices.Equal(calls, want) {
		t.Errorf("calls %v, want %v", calls, want)
	}
}
