package aws

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/providers/tofu/cloud"
)

var threeZones = []string{"eu-west-3a", "eu-west-3b", "eu-west-3c"}

// A cluster whose machines all have a zone asks AWS nothing: Cloud{} has no credentials
// here, so any call would fail. (The placement itself is cloud.Placement's, tested there.)
func TestPlaceAsksNothingWhenAllPlaced(t *testing.T) {
	machines := cloud.Machines("demo", []providers.Pool{{Name: "control-plane", Role: "control-plane", Count: 2}})
	last := &cloud.Inputs{Zones: threeZones, Machines: []cloud.Machine{{Name: "demo-cp-0", Zone: "eu-west-3c"}, {Name: "demo-cp-1", Zone: "eu-west-3a"}}}
	zones, err := Cloud{}.place(context.Background(), "eu-west-3", machines, last)
	if err != nil || !slices.Equal(zones, threeZones) {
		t.Fatalf("zones = %v, %v", zones, err)
	}
	if got := fmt.Sprintf("%s %s", machines[0].Zone, machines[1].Zone); got != "eu-west-3c eu-west-3a" {
		t.Errorf("zones = %s", got)
	}

	// The module gets the machines' system: the request's, or Ubuntu.
	spec := providers.Spec{Name: "demo", Machines: &providers.MachineRequest{Region: "eu-west-3", OS: "almalinux-9"}}
	vars, err := Cloud{}.Vars(context.Background(), spec, machines, last)
	if err != nil || vars["os"] != "almalinux-9" {
		t.Errorf("os = %v, %v", vars["os"], err)
	}
	spec.Machines.OS = ""
	if vars, _ := (Cloud{}).Vars(context.Background(), spec, machines, last); vars["os"] != "ubuntu-24.04" {
		t.Errorf("default os = %v", vars["os"])
	}
}
