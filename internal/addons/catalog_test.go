package addons

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/ui"
	"k8s.io/client-go/tools/clientcmd"
)

const testKubeconfig = `apiVersion: v1
kind: Config
clusters: [{name: lab, cluster: {server: "https://10.0.1.10:6443"}}]
contexts: [{name: lab, context: {cluster: lab, user: admin}}]
current-context: lab
users: [{name: admin, user: {token: abc}}]
`

// catalog is a kubeadm cluster asking for every add-on of the first batch, with fake tools.
func catalog(t *testing.T, edit func(*config.Kubernetes)) (Options, string) {
	t.Helper()
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfig")
	if err := os.WriteFile(kc, []byte(testKubeconfig), 0o600); err != nil {
		t.Fatal(err)
	}
	on, replicas := true, 2
	k := config.Kubernetes{
		Distribution: config.DistributionKubeadm,
		Storage: config.Storage{
			CSI:          []string{config.CSILocalPath, config.CSIOpenEBS, config.CSILonghorn, config.CSINFS, config.CSISMB, config.CSIRookCeph},
			DefaultClass: config.CSILonghorn,
			Longhorn:     &config.Longhorn{Replicas: &replicas},
			NFS:          &config.NFS{Server: "192.168.1.5", Path: "/export/k8s"},
			SMB:          &config.SMB{Source: "//nas/k8s", UsernameEnv: "SMB_USER", PasswordEnv: "SMB_PASSWORD"},
		},
		MetricsServer: config.MetricsServer{Enabled: &on},
		Addons: config.Addons{
			HelmCharts: []config.HelmChart{{Name: "podinfo", Repo: "https://stefanprodan.github.io/podinfo", Chart: "podinfo", Version: "6.17.0", Namespace: "podinfo"}},
			Manifests:  []config.Manifest{{Path: "https://example.com/app.yaml"}, {Path: "manifests", Namespace: "shop"}},
		},
	}
	if edit != nil {
		edit(&k)
	}
	return Options{Helm: "/bin/helm", Kubectl: "/bin/kubectl", Kubeconfig: kc, Dir: dir, Kubernetes: k, Charts: k.Addons.HelmCharts,
		Secrets:      map[string]string{"SMB_USER": "k8s", "SMB_PASSWORD": "smb-s3cret"},
		ManifestPath: func(p string) string { return filepath.Join("/cluster", p) },
		Nodes: []providers.Node{
			{Name: "lab-cp-0", Host: "203.0.113.10", PrivateIP: "10.0.1.10"},
			{Name: "lab-w.0", Host: "nas.example", PrivateIP: "10.0.1.20"},
		}}, dir
}

// kubectlCall is a kubectl command with the namespace of the kubeconfig it was given, read
// while it ran (the copy is removed after).
type kubectlCall struct {
	call
	namespace string
}

func useCatalogRun(t *testing.T) *[]kubectlCall {
	t.Helper()
	calls := useRun(t, "")
	inner := run
	var out []kubectlCall
	run = func(ctx context.Context, program string, args []string, stdin string) (string, string, error) {
		c := kubectlCall{}
		if i := slices.Index(args, "--kubeconfig"); i >= 0 && strings.Contains(args[i+1], "kubeconfig-") {
			cfg, err := clientcmd.LoadFromFile(args[i+1])
			if err != nil {
				t.Errorf("namespaced kubeconfig: %v", err)
			} else {
				c.namespace = cfg.Contexts[cfg.CurrentContext].Namespace
			}
		}
		o, e, err := inner(ctx, program, args, stdin)
		c.call = (*calls)[len(*calls)-1]
		out = append(out, c)
		return o, e, err
	}
	return &out
}

// Every add-on of the first batch, in order, as its docs say; the second batch's storage
// (rook-ceph) is left for later.
func TestCatalog(t *testing.T) {
	calls := useCatalogRun(t)
	o, dir := catalog(t, nil)
	if err := Install(context.Background(), o); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range *calls {
		got = append(got, c.args[0]+" "+strings.Join(c.args[1:3], " "))
	}
	want := []string{
		"apply --server-side --kubeconfig", // local-path's manifest
		"rollout status deployment/local-path-provisioner",
		"upgrade --install openebs",
		"upgrade --install longhorn",
		"upgrade --install csi-driver-nfs", "apply --kubeconfig " + o.Kubeconfig, // the NFS StorageClass
		"upgrade --install csi-driver-smb", "apply --kubeconfig " + o.Kubeconfig, // the SMB Secret and StorageClass
		"annotate storageclass longhorn",
		"upgrade --install kubelet-csr-approver", "upgrade --install metrics-server",
		"upgrade --install podinfo",
		"apply --server-side --field-manager", "apply --server-side --field-manager", // https://example.com/app.yaml
		"apply --server-side --field-manager", "apply --server-side --field-manager", // manifests, namespace shop
	}
	if !slices.Equal(got, want) {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	c := *calls
	if !strings.HasSuffix(c[0].args[len(c[0].args)-1], "/local-path-provisioner/v0.0.37/deploy/local-path-storage.yaml") {
		t.Errorf("local-path: %v", c[0].args)
	}
	if v := c[2].values; at(v, "engines", "replicated", "mayastor", "enabled") != false || at(v, "engines", "local", "lvm", "enabled") != false ||
		at(v, "engines", "local", "zfs", "enabled") != false || at(v, "loki", "enabled") != false || at(v, "alloy", "enabled") != false || !slices.Contains(c[2].args, "4.6.2") {
		t.Errorf("openebs: only hostpath stays on: %v %v", c[2].args, v)
	}
	if v := c[3].values; at(v, "persistence", "defaultClass") != false || at(v, "persistence", "defaultClassReplicaCount") != 2 || at(v, "defaultSettings", "defaultReplicaCount") != 2 {
		t.Errorf("longhorn: %v", v)
	}
	for _, want := range []string{"name: nfs", "provisioner: nfs.csi.k8s.io", `server: "192.168.1.5"`, `share: "/export/k8s"`, "nfsvers=4.1"} {
		if !strings.Contains(c[5].stdin, want) {
			t.Errorf("NFS StorageClass lacks %q:\n%s", want, c[5].stdin)
		}
	}
	for _, want := range []string{"kind: Secret", `username: "k8s"`, `password: "smb-s3cret"`, `source: "//nas/k8s"`, "node-stage-secret-name: smb-credentials", "noserverino"} {
		if !strings.Contains(c[7].stdin, want) {
			t.Errorf("SMB objects lack %q:\n%s", want, c[7].stdin)
		}
	}
	if v := c[9].values; v["providerRegex"] != `^(lab-cp-0|lab-w\.0)$` || v["bypassDnsResolution"] != true ||
		strings.Join(toStrings(v["providerIpPrefixes"]), ",") != "10.0.1.10/32,203.0.113.10/32,10.0.1.20/32" {
		t.Errorf("kubelet-csr-approver allows the nodes only: %v", v)
	}
	if c[12].stdin != "apiVersion: v1\nkind: Namespace\nmetadata:\n  name: app\n  labels: {app.kubernetes.io/managed-by: tow}\n" ||
		c[13].namespace != "app" || !slices.Contains(c[13].args, "https://example.com/app.yaml") || !slices.Contains(c[13].args, "json") {
		t.Errorf("manifest app: %q, namespace %q, %v", c[12].stdin, c[13].namespace, c[13].args)
	}
	if c[15].namespace != "shop" || !slices.Contains(c[15].args, "--recursive") || !slices.Contains(c[15].args, "/cluster/manifests") {
		t.Errorf("manifest folder: namespace %q, %v", c[15].namespace, c[15].args)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*-*.yaml")); len(left) != 0 {
		t.Errorf("values and kubeconfig copies left behind: %v", left)
	}

	// The dry run names every command, never the SMB password.
	lines := strings.Join(Commands(o), "\n")
	for _, want := range []string{"local-path 0.0.37:", "openebs 4.6.2:", "longhorn 1.13.0:", "nfs 4.13.6:", "smb 1.20.4:", "default StorageClass longhorn:",
		"kubelet-csr-approver 1.2.15:", "metrics-server 3.14.0:", "Helm chart podinfo 6.17.0:", "manifest manifests:", "<the cluster's kubeconfig, namespace shop>"} {
		if !strings.Contains(lines, want) {
			t.Errorf("dry run lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "smb-s3cret") || strings.Contains(ui.Redact("x smb-s3cret y"), "smb-s3cret") {
		t.Errorf("the SMB password shows:\n%s", lines)
	}
	if !o.NeedsHelm() || !o.NeedsKubectl() || o.Empty() {
		t.Error("needs")
	}
}

func toStrings(v any) []string {
	var out []string
	for _, x := range v.([]any) {
		out = append(out, x.(string))
	}
	return out
}

// k3s brings local-path (the default class) and metrics-server itself; nothing to install
// then, and no tool needed.
func TestCatalogOnK3s(t *testing.T) {
	on := true
	o := Options{Kubernetes: config.Kubernetes{Distribution: config.DistributionK3s, MetricsServer: config.MetricsServer{Enabled: &on},
		Storage: config.Storage{CSI: []string{config.CSILocalPath}, DefaultClass: config.CSILocalPath}}}
	if !o.Empty() || o.NeedsHelm() || o.NeedsKubectl() {
		t.Errorf("k3s bundles them: %v", Commands(o))
	}
	// Longhorn on k3s: installed; local-path stays k3s's default, and nothing else is marked.
	o.Kubernetes.Storage.CSI = append(o.Kubernetes.Storage.CSI, config.CSILonghorn)
	got := strings.Join(Commands(o), "\n")
	if !strings.Contains(got, "longhorn 1.13.0:") || strings.Contains(got, "annotate") || strings.Contains(got, "metrics-server") {
		t.Errorf("k3s + longhorn:\n%s", got)
	}
}

// A failing step stops the install and names the add-on with the tool's output.
func TestCatalogFailure(t *testing.T) {
	useRun(t, "upgrade")
	o, _ := catalog(t, func(k *config.Kubernetes) {
		k.Storage = config.Storage{CSI: []string{config.CSIOpenEBS}}
		k.MetricsServer, k.Addons = config.MetricsServer{}, config.Addons{}
	})
	o.Charts = nil
	err := Install(context.Background(), o)
	if err == nil || !strings.Contains(err.Error(), "openebs: helm upgrade") || !strings.Contains(err.Error(), "could not find the requested resource") {
		t.Errorf("err = %v", err)
	}
}
