package cloud

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/towproj/tow/internal/providers"
)

var threeZones = []string{"eu-west-3a", "eu-west-3b", "eu-west-3c"}

const defaultSize = "t3.medium"

// awsLike places as the AWS target does; asking the cloud fails the test.
func awsLike(t *testing.T) Placement {
	return Placement{Region: "eu-west-3", DefaultSize: defaultSize, Kind: "instance type",
		Offerings: func(context.Context, []string) (map[string][]string, error) {
			t.Error("the cloud was asked")
			return nil, errors.New("asked")
		}}
}

func offeredEverywhere(sizes ...string) map[string][]string {
	out := map[string][]string{}
	for _, s := range sizes {
		out[s] = threeZones
	}
	return out
}

func zonesOf(machines []Machine) map[string]string {
	out := map[string]string{}
	for _, m := range machines {
		out[m.Name] = m.Zone
	}
	return out
}

// A new cluster is spread as machines always were: machine i in zone i mod n, control
// planes first, over every shape of cluster.
func TestPlaceZonesNewClusterIsRoundRobin(t *testing.T) {
	p := awsLike(t)
	for zones := 1; zones <= 3; zones++ {
		for cps := 1; cps <= 3; cps++ {
			for a := 0; a <= 4; a++ {
				for b := 0; b <= 4; b++ {
					pools := []providers.Pool{{Name: "control-plane", Role: "control-plane", Count: cps}}
					if a > 0 {
						pools = append(pools, providers.Pool{Name: "a", Role: "worker", Count: a})
					}
					if b > 0 {
						pools = append(pools, providers.Pool{Name: "b", Role: "worker", Count: b})
					}
					machines := Machines("demo", pools)
					if err := p.placeZones(machines, nil, threeZones[:zones], offeredEverywhere(defaultSize)); err != nil {
						t.Fatal(err)
					}
					for i, m := range machines {
						if want := threeZones[i%zones]; m.Zone != want {
							t.Errorf("%d zones, %d control planes, pools of %d and %d: %s in %s, want %s", zones, cps, a, b, m.Name, m.Zone, want)
						}
					}
				}
			}
		}
	}
}

// Adding or removing machines moves none of the others; a new one goes where its own pool
// has the fewest machines.
func TestPlaceZonesKeepsMachines(t *testing.T) {
	p := awsLike(t)
	pools := []providers.Pool{
		{Name: "control-plane", Role: "control-plane", Count: 1},
		{Name: "general", Role: "worker", Count: 2},
	}
	first := Machines("demo", pools)
	if err := p.placeZones(first, nil, threeZones, offeredEverywhere(defaultSize)); err != nil {
		t.Fatal(err)
	}
	before := zonesOf(first) // cp-0 in a, general-0 in b, general-1 in c
	last := &Inputs{Machines: first}

	// One more control plane: the workers stay where they are, which numbering them all in
	// one list didn't do.
	pools[0].Count = 2
	grown := Machines("demo", pools)
	if err := p.placeZones(grown, last.Placed(), threeZones, offeredEverywhere(defaultSize)); err != nil {
		t.Fatal(err)
	}
	after := zonesOf(grown)
	for name, zone := range before {
		if after[name] != zone {
			t.Errorf("%s moved from %s to %s", name, zone, after[name])
		}
	}
	if after["demo-cp-1"] != "eu-west-3b" {
		t.Errorf("new control plane in %s, want eu-west-3b: the first zone with no control plane", after["demo-cp-1"])
	}

	// A third worker in a pool spread over b and c goes to a.
	pools[0].Count, pools[1].Count = 1, 3
	grown = Machines("demo", pools)
	if err := p.placeZones(grown, last.Placed(), threeZones, offeredEverywhere(defaultSize)); err != nil {
		t.Fatal(err)
	}
	if got := zonesOf(grown)["demo-general-2"]; got != "eu-west-3a" {
		t.Errorf("new worker in %s, want eu-west-3a", got)
	}
}

// A pool's machines go only where the cloud offers their size.
func TestPlaceZonesFollowsOfferings(t *testing.T) {
	p := awsLike(t)
	pools := []providers.Pool{
		{Name: "control-plane", Role: "control-plane", Count: 1},
		{Name: "gpu", Role: "worker", Count: 2, InstanceType: "g5.xlarge"},
	}
	offers := offeredEverywhere(defaultSize)
	offers["g5.xlarge"] = []string{"eu-west-3c"}
	machines := Machines("demo", pools)
	if err := p.placeZones(machines, nil, threeZones, offers); err != nil {
		t.Fatal(err)
	}
	if got := zonesOf(machines); got["demo-gpu-0"] != "eu-west-3c" || got["demo-gpu-1"] != "eu-west-3c" {
		t.Errorf("zones = %v", got)
	}

	offers["g5.xlarge"] = []string{"eu-west-3d"}
	err := p.placeZones(Machines("demo", pools), nil, threeZones, offers)
	if err == nil || !strings.Contains(err.Error(), "pool gpu: instance type g5.xlarge isn't offered in any of the cluster's zones (eu-west-3a, eu-west-3b, eu-west-3c)") {
		t.Errorf("err = %v", err)
	}
}

func TestCommonZones(t *testing.T) {
	p := awsLike(t)
	offers := map[string][]string{
		"t3.small":  {"eu-west-3c", "eu-west-3a", "eu-west-3b", "eu-west-3d"},
		"m7g.large": {"eu-west-3d", "eu-west-3b", "eu-west-3c"},
	}
	got, err := p.commonZones([]string{"m7g.large", "t3.small"}, offers)
	if want := []string{"eu-west-3b", "eu-west-3c", "eu-west-3d"}; err != nil || !slices.Equal(got, want) {
		t.Errorf("zones = %v, %v; want %v", got, err, want)
	}
	if got, _ := p.commonZones([]string{"t3.small"}, offers); !slices.Equal(got, []string{"eu-west-3a", "eu-west-3b", "eu-west-3c"}) {
		t.Errorf("at most three zones, in name order: %v", got)
	}

	offers["x1.tiny"] = []string{"eu-west-3a"}
	for sizes, want := range map[string]string{
		"t3.smal":           "instance type t3.smal is not offered in any zone of eu-west-3; check its spelling",
		"m7g.large,x1.tiny": "no zone of eu-west-3 offers all of these instance types together: m7g.large, x1.tiny",
	} {
		if _, err := p.commonZones(strings.Split(sizes, ","), offers); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", sizes, err, want)
		}
	}
}

// A cluster whose machines all have a zone asks the cloud nothing.
func TestPlaceAsksNothingWhenAllPlaced(t *testing.T) {
	machines := Machines("demo", []providers.Pool{{Name: "control-plane", Role: "control-plane", Count: 2}})
	last := &Inputs{Zones: threeZones, Machines: []Machine{{Name: "demo-cp-0", Zone: "eu-west-3c"}, {Name: "demo-cp-1", Zone: "eu-west-3a"}}}
	zones, err := awsLike(t).Place(context.Background(), machines, last)
	if err != nil || !slices.Equal(zones, threeZones) {
		t.Fatalf("zones = %v, %v", zones, err)
	}
	if got := fmt.Sprint(zonesOf(machines)); got != "map[demo-cp-0:eu-west-3c demo-cp-1:eu-west-3a]" {
		t.Errorf("zones = %s", got)
	}
}

// A new cluster asks once, for the sizes it uses, and takes the zones that offer them all.
func TestPlaceNewCluster(t *testing.T) {
	var asked [][]string
	p := Placement{Region: "europe-west1", DefaultSize: "e2-medium", Kind: "machine type",
		Offerings: func(_ context.Context, sizes []string) (map[string][]string, error) {
			asked = append(asked, sizes)
			return map[string][]string{"e2-medium": {"europe-west1-d", "europe-west1-b", "europe-west1-c"}, "n2-standard-4": {"europe-west1-b", "europe-west1-c"}}, nil
		}}
	machines := Machines("demo", []providers.Pool{
		{Name: "control-plane", Role: "control-plane", Count: 1},
		{Name: "big", Role: "worker", Count: 2, InstanceType: "n2-standard-4"},
	})
	zones, err := p.Place(context.Background(), machines, nil)
	if err != nil || !slices.Equal(zones, []string{"europe-west1-b", "europe-west1-c"}) {
		t.Fatalf("zones = %v, %v", zones, err)
	}
	if fmt.Sprint(asked) != "[[e2-medium n2-standard-4]]" {
		t.Errorf("asked %v", asked)
	}
	if got := fmt.Sprint(zonesOf(machines)); got != "map[demo-big-0:europe-west1-c demo-big-1:europe-west1-b demo-cp-0:europe-west1-b]" {
		t.Errorf("zones = %s", got)
	}
}
