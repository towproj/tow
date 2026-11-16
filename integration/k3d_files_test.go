package integration

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/kubecfg"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// checkPodinfo proves the podinfo chart went on with the file's values: 2 replicas ready,
// answering through its Service (reached through the API's service proxy).
func checkPodinfo(t *testing.T, ctx context.Context, cs kubernetes.Interface) {
	t.Helper()
	d, err := cs.AppsV1().Deployments("demo").Get(ctx, "podinfo", metav1.GetOptions{})
	if err != nil || d.Status.ReadyReplicas != 2 {
		t.Fatalf("podinfo deployment: %v, ready %d", err, d.Status.ReadyReplicas)
	}
	out, err := cs.CoreV1().Services("demo").ProxyGet("http", "podinfo", "9898", "/", nil).DoRaw(ctx)
	if err != nil || !strings.Contains(string(out), `"hostname"`) {
		t.Errorf("podinfo answered %q, %v", out, err)
	}
}

// TestK3dFromFiles builds real local clusters from example files through 'apply', checks that
// exactly what each file asks for is installed (and nothing it doesn't) and that a hello
// world works (checkCluster), then deletes them and checks nothing is left: cluster,
// kubeconfig context, state. KUBECONFIG points to a throwaway file, so the user's own
// kubeconfig is never touched.
func TestK3dFromFiles(t *testing.T) {
	if os.Getenv("INTEGRATION") == "" {
		t.Skip("set INTEGRATION=1 to run real infra tests")
	}
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "kubeconfig"))
	ui.SetNonInteractive(true)

	ensureTools(t, "") // charts need helm; k3d brings its CNI

	tests := []struct {
		file        string   // an example, or "" for spec
		spec        string   // a file written here
		present     []string // pods that must be running (name contains)
		absent      []string // pods that must not exist
		gatewayCRDs bool     // k3s ships them with its traefik
		extra       func(t *testing.T, ctx context.Context, cs kubernetes.Interface)
	}{
		// podinfo from its chart, with the file's values: 2 replicas answering.
		{file: "local-helm.yaml", present: []string{"coredns", "traefik"}, gatewayCRDs: true, extra: checkPodinfo},
		{file: "custom-bare.yaml", present: []string{"coredns"}, absent: []string{"traefik", "svclb", "local-path", "metrics-server"}},
		{file: "local.yaml", present: []string{"coredns", "traefik", "svclb", "local-path-provisioner", "metrics-server"}, gatewayCRDs: true},
		// A worker with a role label (set through the API) and a taint; workloads still fit
		// on the server, which is schedulable when told so.
		{spec: `infrastructure: {provider: local}
  controlPlane: {schedulable: "true"}
  workers: [{name: infra, count: 1, labels: {node-role.kubernetes.io/infra: "", tier: infra},
             taints: [{key: dedicated, value: infra, effect: NoSchedule}]}]`,
			present: []string{"coredns"}, absent: []string{"traefik", "metrics-server"}},
	}
	for _, tt := range tests {
		t.Run(cmp.Or(tt.file, "workers"), func(t *testing.T) {
			var cfg *config.Config
			var err error
			if tt.file != "" {
				cfg, err = config.Load(filepath.Join("..", "examples", tt.file))
			} else {
				cfg, err = config.Parse([]byte("apiVersion: towproj.github.io/v1alpha1\nkind: Cluster\nmetadata: {name: workers}\nspec:\n  " + tt.spec + "\n"))
			}
			if err != nil {
				t.Fatal(err)
			}
			name := fmt.Sprintf("test-%s-%d", cmp.Or(strings.TrimSuffix(strings.TrimPrefix(tt.file, "custom-"), ".yaml"), "workers"), time.Now().Unix())
			cfg.Metadata.Name = name
			cfg.Spec.Kubernetes.Kubeconfig.Context = name

			orch := core.NewOrchestrator(false)
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
			ctx, cancel := context.WithTimeout(ctx, 8*time.Minute)
			defer cancel()

			if err := orch.Apply(ctx, cfg); err != nil {
				t.Fatalf("apply: %v", err)
			}
			if ok, err := kubecfg.HasContext(name); !ok || err != nil {
				t.Errorf("context %s missing from the kubeconfig: %v", name, err)
			}
			st, err := state.Load(name)
			if err != nil {
				t.Fatal(err)
			}
			checkCluster(t, ctx, clusterCheck{cfg: cfg, st: st, present: tt.present, absent: tt.absent})
			if tt.extra != nil {
				tt.extra(t, ctx, clientset(t, st))
			}

			// kubectl inside the server container: nothing to install on this machine.
			server := "k3d-" + name + "-server-0"
			kubectl := func(args ...string) string {
				out, _ := exec.CommandContext(ctx, "docker", append([]string{"exec", server, "kubectl"}, args...)...).CombinedOutput()
				return string(out)
			}
			if has := strings.Contains(kubectl("get", "crd", "-o", "name"), "gateway.networking.k8s.io"); has != tt.gatewayCRDs {
				t.Errorf("Gateway API CRDs present = %v, want %v", has, tt.gatewayCRDs)
			}

			if err := orch.Delete(ctx, name, yes); err != nil {
				t.Fatalf("delete: %v", err)
			}
			if ok, _ := kubecfg.HasContext(name); ok {
				t.Error("delete left the kubeconfig context behind")
			}
			if state.Exists(name) {
				t.Error("delete left the state behind")
			}
			if out, _ := exec.CommandContext(ctx, "k3d", "cluster", "list").Output(); strings.Contains(string(out), name) {
				t.Error("delete left the k3d cluster behind")
			}
		})
	}
}

// eventually polls check every 5 seconds until it holds or ctx ends (at most 3 minutes).
func eventually(t *testing.T, ctx context.Context, what string, check func() bool) {
	t.Helper()
	eventuallyWithin(t, ctx, 3*time.Minute, what, check)
}

func eventuallyWithin(t *testing.T, ctx context.Context, d time.Duration, what string, check func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	for !check() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(5 * time.Second):
		}
	}
}
