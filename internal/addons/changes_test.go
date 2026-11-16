package addons

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/config"
)

// exitError is a program that exited with a code, as *exec.ExitError says it.
type exitError int

func (e exitError) Error() string { return "exit status" }
func (e exitError) ExitCode() int { return int(e) }

const deployAndService = `{"kind": "List", "apiVersion": "v1", "items": [
	{"apiVersion": "apps/v1", "kind": "Deployment", "metadata": {"name": "web", "namespace": "shop"}},
	{"apiVersion": "v1", "kind": "Service", "metadata": {"name": "web", "namespace": "shop"}}]}`

// kubectl prints one object, a List, or several one after the other: all are read.
func TestObjectsIn(t *testing.T) {
	ns := `{"apiVersion": "v1", "kind": "Namespace", "metadata": {"name": "shop"}}`
	objs, err := objectsIn(ns + "\n" + deployAndService)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, o := range objs {
		got = append(got, o.String())
	}
	if want := []string{"namespace/shop", "deployment/web in shop", "service/web in shop"}; !slices.Equal(got, want) {
		t.Errorf("objects = %v, want %v", got, want)
	}
	if m := Missing(objs, objs[1:2]); len(m) != 2 || m[0].Kind != "Namespace" || m[1].Kind != "Service" {
		t.Errorf("missing = %v", m)
	}
}

func TestCheckApplyAndDeleteManifest(t *testing.T) {
	var calls []call
	diffCode := 1
	orig := run
	run = func(_ context.Context, program string, args []string, stdin string) (string, string, error) {
		calls = append(calls, call{program: program, args: args, stdin: stdin})
		switch {
		case args[0] == "diff":
			if diffCode != 0 {
				return "-  replicas: 1\n+  replicas: 2", "", exitError(diffCode)
			}
		case args[0] == "apply" && slices.Contains(args, "json"):
			return deployAndService, "", nil
		}
		return "", "", nil
	}
	t.Cleanup(func() { run = orig })
	dir := t.TempDir()
	kc := filepath.Join(dir, "kubeconfig")
	os.WriteFile(kc, []byte(testKubeconfig), 0o600)
	o := Options{Kubectl: "kubectl", Helm: "helm", Kubeconfig: kc, Dir: dir}
	m := config.Manifest{Path: "https://example.com/shop.yaml", Namespace: "shop"}
	ctx := context.Background()

	differs, objs, err := o.CheckManifest(ctx, m)
	if err != nil || !differs || len(objs) != 2 {
		t.Fatalf("differs %v, objects %v, %v", differs, objs, err)
	}
	for _, c := range calls {
		if c.args[0] == "apply" && !slices.Contains(c.args, "--dry-run=server") {
			t.Errorf("a check changes nothing: %v", c.args)
		}
	}
	diffCode = 0
	if differs, _, _ := o.CheckManifest(ctx, m); differs {
		t.Error("kubectl diff found nothing: the manifest doesn't differ")
	}
	diffCode = 2
	if _, _, err := o.CheckManifest(ctx, m); err == nil {
		t.Error("kubectl diff failing (exit 2) is an error, not a difference")
	}

	calls = nil
	objs, err = o.ApplyManifest(ctx, m)
	if err != nil || len(objs) != 2 || objs[0].Namespace != "shop" {
		t.Fatalf("applied %v, %v", objs, err)
	}
	if len(calls) != 2 || !strings.Contains(calls[0].stdin, "kind: Namespace") || calls[1].args[0] != "apply" {
		t.Errorf("calls = %v", calls)
	}

	calls = nil
	if err := o.DeleteObjects(ctx, objs); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !slices.Contains(calls[0].args, "--ignore-not-found") ||
		!strings.Contains(calls[0].stdin, "kind: Deployment\nmetadata:\n  name: \"web\"\n  namespace: \"shop\"") {
		t.Errorf("delete: %v", calls)
	}
}

func TestReleases(t *testing.T) {
	orig := run
	run = func(_ context.Context, _ string, args []string, _ string) (string, string, error) {
		// Every status named (Helm 4 has no --all; Helm 3 shows only deployed and failed
		// ones without it), as checked on v3.22.1 and v4.3.1.
		for _, f := range []string{"--all-namespaces", "--deployed", "--failed", "--pending", "--superseded", "--uninstalling"} {
			if !slices.Contains(args, f) {
				t.Errorf("helm list without %s: %v", f, args)
			}
		}
		if slices.Contains(args, "--all") {
			t.Errorf("Helm 4 refuses --all: %v", args)
		}
		return `[{"name": "podinfo", "namespace": "apps", "status": "deployed"}]`, "", nil
	}
	t.Cleanup(func() { run = orig })
	ids, err := Options{Helm: "helm"}.Releases(context.Background())
	if err != nil || !slices.Equal(ids, []string{"apps/podinfo"}) {
		t.Errorf("releases = %v, %v", ids, err)
	}
}
