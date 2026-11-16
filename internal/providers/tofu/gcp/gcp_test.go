package gcp

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"golang.org/x/oauth2/google"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

// fakeGoogle answers the Compute Engine and IAM calls tow makes, from a map of paths to
// JSON bodies; any other path is a 404, as Google answers for what doesn't exist.
func fakeGoogle(t *testing.T, answers map[string]string) Cloud {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if r.Method == http.MethodGet {
			key = r.URL.Path
		}
		body, ok := answers[key]
		switch {
		case !ok:
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error": {"code": 404, "message": "The resource '%s' was not found"}}`, r.URL.Path)
		case strings.HasPrefix(body, "403 "):
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprintf(w, `{"error": {"code": 403, "message": %q}}`, strings.TrimPrefix(body, "403 "))
		default:
			fmt.Fprint(w, body)
		}
	}))
	t.Cleanup(srv.Close)
	return Cloud{newAPI: func(_ context.Context, project string) (*api, error) {
		return &api{project: project, client: srv.Client(), creds: &google.Credentials{JSON: []byte(`{"client_email": "ci@demo-project.iam.gserviceaccount.com"}`)},
			compute: srv.URL + "/compute/v1", iam: srv.URL + "/iam/v1", tokenInfo: srv.URL + "/tokeninfo"}, nil
	}}
}

const p = "/compute/v1/projects/demo-project"

// europeWest1: three zones, e2 everywhere, the ARM t2a in one zone only.
func europeWest1() map[string]string {
	a := map[string]string{
		p + "/regions/europe-west1":                             `{"zones": ["https://www.googleapis.com/compute/v1/projects/demo-project/zones/europe-west1-b", "x/zones/europe-west1-c", "x/zones/europe-west1-d"]}`,
		p + "/zones/europe-west1-b/machineTypes/t2a-standard-2": `{"name": "t2a-standard-2", "guestCpus": 2, "memoryMb": 8192, "architecture": "ARM64"}`,
	}
	for _, z := range []string{"b", "c", "d"} {
		a[p+"/zones/europe-west1-"+z+"/machineTypes/e2-medium"] = `{"name": "e2-medium", "guestCpus": 2, "memoryMb": 4096, "architecture": "X86_64"}`
		a[p+"/zones/europe-west1-"+z+"/machineTypes/e2-small"] = `{"name": "e2-small", "guestCpus": 2, "memoryMb": 2048}`
	}
	return a
}

func request(os string, pools ...providers.Pool) *providers.MachineRequest {
	return &providers.MachineRequest{Cloud: "gcp", Project: "demo-project", Region: "europe-west1", OS: os, Pools: pools}
}

// A new cluster spreads over the zones offering its machine types; each type gets its CPU
// architecture, for the image.
func TestVars(t *testing.T) {
	c := fakeGoogle(t, europeWest1())
	m := request("", providers.Pool{Name: "control-plane", Role: "control-plane", Count: 1},
		providers.Pool{Name: "arm", Role: "worker", Count: 2, InstanceType: "t2a-standard-2"})
	m.Network = &providers.StaticNetwork{Subnet: "10.200.0.0/16"}
	spec := providers.Spec{Name: "demo", Engine: "k3s", Storage: []string{"gcp-pd"}, Machines: m}
	machines := cloud.Machines("demo", m.Pools)
	vars, err := c.Vars(context.Background(), spec, machines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(vars["zones"]); got != "[europe-west1-b]" {
		t.Errorf("zones = %s: only b offers both types", got)
	}
	if got := fmt.Sprint(vars["machine_type_archs"]); got != "map[e2-medium:amd64 t2a-standard-2:arm64]" {
		t.Errorf("archs = %s", got)
	}
	if vars["project"] != "demo-project" || vars["pd_csi"] != true || vars["os"] != "ubuntu-24.04" || vars["subnet_cidr"] != "10.200.0.0/16" || vars["default_instance_type"] != "e2-medium" {
		t.Errorf("vars = %v", vars)
	}

	// A running cluster keeps its zones and places; its types are still described.
	prev := &cloud.Inputs{Zones: []string{"europe-west1-b"}, Machines: machines}
	again := cloud.Machines("demo", m.Pools)
	if vars, err = c.Vars(context.Background(), spec, again, prev); err != nil || fmt.Sprint(vars["machine_type_archs"]) != "map[e2-medium:amd64 t2a-standard-2:arm64]" {
		t.Errorf("again: %v, %v", vars, err)
	}

	m.Region = "europe-west9"
	if _, err := c.Vars(context.Background(), spec, cloud.Machines("demo", m.Pools), nil); err == nil || !strings.Contains(err.Error(), "region europe-west9 doesn't exist for project demo-project") {
		t.Errorf("err = %v", err)
	}
}

// Machine types are checked against the distribution's minimum before anything is created:
// AlmaLinux's kdump makes a kubeadm control plane need more than 2 GB.
func TestBeforeCreate(t *testing.T) {
	c := fakeGoogle(t, europeWest1())
	ok := request("ubuntu-24.04", providers.Pool{Name: "control-plane", Role: "control-plane", Count: 1, InstanceType: "e2-small"})
	if err := c.BeforeCreate(context.Background(), providers.Spec{Engine: "kubeadm", Machines: ok}, nil); err != nil {
		t.Errorf("2 vCPUs and 2 GB fit a kubeadm control plane on Ubuntu: %v", err)
	}
	alma := request("almalinux-9", providers.Pool{Name: "control-plane", Role: "control-plane", Count: 1, InstanceType: "e2-small"},
		providers.Pool{Name: "big", Role: "worker", Count: 1, InstanceType: "n9-huge"})
	err := c.BeforeCreate(context.Background(), providers.Spec{Engine: "kubeadm", Machines: alma}, nil)
	for _, want := range []string{"pool control-plane: e2-small has 2 vCPUs and 2048 MiB; a kubeadm control plane on almalinux-9", "needs at least 2 vCPUs and 3072 MiB",
		"pool big: machine type n9-huge is not offered in any zone of europe-west1"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestIdentity(t *testing.T) {
	a := europeWest1()
	a[p] = `{"name": "demo-project"}`
	got, err := fakeGoogle(t, a).Identity(context.Background(), request(""))
	if err != nil || got != "GCP project demo-project as ci@demo-project.iam.gserviceaccount.com" {
		t.Errorf("identity = %q, %v", got, err)
	}
	a[p] = "403 Compute Engine API has not been used in project 913 before or it is disabled."
	_, err = fakeGoogle(t, a).Identity(context.Background(), request(""))
	for _, want := range []string{"could not reach Compute Engine in GCP project demo-project: Compute Engine API has not been used", "gcloud services enable compute.googleapis.com iam.googleapis.com --project demo-project", "gcloud auth application-default login"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

// The cluster's machines by its label, another cluster's left alone; states as AWS's words.
func TestInstances(t *testing.T) {
	a := europeWest1()
	a[p+"/aggregated/instances"] = `{"items": {
		"zones/europe-west1-b": {"instances": [
			{"name": "demo-cp-0", "status": "RUNNING", "labels": {"tow-cluster": "demo", "tow-role": "control-plane"}},
			{"name": "demo-general-0", "status": "TERMINATED", "labels": {"tow-cluster": "demo", "tow-role": "worker"}},
			{"name": "demo2-cp-0", "status": "RUNNING", "labels": {"tow-cluster": "demo2", "tow-role": "control-plane"}}]},
		"zones/europe-west1-c": {"warning": {"code": "NO_RESULTS_ON_PAGE"}}}}`
	got, err := fakeGoogle(t, a).Instances(context.Background(), request(""), "demo")
	if err != nil || fmt.Sprint(got) != "[{demo-cp-0 control-plane running} {demo-general-0 worker stopped}]" {
		t.Errorf("instances = %v, %v", got, err)
	}
}

// After a delete: every kind of resource the module makes is looked for, by label or by name;
// another cluster's whose name starts the same is not this one's, and a deleted role is gone.
func TestLeftovers(t *testing.T) {
	a := europeWest1()
	a[p+"/aggregated/instances"] = `{"items": {}}`
	a[p+"/aggregated/disks"] = `{"items": {"zones/europe-west1-b": {"disks": [{"name": "pvc-1", "labels": {"tow-cluster": "demo"}}, {"name": "other", "labels": {"tow-cluster": "demo2"}}]}}}`
	a[p+"/regions/europe-west1/forwardingRules"] = `{"items": [{"name": "demo-api"}, {"name": "demo2-api"}]}`
	a[p+"/regions/europe-west1/addresses"] = `{}`
	a[p+"/regions/europe-west1/backendServices"] = `{"items": [{"name": "demo-api-nodes"}]}`
	a[p+"/regions/europe-west1/healthChecks"] = `{}`
	a[p+"/aggregated/instanceGroups"] = `{"items": {"zones/europe-west1-c": {"instanceGroups": [{"name": "demo-cp-1"}, {"name": "demo-cp-x"}, {"name": "demo2-cp-0"}]}}}`
	a[p+"/global/firewalls"] = `{"items": [{"name": "demo-admin", "network": "x/global/networks/demo-vpc"}, {"name": "default-allow-ssh", "network": "x/global/networks/default"}]}`
	a[p+"/regions/europe-west1/subnetworks"] = `{"items": [{"name": "default", "network": "x/global/networks/default"}]}`
	a[p+"/global/networks/demo-vpc"] = `{"name": "demo-vpc"}`
	a["/iam/v1/projects/demo-project/roles/tow_demo_pdcsi"] = `{"name": "x", "deleted": true}`
	left, err := fakeGoogle(t, a).Leftovers(context.Background(), request(""), "demo")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"disk pvc-1", "forwarding rule demo-api", "backend service demo-api-nodes", "instance group demo-cp-1", "firewall rule demo-admin", "network demo-vpc"}
	if !slices.Equal(left, want) {
		t.Errorf("left = %v\nwant %v", left, want)
	}

	a[p+"/global/networks/demo-vpc"] = "403 Permission denied"
	if _, err := fakeGoogle(t, a).Leftovers(context.Background(), request(""), "demo"); err == nil {
		t.Error("an error that isn't 'not found' must not count as gone")
	}
}

func TestNames(t *testing.T) {
	if got := pdCSIAccount("p", strings.Repeat("a", 30)); got != strings.Repeat("a", 24)+"-pdcsi@p.iam.gserviceaccount.com" {
		t.Errorf("account = %s", got)
	}
	if got := pdCSIRole("my-lab"); got != "tow_my_lab_pdcsi" {
		t.Errorf("role = %s", got)
	}
}
