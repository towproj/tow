package core

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/providers"
)

func useHealth(t *testing.T, h kubeapi.Health, err error) {
	t.Helper()
	orig := clusterHealth
	clusterHealth = func(context.Context, string) (kubeapi.Health, error) { return h, err }
	t.Cleanup(func() { clusterHealth = orig })
}

// status reads the machines from the provider and Kubernetes through the kubeconfig: healthy
// only when all of it is.
func TestStatusReport(t *testing.T) {
	_, fake := useFakes(t)
	if err := NewOrchestrator(false).Apply(context.Background(), load(t, "homelab.yaml")); err != nil {
		t.Fatal(err)
	}
	fake.status = &providers.StatusResult{Healthy: true, State: "ready", ControlPlanes: "1/1", Workers: "2/2"}
	nodes := []kubeapi.Node{{Name: "cp", ControlPlane: true, Ready: true, Version: "v1.37.2+k3s1"}, {Name: "pi-1", Ready: true}, {Name: "pi-2", Ready: true}}
	useHealth(t, kubeapi.Health{Nodes: nodes}, nil)
	ctx := context.Background()

	r, err := NewOrchestrator(false).Report(ctx, "homelab")
	if err != nil || !r.Healthy() || r.Where != "your machines" || !strings.HasPrefix(r.Kubernetes, "k3s ") {
		t.Fatalf("report %+v, %v", r, err)
	}
	var out bytes.Buffer
	printReport(&out, r)
	for _, want := range []string{"✓ homelab: healthy, k3s ", " on your machines", "Machines: control planes 1/1, workers 2/2 running", "cp     control plane   Ready", "pi-2   worker"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if err := NewOrchestrator(false).Status(ctx, "homelab"); err != nil {
		t.Errorf("healthy: %v", err)
	}

	// A node down, a pod stuck: not healthy, and both named.
	nodes[2].Ready = false
	useHealth(t, kubeapi.Health{Nodes: nodes, Problems: []string{"apps/web-1 on pi-1: CrashLoopBackOff"}}, nil)
	r, _ = NewOrchestrator(false).Report(ctx, "homelab")
	out.Reset()
	printReport(&out, r)
	if r.Healthy() || !strings.Contains(out.String(), "pi-2   worker          NotReady") || !strings.Contains(out.String(), "Problems:\n    apps/web-1 on pi-1: CrashLoopBackOff") {
		t.Errorf("degraded:\n%s", out.String())
	}
	if err := NewOrchestrator(false).Status(ctx, "homelab"); err == nil || !strings.Contains(err.Error(), "isn't healthy") {
		t.Errorf("err = %v", err)
	}

	// The API doesn't answer: said, and not healthy.
	useHealth(t, kubeapi.Health{}, errors.New("dial tcp 192.168.1.10:6443: connect: no route to host"))
	r, _ = NewOrchestrator(false).Report(ctx, "homelab")
	out.Reset()
	printReport(&out, r)
	if r.Healthy() || !strings.Contains(out.String(), "the API doesn't answer: dial tcp") {
		t.Errorf("API down:\n%s", out.String())
	}

	if _, err := NewOrchestrator(false).Report(ctx, "nope"); err == nil || !strings.Contains(err.Error(), "no cluster named 'nope'") {
		t.Errorf("err = %v", err)
	}
}

// Nodes joined outside tow are named, and so are machines that never became nodes.
func TestStatusNamesNodesItDidntMake(t *testing.T) {
	useFakes(t)
	running(t, parse(t, "lab", fmt.Sprintf(lab, "", "")))
	useHealth(t, kubeapi.Health{Nodes: []kubeapi.Node{{Name: "cp", ControlPlane: true, Ready: true}, {Name: "gpu-1", Ready: true}}}, nil)
	r, err := NewOrchestrator(false).Report(context.Background(), "lab")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	printReport(&out, r)
	for _, want := range []string{"Not made by tow (joined outside tow?): gpu-1; tow leaves them alone", "Made by tow but not in the cluster: w1"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// A long list of stuck pods is cut, with how to see them all.
func TestStatusCutsProblems(t *testing.T) {
	r := Report{Name: "lab", Installed: true, Nodes: []kubeapi.Node{{Name: "a", Ready: true}}}
	for i := range maxProblems + 3 {
		r.Problems = append(r.Problems, "apps/p"+string(rune('a'+i))+": failed")
	}
	var out bytes.Buffer
	printReport(&out, r)
	if !strings.Contains(out.String(), "… and 3 more (kubectl get pods -A)") || strings.Count(out.String(), ": failed") != maxProblems {
		t.Errorf("output:\n%s", out.String())
	}
}
