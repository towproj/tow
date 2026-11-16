package integration

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// TestK3dAddonChanges changes a running cluster's own Helm charts and manifests through
// 'apply', one file after another: a chart and a manifest added, then the chart's values
// changed and an object dropped from the manifest, then both removed from the file. Each
// step is checked through the API, and state's record of what the manifest made with it.
func TestK3dAddonChanges(t *testing.T) {
	if os.Getenv("INTEGRATION") == "" {
		t.Skip("set INTEGRATION=1 to run real infra tests")
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig"))
	ui.SetNonInteractive(true)

	name := fmt.Sprintf("test-addons-%d", time.Now().Unix())
	dir := t.TempDir()
	manifest := filepath.Join(dir, "app.yaml") // its objects go into namespace "app"
	writeManifest := func(objs ...string) {
		t.Helper()
		var data string
		for _, o := range objs {
			data += "---\napiVersion: v1\nkind: ConfigMap\nmetadata: {name: " + o + "}\ndata: {from: tow}\n"
		}
		if err := os.WriteFile(manifest, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// file builds the cluster file with the add-ons given (YAML under spec.kubernetes.addons).
	file := func(addons string) *config.Config {
		t.Helper()
		cfg, err := config.ParseIn([]byte(fmt.Sprintf(`apiVersion: towproj.github.io/v1alpha1
kind: Cluster
metadata: {name: %s}
spec:
  infrastructure: {provider: local}
  kubernetes:
    kubeconfig: {context: %s}
    addons: {%s}
`, name, name, addons)), dir)
		if err != nil {
			t.Fatal(err)
		}
		return cfg
	}
	podinfo := func(replicas int) string {
		return fmt.Sprintf(`helmCharts: [{name: podinfo, repo: "https://stefanprodan.github.io/podinfo", chart: podinfo,
      version: 6.17.0, namespace: demo, values: {replicaCount: %d}}]`, replicas)
	}

	cfg := file("")
	ensureTools(t, cfg.Spec.Kubernetes.Release()) // helm, and kubectl for the manifest

	orch := core.NewOrchestrator(false).AssumeYes() // apply --yes: a running cluster's plan asks
	yes := func(string) bool { return true }
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		if state.Exists(name) {
			if err := orch.Delete(ctx, name, yes); err != nil {
				t.Errorf("cleanup: %v", err)
			}
		}
		exec.CommandContext(ctx, "k3d", "cluster", "delete", name).Run()
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()

	apply := func(step string, cfg *config.Config) (*state.ClusterState, kubernetes.Interface) {
		t.Helper()
		if err := orch.Apply(ctx, cfg); err != nil {
			t.Fatalf("%s: apply: %v", step, err)
		}
		st, err := state.Load(name)
		if err != nil {
			t.Fatal(err)
		}
		return st, clientset(t, st)
	}
	replicas := func(cs kubernetes.Interface) int32 {
		d, err := cs.AppsV1().Deployments("demo").Get(ctx, "podinfo", metav1.GetOptions{})
		if err != nil {
			return -1
		}
		return d.Status.ReadyReplicas
	}
	configMaps := func(t *testing.T, cs kubernetes.Interface, want map[string]bool) {
		t.Helper()
		for cm, present := range want {
			_, err := cs.CoreV1().ConfigMaps("app").Get(ctx, cm, metav1.GetOptions{})
			if present && err != nil {
				t.Errorf("ConfigMap app/%s: %v", cm, err)
			}
			if !present && !apierrors.IsNotFound(err) {
				t.Errorf("ConfigMap app/%s still there (%v)", cm, err)
			}
		}
	}
	recorded := func(t *testing.T, st *state.ClusterState, want int) {
		t.Helper()
		if got := len(st.Manifests["app.yaml"]); got != want {
			t.Errorf("state records %d objects for app.yaml, want %d: %v", got, want, st.Manifests)
		}
	}

	// A cluster with no add-ons of its own.
	apply("create", cfg)

	// Added: the chart (1 replica) and the manifest (2 ConfigMaps).
	writeManifest("one", "two")
	added := file(podinfo(1) + ", manifests: [app.yaml]")
	st, cs := apply("add", added)
	eventually(t, ctx, "podinfo with 1 ready replica", func() bool { return replicas(cs) == 1 })
	configMaps(t, cs, map[string]bool{"one": true, "two": true})
	recorded(t, st, 2)

	// The same file again: nothing to change, nothing lost from the record.
	st, _ = apply("unchanged", added)
	recorded(t, st, 2)

	// Changed: 2 replicas; "two" left the manifest.
	writeManifest("one")
	st, cs = apply("change", file(podinfo(2)+", manifests: [app.yaml]"))
	eventually(t, ctx, "podinfo with 2 ready replicas", func() bool { return replicas(cs) == 2 })
	configMaps(t, cs, map[string]bool{"one": true, "two": false})
	recorded(t, st, 1)

	// The manifest's namespace deleted by hand: the same file puts it back, with "one",
	// which stays (it isn't taken for something the manifest dropped).
	if err := cs.CoreV1().Namespaces().Delete(ctx, "app", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	eventually(t, ctx, "namespace app deleted", func() bool {
		_, err := cs.CoreV1().Namespaces().Get(ctx, "app", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	st, cs = apply("namespace gone", file(podinfo(2)+", manifests: [app.yaml]"))
	configMaps(t, cs, map[string]bool{"one": true})
	recorded(t, st, 1)

	// Removed: the release is uninstalled and the manifest's objects deleted; the namespaces
	// stay.
	st, cs = apply("remove", file(""))
	eventually(t, ctx, "podinfo gone", func() bool {
		_, err := cs.AppsV1().Deployments("demo").Get(ctx, "podinfo", metav1.GetOptions{})
		return apierrors.IsNotFound(err)
	})
	configMaps(t, cs, map[string]bool{"one": false})
	if len(st.Manifests) != 0 {
		t.Errorf("state still records objects: %v", st.Manifests)
	}
	for _, ns := range []string{"demo", "app"} {
		if _, err := cs.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); err != nil {
			t.Errorf("namespace %s should stay: %v", ns, err)
		}
	}
}
