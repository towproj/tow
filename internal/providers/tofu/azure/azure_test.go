package azure

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

const sub = "00000000-0000-0000-0000-000000000001"

// fakeToken is a credential whose token names a user, as Entra ID's do.
type fakeToken struct{}

func (fakeToken) GetToken(context.Context, policy.TokenRequestOptions) (azcore.AccessToken, error) {
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"upn": "me@example.com"}`))
	return azcore.AccessToken{Token: "e30." + claims + ".sig", ExpiresOn: time.Now().Add(time.Hour)}, nil
}

// fakeARM answers Resource Manager's calls from a map of paths (without the query) to JSON
// bodies; any other path is a 404, as Azure answers for what doesn't exist.
func fakeARM(t *testing.T, answers map[string]string) Cloud {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer e30."+base64.RawURLEncoding.EncodeToString([]byte(`{"upn": "me@example.com"}`))+".sig" {
			t.Errorf("%s: no token", r.URL.Path)
		}
		if r.URL.Query().Get("api-version") == "" {
			t.Errorf("%s: no api-version", r.URL.Path)
		}
		// As Azure answers a resource group's VM list asked for power states without a scale
		// set filter.
		if strings.HasSuffix(r.URL.Path, "/virtualMachines") && r.URL.Query().Get("$expand") == "instanceView" && r.URL.Query().Get("$filter") == "" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error": {"code": "BadRequest", "message": "Expand Instance View is only supported when Virtual Machine Scale Set resource filter is applied"}}`)
			return
		}
		key := r.URL.Path
		if r.Method != http.MethodGet {
			key = r.Method + " " + key
		}
		body, ok := answers[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, `{"error": {"code": "ResourceNotFound", "message": "%s was not found"}}`, r.URL.Path)
			return
		}
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return Cloud{newAPI: func(subscription string) (*api, error) {
		return &api{subscription: subscription, cred: fakeToken{}, client: srv.Client(), arm: srv.URL}, nil
	}}
}

// westeurope: B2s in three zones, B1s in zones 1 and 2 (3 restricted for this subscription),
// an Arm size, and one restricted in the whole location.
func westeurope() map[string]string {
	return map[string]string{
		"/subscriptions/" + sub + "/providers/Microsoft.Compute/skus": `{"value": [
			{"resourceType": "virtualMachines", "name": "Standard_B2s_v2", "locationInfo": [{"location": "westeurope", "zones": ["3", "1", "2"]}],
			 "capabilities": [{"name": "vCPUs", "value": "2"}, {"name": "MemoryGB", "value": "4"}, {"name": "CpuArchitectureType", "value": "x64"}]},
			{"resourceType": "virtualMachines", "name": "Standard_B1s", "locationInfo": [{"location": "westeurope", "zones": ["1", "2", "3"]}],
			 "capabilities": [{"name": "vCPUs", "value": "1"}, {"name": "MemoryGB", "value": "1"}],
			 "restrictions": [{"type": "Zone", "values": ["westeurope"], "restrictionInfo": {"zones": ["3"]}}]},
			{"resourceType": "virtualMachines", "name": "Standard_D2ps_v5", "locationInfo": [{"location": "westeurope", "zones": ["1"]}],
			 "capabilities": [{"name": "vCPUs", "value": "2"}, {"name": "MemoryGB", "value": "8"}, {"name": "CpuArchitectureType", "value": "Arm64"}]},
			{"resourceType": "virtualMachines", "name": "Standard_M416", "locationInfo": [{"location": "westeurope", "zones": ["1"]}],
			 "restrictions": [{"type": "Location", "values": ["westeurope"]}]},
			{"resourceType": "disks", "name": "Premium_LRS"}]}`,
	}
}

func request(os string, pools ...providers.Pool) *providers.MachineRequest {
	return &providers.MachineRequest{Cloud: "azure", Subscription: sub, Region: "westeurope", OS: os, Pools: pools}
}

// A new cluster spreads over the zones offering its VM sizes to this subscription; each size
// gets its CPU architecture, for the image.
func TestVars(t *testing.T) {
	c := fakeARM(t, westeurope())
	m := request("", providers.Pool{Name: "control-plane", Role: "control-plane", Count: 3},
		providers.Pool{Name: "small", Role: "worker", Count: 1, InstanceType: "Standard_B1s"})
	spec := providers.Spec{Name: "demo", Engine: "k3s", Storage: []string{"azure-disk"}, Machines: m}
	machines := cloud.Machines("demo", m.Pools)
	vars, err := c.Vars(context.Background(), spec, machines, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(vars["zones"]); got != "[1 2]" {
		t.Errorf("zones = %s: B1s isn't offered in 3 to this subscription", got)
	}
	if got := fmt.Sprint(vars["machine_type_archs"]); got != "map[Standard_B1s:amd64 Standard_B2s_v2:amd64]" {
		t.Errorf("archs = %s", got)
	}
	if vars["subscription"] != sub || vars["disk_csi"] != true || vars["os"] != "ubuntu-24.04" || vars["default_instance_type"] != "Standard_B2s_v2" {
		t.Errorf("vars = %v", vars)
	}
	if got := fmt.Sprint(machines[0].Zone, machines[1].Zone, machines[2].Zone); got != "121" {
		t.Errorf("control planes in zones %s", got)
	}

	m.Pools[1].InstanceType = "Standard_M416"
	if _, err := c.Vars(context.Background(), spec, cloud.Machines("demo", m.Pools), nil); err == nil || !strings.Contains(err.Error(), "VM size Standard_M416 is not offered in any zone of westeurope") {
		t.Errorf("err = %v", err)
	}
}

// Azure lists a size twice, one entry restricted in every zone: the other one's zones count,
// whichever comes last.
func TestSizesListedTwice(t *testing.T) {
	good := `{"resourceType": "virtualMachines", "name": "Standard_B2s_v2", "locationInfo": [{"location": "SwedenCentral", "zones": ["2", "1", "3"]}],
		"capabilities": [{"name": "vCPUs", "value": "2"}, {"name": "MemoryGB", "value": "4"}]}`
	bad := `{"resourceType": "virtualMachines", "name": "Standard_B2s_v2", "locationInfo": [{"location": "SwedenCentral", "zones": ["1", "2", "3"]}],
		"capabilities": [{"name": "vCPUs", "value": "2"}, {"name": "MemoryGB", "value": "4"}],
		"restrictions": [{"type": "Zone", "values": ["swedencentral"], "restrictionInfo": {"zones": ["1", "2", "3"]}}]}`
	for _, order := range [][2]string{{good, bad}, {bad, good}} {
		c := fakeARM(t, map[string]string{"/subscriptions/" + sub + "/providers/Microsoft.Compute/skus": `{"value": [` + order[0] + `, ` + order[1] + `]}`})
		a, _ := c.newAPI(sub)
		info, err := a.sizes(context.Background(), "swedencentral")
		if err != nil || fmt.Sprint(info["Standard_B2s_v2"].Zones) != "[1 2 3]" {
			t.Errorf("zones = %v, %v", info["Standard_B2s_v2"].Zones, err)
		}
	}
}

// A location without zones: every machine goes in none.
func TestVarsWithoutZones(t *testing.T) {
	c := fakeARM(t, map[string]string{"/subscriptions/" + sub + "/providers/Microsoft.Compute/skus": `{"value": [
		{"resourceType": "virtualMachines", "name": "Standard_B2s_v2", "locationInfo": [{"location": "westeurope", "zones": []}],
		 "capabilities": [{"name": "vCPUs", "value": "2"}, {"name": "MemoryGB", "value": "4"}]}]}`})
	m := request("", providers.Pool{Name: "control-plane", Role: "control-plane", Count: 2})
	machines := cloud.Machines("demo", m.Pools)
	vars, err := c.Vars(context.Background(), providers.Spec{Name: "demo", Machines: m}, machines, nil)
	if err != nil || fmt.Sprintf("%q", vars["zones"]) != `[""]` || machines[0].Zone != "" || machines[1].Zone != "" {
		t.Errorf("zones = %v, machines %+v, %v", vars["zones"], machines, err)
	}
}

func TestBeforeCreate(t *testing.T) {
	c := fakeARM(t, westeurope())
	ok := request("almalinux-9", providers.Pool{Name: "control-plane", Role: "control-plane", Count: 1},
		providers.Pool{Name: "arm", Role: "worker", Count: 1, InstanceType: "Standard_D2ps_v5"})
	if err := c.BeforeCreate(context.Background(), providers.Spec{Engine: "kubeadm", Machines: ok}, nil); err != nil {
		t.Errorf("B2s fits a kubeadm control plane on AlmaLinux, D2ps_v5 an Arm worker: %v", err)
	}
	bad := request("almalinux-10", providers.Pool{Name: "control-plane", Role: "control-plane", Count: 1, InstanceType: "Standard_B1s", DiskGB: 20},
		providers.Pool{Name: "arm", Role: "worker", Count: 1, InstanceType: "Standard_D2ps_v5"},
		providers.Pool{Name: "typo", Role: "worker", Count: 1, InstanceType: "Standard_B2z"})
	err := c.BeforeCreate(context.Background(), providers.Spec{Engine: "kubeadm", Machines: bad}, nil)
	for _, want := range []string{"pool control-plane: Standard_B1s has 1 vCPUs and 1024 MiB; a kubeadm control plane on almalinux-10",
		"pool control-plane: a 20 GB disk is smaller than Azure's Ubuntu and AlmaLinux images (30 GB)",
		"pool typo: VM size Standard_B2z is not offered in westeurope to this subscription"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want %q", err, want)
		}
	}
}

func TestIdentity(t *testing.T) {
	a := westeurope()
	a["/subscriptions/"+sub] = `{"displayName": "Free Trial", "state": "Enabled"}`
	got, err := fakeARM(t, a).Identity(context.Background(), request(""))
	if err != nil || got != "Azure subscription Free Trial ("+sub+") as me@example.com" {
		t.Errorf("identity = %q, %v", got, err)
	}
	a["/subscriptions/"+sub] = `{"displayName": "Free Trial", "state": "Disabled"}`
	if _, err := fakeARM(t, a).Identity(context.Background(), request("")); err == nil || !cloud.IsPermanent(err) || !strings.Contains(err.Error(), "is Disabled") {
		t.Errorf("a disabled subscription is a permanent error: %v", err)
	}
	delete(a, "/subscriptions/"+sub)
	if _, err := fakeARM(t, a).Identity(context.Background(), request("")); err == nil || !strings.Contains(err.Error(), "az login") {
		t.Errorf("err = %v", err)
	}
}

func TestInstancesAndLeftovers(t *testing.T) {
	rg := "/subscriptions/" + sub + "/resourceGroups/tow-demo"
	a := westeurope()
	vms := rg + "/providers/Microsoft.Compute/virtualMachines"
	a[vms] = `{"value": [
		{"name": "demo-cp-0", "tags": {"tow-cluster": "demo", "tow-role": "control-plane"}},
		{"name": "demo-general-0", "tags": {"tow-cluster": "demo", "tow-role": "worker"}},
		{"name": "demo-general-1", "tags": {"tow-cluster": "demo", "tow-role": "worker"}},
		{"name": "someone-elses", "tags": {}}],
		"nextLink": ""}`
	a[vms+"/demo-cp-0/instanceView"] = `{"statuses": [{"code": "ProvisioningState/succeeded"}, {"code": "PowerState/running"}]}`
	a[vms+"/demo-general-0/instanceView"] = `{"statuses": [{"code": "PowerState/deallocated"}]}`
	// demo-general-1 is deleted between the list and its instance view: gone.
	got, err := fakeARM(t, a).Instances(context.Background(), request(""), "demo")
	if err != nil || fmt.Sprint(got) != "[{demo-cp-0 control-plane running} {demo-general-0 worker stopped}]" {
		t.Errorf("instances = %v, %v", got, err)
	}
	if got, err := fakeARM(t, westeurope()).Instances(context.Background(), request(""), "demo"); err != nil || len(got) != 0 {
		t.Errorf("no resource group, no machines: %v, %v", got, err)
	}

	// Leftovers: the resource group and what's in it; nothing once it's gone.
	a["/subscriptions/"+sub+"/resourcegroups/tow-demo"] = `{"name": "tow-demo"}`
	a["/subscriptions/"+sub+"/resourcegroups/tow-demo/resources"] = `{"value": [{"name": "pvc-1", "type": "Microsoft.Compute/disks"}]}`
	left, err := fakeARM(t, a).Leftovers(context.Background(), request(""), "demo")
	if want := []string{"resource group tow-demo", "Microsoft.Compute/disks pvc-1"}; err != nil || !slices.Equal(left, want) {
		t.Errorf("left = %v, %v", left, err)
	}
	if left, err := fakeARM(t, westeurope()).Leftovers(context.Background(), request(""), "demo"); err != nil || len(left) != 0 {
		t.Errorf("gone = %v, %v", left, err)
	}
}

func TestPower(t *testing.T) {
	rg := "/subscriptions/" + sub + "/resourceGroups/tow-demo"
	a := map[string]string{
		"POST " + rg + "/providers/Microsoft.Compute/virtualMachines/demo-cp-0/start": ``,
		rg + "/providers/Microsoft.Compute/virtualMachines/demo-cp-0/instanceView":    `{"statuses": [{"code": "PowerState/running"}]}`,
		rg + "/providers/Microsoft.Network/publicIPAddresses/demo-cp-0":               `{"properties": {"ipAddress": "20.1.2.3"}}`,
	}
	ip, err := fakeARM(t, a).PowerOn(context.Background(), sub, "demo", "demo-cp-0")
	if err != nil || ip != "20.1.2.3" {
		t.Errorf("ip = %q, %v", ip, err)
	}
}
