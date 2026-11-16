package core

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/addons"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
)

// fakeBuiltins is helm, kubectl and the API for the built-in add-ons: what runs as given,
// every change recorded.
type fakeBuiltins struct {
	releases  map[string]string // namespace/name → version; nil: what the file asks for runs
	images    map[string]string // a Deployment's namespace/name → its image
	volumes   []string
	services  []string
	ingresses []string
	calls     []string
	installed map[string]addons.Builtin
}

func (f *fakeBuiltins) ReleaseVersions(context.Context) (map[string]string, error) {
	return f.releases, nil
}
func (f *fakeBuiltins) DeploymentImage(_ context.Context, ns, name string) (string, error) {
	return f.images[ns+"/"+name], nil
}
func (f *fakeBuiltins) Volumes(context.Context, []string) ([]string, error) { return f.volumes, nil }
func (f *fakeBuiltins) LoadBalancerServices(context.Context) ([]string, error) {
	return f.services, nil
}
func (f *fakeBuiltins) Ingresses(context.Context, string, bool) ([]string, error) {
	return f.ingresses, nil
}
func (f *fakeBuiltins) Install(_ context.Context, b addons.Builtin) error {
	f.calls = append(f.calls, "install "+b.Name())
	f.installed[b.Name()] = b
	return nil
}
func (f *fakeBuiltins) Remove(_ context.Context, b addons.Builtin) error {
	f.calls = append(f.calls, "remove "+b.Name())
	return nil
}
func (f *fakeBuiltins) DeleteObjects(_ context.Context, objs []addons.Object) error {
	if len(objs) > 0 {
		f.calls = append(f.calls, "delete "+describeObjects(objs))
	}
	return nil
}
func (f *fakeBuiltins) UpdateCNI(_ context.Context, n addons.Network) error {
	f.calls = append(f.calls, "cni "+n.Networking.CNI+" "+n.Version())
	return nil
}

// useBuiltins swaps the built-in add-ons' client for f; nil: one where every add-on of the
// file runs, at the version the file asks for.
func useBuiltins(t *testing.T, f *fakeBuiltins) *fakeBuiltins {
	t.Helper()
	if f == nil {
		f = &fakeBuiltins{}
	}
	f.installed = map[string]addons.Builtin{}
	orig := newBuiltinClient
	newBuiltinClient = func(ad addons.Options) (builtinClient, error) {
		if f.releases == nil {
			f.releases, f.images = map[string]string{}, map[string]string{}
			for _, b := range addons.Builtins(ad) {
				if b.Release() != "" {
					f.releases[b.Release()] = b.Version()
				}
				if b.Deployment() != "" {
					f.images[b.Deployment()] = "example/" + b.Name() + ":v" + b.Version()
				}
			}
			if n := (addons.Network{Networking: ad.Kubernetes.Networking}); n.Release() != "" {
				f.releases[n.Release()] = n.Version()
			}
		}
		return f, nil
	}
	t.Cleanup(func() { newBuiltinClient = orig })
	return f
}

// builtinsLab is a kubeadm cluster on two of the user's machines, with what kubernetes
// (YAML) adds to spec.kubernetes: the lab* blocks below, say.
func builtinsLab(t *testing.T, kubernetes string) *config.Config {
	t.Helper()
	return parse(t, "lab", `{infrastructure: {provider: byoh, access: {ssh: {user: pi}}},
  kubernetes: {distribution: kubeadm, `+kubernetes+`},
  controlPlane: {machines: [{host: 10.0.0.1}]}, workers: [{name: general, machines: [{host: 10.0.0.2}]}]}`)
}

const (
	labCilium   = `networking: {cni: cilium, cilium: {kubeProxyReplacement: true}}`
	labStorage  = `storage: {csi: [longhorn, nfs], defaultClass: longhorn, nfs: {server: 10.0.0.9, path: /share}}`
	labMetalLB  = `serviceLoadBalancer: {provider: metallb, metallb: {addresses: [10.0.0.200-10.0.0.210]}}`
	labTraefik  = `ingress: {controllers: [traefik]}`
	labBuiltins = labCilium + ", " + labStorage + ", " + labMetalLB + ", " + labTraefik
)

func applyYes(t *testing.T, cfg *config.Config) error {
	t.Helper()
	return NewOrchestrator(false).AssumeYes().Apply(context.Background(), cfg)
}

// The built-in add-ons follow the file: a load balancer it replaces goes first, the CNI's
// new settings next, then what's turned on or changed in install order (a StorageClass
// whose settings changed made again), and what's turned off last.
func TestBuiltinsFollowTheFile(t *testing.T) {
	useFakes(t)
	running(t, builtinsLab(t, labBuiltins))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	f := useBuiltins(t, &fakeBuiltins{services: []string{"shop/web"}, ingresses: []string{"shop/web"}})

	// Longhorn's replicas and NFS's server change, OpenEBS is added, Cilium takes over the
	// Services from MetalLB, and Traefik goes.
	changed := builtinsLab(t, labCilium+`, storage: {csi: [longhorn, nfs, openebs], defaultClass: longhorn, longhorn: {replicas: 2},
    nfs: {server: 10.0.0.8, path: /share}}, serviceLoadBalancer: {provider: cilium, cilium: {addresses: [10.0.0.200-10.0.0.210]}}`)
	// What runs is the first file's add-ons, at tow's versions.
	f.releases = map[string]string{"kube-system/cilium": addons.CiliumVersion, "longhorn-system/longhorn": addons.LonghornVersion,
		"kube-system/csi-driver-nfs": addons.NFSVersion, "metallb-system/metallb": addons.MetalLBVersion, "traefik/traefik": addons.TraefikVersion}
	if err := applyYes(t, changed); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"remove metallb",
		"cni cilium " + addons.CiliumVersion,
		"install longhorn",
		"delete storageclass/nfs", "install nfs",
		"install openebs",
		"install cilium load balancer pool",
		"remove traefik",
	}
	if !slices.Equal(f.calls, want) {
		t.Errorf("calls:\n  %s\nwant:\n  %s", strings.Join(f.calls, "\n  "), strings.Join(want, "\n  "))
	}
	if v := f.installed["openebs"].Version(); v != addons.OpenEBSVersion {
		t.Errorf("a new add-on installs tow's version, got %s", v)
	}

	// Applied again: nothing left to do.
	f.calls = nil
	f.releases = nil
	if err := applyYes(t, changed); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("an unchanged file changed %v", f.calls)
	}
}

// The Services a load balancer's removal affects leave out those of add-ons turned off in
// the same run (Traefik's own, say).
func TestAffectedServices(t *testing.T) {
	o := addons.Options{Kubernetes: builtinsLab(t, labMetalLB+", "+labTraefik).Spec.Kubernetes}
	old := addons.Builtins(o)
	kept := byName(addons.Builtins(addons.Options{Kubernetes: builtinsLab(t, labMetalLB).Spec.Kubernetes}))
	got := outside([]string{"default/web", "traefik/traefik", "shop/api"}, removedNamespaces(old, kept))
	if want := []string{"default/web", "shop/api"}; !slices.Equal(got, want) {
		t.Errorf("affected %v, want %v", got, want)
	}
}

// A changed add-on stays at the version that runs, not the one this tow installs; one
// whose release is gone is installed again.
func TestBuiltinsKeepTheirVersion(t *testing.T) {
	useFakes(t)
	running(t, builtinsLab(t, labStorage))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	f := useBuiltins(t, &fakeBuiltins{releases: map[string]string{"longhorn-system/longhorn": "1.12.1"}}) // NFS's release is gone

	if err := applyYes(t, builtinsLab(t, `storage: {csi: [longhorn, nfs], defaultClass: longhorn, longhorn: {replicas: 2}, nfs: {server: 10.0.0.9, path: /share}}`)); err != nil {
		t.Fatal(err)
	}
	if want := []string{"install longhorn", "install nfs"}; !slices.Equal(f.calls, want) {
		t.Errorf("calls = %v, want %v", f.calls, want)
	}
	if v := f.installed["longhorn"].Version(); v != "1.12.1" {
		t.Errorf("longhorn changed to %s: it keeps the version that runs", v)
	}
	if v := f.installed["nfs"].Version(); v != addons.NFSVersion {
		t.Errorf("nfs installed again at %s", v)
	}
}

// Refused before anything changes: another version of an add-on that runs, storage that still
// has volumes, k3s's bundled add-ons, kube-vip's Services while it serves the API.
func TestBuiltinsRefused(t *testing.T) {
	useFakes(t)
	running(t, builtinsLab(t, labStorage))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	f := useBuiltins(t, &fakeBuiltins{volumes: []string{"shop/data-db-0"},
		releases: map[string]string{"longhorn-system/longhorn": addons.LonghornVersion, "kube-system/csi-driver-nfs": addons.NFSVersion}})

	err := applyYes(t, builtinsLab(t, `storage: {csi: [longhorn, nfs], defaultClass: longhorn, longhorn: {version: 1.12.1}, nfs: {server: 10.0.0.9, path: /share}}`))
	wantErr(t, err, "nothing was changed", "longhorn: "+addons.LonghornVersion+" runs, the file asks for 1.12.1", "Kubernetes upgrade")
	err = applyYes(t, builtinsLab(t, `storage: {csi: [longhorn], defaultClass: longhorn}`))
	wantErr(t, err, "nothing was changed", "nfs still has 1 volume (shop/data-db-0): delete them")
	if len(f.calls) != 0 {
		t.Errorf("a refused run changed %v", f.calls)
	}

	// From the two files alone.
	k3s := parse(t, "lab", `{infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, flavor: homelab,
  controlPlane: {machines: [{host: 10.0.0.1}]}, workers: [{name: general, machines: [{host: 10.0.0.2}]}]}`)
	noTraefik := parse(t, "lab", `{infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, flavor: homelab, kubernetes: {ingress: {controllers: []}},
  controlPlane: {machines: [{host: 10.0.0.1}]}, workers: [{name: general, machines: [{host: 10.0.0.2}]}]}`)
	// k3s's Traefik is a setting that rolls; its servicelb is a critical one.
	if got := refusedSettings(k3s, noTraefik, nil); len(got) != 0 {
		t.Errorf("k3s's traefik: %v", got)
	}
	noServiceLB := parse(t, "lab", `{infrastructure: {provider: byoh, access: {ssh: {user: pi}}}, flavor: homelab, kubernetes: {serviceLoadBalancer: {provider: none}},
  controlPlane: {machines: [{host: 10.0.0.1}]}, workers: [{name: general, machines: [{host: 10.0.0.2}]}]}`)
	if got := refusedSettings(k3s, noServiceLB, nil); len(got) != 1 || !strings.Contains(got[0], "k3s's disable=servicelb must be the same on every server") {
		t.Errorf("k3s's servicelb: %v", got)
	}
	kv, noKV := builtinsLab(t, labStorage), builtinsLab(t, labStorage)
	kv.Spec.Kubernetes.APILoadBalancer, noKV.Spec.Kubernetes.APILoadBalancer = config.APILBKubeVIP, config.APILBKubeVIP
	kv.Spec.Kubernetes.ServiceLoadBalancer.Provider = config.ServiceLBKubeVIP
	// kubeadm: kube-vip's static pod is written again on each control plane (planKubeadmSettings).
	if got := refusedSettings(kv, noKV, nil); len(got) != 0 {
		t.Errorf("kube-vip: %v", got)
	}
	if got := refusedSettings(builtinsLab(t, labStorage), builtinsLab(t, labStorage+", "+labMetalLB), nil); len(got) != 0 {
		t.Errorf("kubeadm's own add-ons can change: %v", got)
	}
}

// status and apply name the add-ons this tow installs a newer release of, unless the
// file pins theirs.
func TestBuiltinsNewerReleases(t *testing.T) {
	useFakes(t)
	running(t, builtinsLab(t, labCilium+`, storage: {csi: [longhorn, nfs], defaultClass: longhorn, nfs: {version: 4.12.0, server: 10.0.0.9, path: /share}}`))
	useBuiltins(t, &fakeBuiltins{releases: map[string]string{"longhorn-system/longhorn": "1.12.1", "kube-system/csi-driver-nfs": "4.12.0", "kube-system/cilium": "1.19.2"}})
	useHealth(t, kubeapi.Health{Nodes: []kubeapi.Node{{Name: "cp", ControlPlane: true, Ready: true}, {Name: "w1", Ready: true}}}, nil)

	r, err := NewOrchestrator(false).Report(context.Background(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"longhorn " + addons.LonghornVersion + " (1.12.1 runs)", "cilium " + addons.CiliumVersion + " (1.19.2 runs)"}
	if !slices.Equal(r.Newer, want) {
		t.Errorf("newer = %v, want %v (nfs is pinned)", r.Newer, want)
	}
	var out bytes.Buffer
	printReport(&out, r)
	if !strings.Contains(out.String(), "Add-ons: newer releases this tow installs, which come with the next Kubernetes upgrade: longhorn") {
		t.Errorf("status:\n%s", out.String())
	}
}

// A dry run reads what runs and shows the plan; it changes nothing and reads no secret.
func TestBuiltinsDryRun(t *testing.T) {
	useFakes(t)
	running(t, builtinsLab(t, labStorage))
	useNodes(t, kubeapi.NodeMeta{Name: "cp", Taints: []kubeapi.Taint{controlPlaneTaint}}, kubeapi.NodeMeta{Name: "w1"})
	f := useBuiltins(t, nil)
	if err := NewOrchestrator(true).Apply(context.Background(), builtinsLab(t, labStorage+", "+labMetalLB)); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Errorf("a dry run changed %v", f.calls)
	}
}
