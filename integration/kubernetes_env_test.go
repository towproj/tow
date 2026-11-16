package integration

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/towproj/tow/internal/config"
	"gopkg.in/yaml.v3"
)

// loadWithKubernetes loads a cluster file with TOW_TEST_KUBERNETES, YAML for
// spec.kubernetes, merged into it: its keys win, maps merge. The result goes through the same
// parsing and checks as any file. It lets one test file try other Kubernetes choices:
// '{containerRuntime: cri-o, networking: {cni: flannel}}', for instance.
func loadWithKubernetes(t *testing.T, path string) *config.Config {
	t.Helper()
	extra := os.Getenv("TOW_TEST_KUBERNETES")
	if extra == "" {
		cfg, err := config.Load(path)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := config.ParseIn(withKubernetes(t, data, extra), filepath.Dir(path))
	if err != nil {
		t.Fatalf("%s with TOW_TEST_KUBERNETES=%s: %v", path, extra, err)
	}
	return cfg
}

// withKubernetes merges extra, YAML for spec.kubernetes, into a cluster file.
func withKubernetes(t *testing.T, file []byte, extra string) []byte {
	t.Helper()
	var doc, add map[string]any
	if err := yaml.Unmarshal(file, &doc); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal([]byte(extra), &add); err != nil {
		t.Fatalf("TOW_TEST_KUBERNETES must be YAML for spec.kubernetes, like '{networking: {cni: flannel}}': %v", err)
	}
	spec, _ := doc["spec"].(map[string]any)
	if spec == nil {
		t.Fatal("the cluster file has no spec")
	}
	k, _ := spec["kubernetes"].(map[string]any)
	spec["kubernetes"] = merge(k, add)
	out, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("spec.kubernetes with TOW_TEST_KUBERNETES: %v", spec["kubernetes"])
	return out
}

func merge(base, add map[string]any) map[string]any {
	if base == nil {
		base = map[string]any{}
	}
	for k, v := range add {
		if m, ok := v.(map[string]any); ok {
			if b, ok := base[k].(map[string]any); ok {
				base[k] = merge(b, m)
				continue
			}
		}
		base[k] = v
	}
	return base
}

// The merge keeps the file's other keys and replaces only what the variable names.
func TestWithKubernetes(t *testing.T) {
	file := []byte("apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: x}\nspec:\n  flavor: dev-test\n  kubernetes: {version: v1.37.2, networking: {podCIDR: 10.50.0.0/16, cni: calico}}\n  infrastructure: {provider: aws, aws: {region: eu-west-3}}\n")
	var doc struct {
		Spec struct {
			Flavor     string         `yaml:"flavor"`
			Kubernetes map[string]any `yaml:"kubernetes"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(withKubernetes(t, file, "{containerRuntime: cri-o, networking: {cni: flannel}}"), &doc); err != nil {
		t.Fatal(err)
	}
	k := doc.Spec.Kubernetes
	n, _ := k["networking"].(map[string]any)
	if doc.Spec.Flavor != "dev-test" || k["version"] != "v1.37.2" || k["containerRuntime"] != "cri-o" || n["cni"] != "flannel" || n["podCIDR"] != "10.50.0.0/16" {
		t.Errorf("merged: %+v", doc.Spec)
	}
}
