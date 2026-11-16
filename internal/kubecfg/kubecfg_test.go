package kubecfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/client-go/tools/clientcmd"
)

// k3d's kubeconfig shape, with fake credentials.
const fromK3d = `apiVersion: v1
kind: Config
clusters:
- name: k3d-dev
  cluster: {server: "https://0.0.0.0:41234", certificate-authority-data: ZmFrZQ==}
users:
- name: admin@k3d-dev
  user: {client-certificate-data: ZmFrZQ==, client-key-data: ZmFrZQ==}
contexts:
- name: k3d-dev
  context: {cluster: k3d-dev, user: admin@k3d-dev}
current-context: k3d-dev
`

const other = `apiVersion: v1
kind: Config
clusters:
- name: other
  cluster: {server: "https://10.0.0.1:6443"}
users:
- name: other
  user: {token: fake}
contexts:
- name: other
  context: {cluster: other, user: other}
current-context: other
`

func TestRename(t *testing.T) {
	out, err := Rename([]byte(fromK3d), "dev")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(out)
	if err != nil {
		t.Fatal(err)
	}
	ctx := cfg.Contexts["dev"]
	if ctx == nil || ctx.Cluster != "dev" || ctx.AuthInfo != "dev" || cfg.CurrentContext != "dev" || len(cfg.Contexts) != 1 {
		t.Errorf("renamed config: %+v", cfg)
	}
	if cfg.Clusters["dev"].Server != "https://0.0.0.0:41234" || cfg.AuthInfos["dev"] == nil {
		t.Errorf("the cluster and its credentials must be kept: %+v", cfg)
	}
}

// Merge, look up and remove act on $KUBECONFIG like kubectl does, and keep other clusters.
func TestMergeAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte(other), 0o600)
	t.Setenv("KUBECONFIG", path)

	dev, _ := Rename([]byte(fromK3d), "dev")
	if err := Merge(dev); err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.LoadFromFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Contexts["dev"] == nil || cfg.Contexts["other"] == nil || cfg.CurrentContext != "dev" {
		t.Errorf("after merge: contexts %v, current %q", keys(cfg.Contexts), cfg.CurrentContext)
	}
	if info, _ := os.Stat(path); info.Mode().Perm() != 0o600 {
		t.Errorf("kubeconfig mode = %v, want 0600", info.Mode().Perm())
	}
	if ok, _ := HasContext("dev"); !ok {
		t.Error("HasContext(dev) = false after merge")
	}
	if ok, _ := HasContext("nope"); ok {
		t.Error("HasContext(nope) = true")
	}

	if err := Remove("dev"); err != nil {
		t.Fatal(err)
	}
	cfg, _ = clientcmd.LoadFromFile(path)
	if cfg.Contexts["dev"] != nil || cfg.Clusters["dev"] != nil || cfg.AuthInfos["dev"] != nil || cfg.Contexts["other"] == nil || cfg.CurrentContext != "" {
		t.Errorf("after remove: contexts %v, clusters %v, current %q", keys(cfg.Contexts), len(cfg.Clusters), cfg.CurrentContext)
	}
	if err := Remove("dev"); err != nil {
		t.Errorf("removing a context that's gone is not an error: %v", err)
	}
}

// Add merges without moving the user off the cluster they're on, unless they're on the same
// cluster under its old name.
func TestAdd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	os.WriteFile(path, []byte(other), 0o600)
	t.Setenv("KUBECONFIG", path)

	dev, _ := Rename([]byte(fromK3d), "dev")
	if err := Add(dev, "lab"); err != nil {
		t.Fatal(err)
	}
	cfg, _ := clientcmd.LoadFromFile(path)
	if cfg.Contexts["dev"] == nil || cfg.CurrentContext != "other" {
		t.Errorf("contexts %v, current %q: want dev added, other still current", keys(cfg.Contexts), cfg.CurrentContext)
	}

	renamed, _ := Rename([]byte(fromK3d), "dev-2")
	Merge(dev) // the user is on dev
	if err := Add(renamed, "dev"); err != nil {
		t.Fatal(err)
	}
	if cfg, _ = clientcmd.LoadFromFile(path); cfg.CurrentContext != "dev-2" {
		t.Errorf("current %q: the user was on this cluster, under its old name", cfg.CurrentContext)
	}
}

func TestSaveIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cluster", "kubeconfig")
	if err := Save(path, []byte("x")); err != nil {
		t.Fatal(err)
	}
	file, _ := os.Stat(path)
	dir, _ := os.Stat(filepath.Dir(path))
	if file.Mode().Perm() != 0o600 || dir.Mode().Perm() != 0o700 {
		t.Errorf("modes: file %v, folder %v", file.Mode().Perm(), dir.Mode().Perm())
	}
}

// k3s writes its kubeconfig for the machine itself (127.0.0.1); tow points it at the API
// address, keeps it, merges it under the file's context, and refuses a name already taken.
func TestPointStoreAndCheckFree(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "config"))
	data, err := Point([]byte(fromK3d), "https://203.0.113.10:6443")
	if err != nil {
		t.Fatal(err)
	}
	if cfg, _ := clientcmd.Load(data); cfg.Clusters["k3d-dev"].Server != "https://203.0.113.10:6443" {
		t.Errorf("server = %s", cfg.Clusters["k3d-dev"].Server)
	}

	if err := CheckFree("lab"); err != nil {
		t.Errorf("no context yet: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := Store(data, "lab", path, false); err != nil {
		t.Fatal(err)
	}
	if ok, _ := HasContext("lab"); ok {
		t.Error("merge: false keeps the user's kubeconfig as it is")
	}
	if saved, err := clientcmd.LoadFromFile(path); err != nil || saved.CurrentContext != "lab" {
		t.Errorf("the state folder's copy: %v, %v", saved, err)
	}
	if err := Store(data, "lab", path, true); err != nil {
		t.Fatal(err)
	}
	if err := CheckFree("lab"); err == nil || !strings.Contains(err.Error(), "already has a context named 'lab'") {
		t.Errorf("a taken name must be refused: %v", err)
	}
}

// A name the user's kubeconfig gives a cluster or a user, under another context's name, is
// taken too: merging would replace that entry, and the other context would reach this cluster.
func TestCheckFreeClusterAndUser(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	shared := strings.ReplaceAll(other, "- name: other\n  context:", "- name: other-admin\n  context:")
	if err := os.WriteFile(path, []byte(shared), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	if err := CheckFree("other"); err == nil || !strings.Contains(err.Error(), "already has a cluster named 'other'") {
		t.Errorf("a cluster entry's name must be refused: %v", err)
	}
	user := strings.ReplaceAll(shared, "clusters:\n- name: other\n", "clusters:\n- name: lab\n")
	user = strings.ReplaceAll(user, "{cluster: other,", "{cluster: lab,")
	if err := os.WriteFile(path, []byte(user), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckFree("other"); err == nil || !strings.Contains(err.Error(), "already has a user named 'other'") {
		t.Errorf("a user entry's name must be refused: %v", err)
	}
	if err := CheckFree("lab-2"); err != nil {
		t.Errorf("a free name: %v", err)
	}
}

func keys[V any](m map[string]V) string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return strings.Join(out, ",")
}

// A copy whose context names a namespace: kubectl puts objects that name none there.
func TestWithNamespace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(other), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := WithNamespace(path, "shop")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := clientcmd.Load(data)
	if err != nil || cfg.Contexts["other"].Namespace != "shop" || cfg.Clusters["other"].Server != "https://10.0.0.1:6443" {
		t.Errorf("copy: %+v, %v", cfg, err)
	}
	if orig, _ := os.ReadFile(path); string(orig) != other {
		t.Error("the cluster's own kubeconfig must stay as it is")
	}
}
