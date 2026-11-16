package cloud

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// MaxZones is how many zones a new cluster spreads over.
const MaxZones = 3

// Placement puts machines in a region's zones, for the clouds where each machine lives in one
// zone (AWS, GCP, Azure).
type Placement struct {
	Region      string
	DefaultSize string // the size of machines that name none
	Kind        string // what the cloud calls a machine size, for messages: "instance type"
	// Offerings lists, for each size, the zones of the region that offer it.
	Offerings func(ctx context.Context, sizes []string) (map[string][]string, error)
}

func (p Placement) size(s string) string {
	if s != "" {
		return s
	}
	return p.DefaultSize
}

// Place puts each machine in a zone and returns the cluster's zones. A new cluster gets up to
// three zones offering every size it uses, and keeps them for good (on AWS each zone has its
// subnet). A machine keeps the zone it has, so adding or removing machines never moves the
// others; the cloud is asked only about the sizes of machines that need a zone.
func (p Placement) Place(ctx context.Context, machines []Machine, prev *Inputs) ([]string, error) {
	placed := prev.Placed()
	var zones []string
	if prev != nil {
		zones = prev.Zones
	}
	need := map[string]bool{}
	for _, m := range machines {
		if was, ok := placed[m.Name]; len(zones) == 0 || !ok || !slices.Contains(zones, was.Zone) {
			need[p.size(m.InstanceType)] = true
		}
	}
	var offers map[string][]string
	if len(need) > 0 {
		sizes := make([]string, 0, len(need))
		for s := range need {
			sizes = append(sizes, s)
		}
		slices.Sort(sizes)
		var err error
		if offers, err = p.Offerings(ctx, sizes); err != nil {
			return nil, err
		}
		if len(zones) == 0 {
			if zones, err = p.commonZones(sizes, offers); err != nil {
				return nil, err
			}
		}
	}
	return zones, p.placeZones(machines, placed, zones, offers)
}

// commonZones picks a new cluster's zones: the first three, by name, that offer every size it
// uses.
func (p Placement) commonZones(sizes []string, offers map[string][]string) ([]string, error) {
	var common []string
	for i, s := range sizes {
		zones := offers[s]
		if len(zones) == 0 {
			return nil, fmt.Errorf("%s %s is not offered in any zone of %s; check its spelling, or pick another instanceType", p.Kind, s, p.Region)
		}
		if i == 0 {
			common = slices.Clone(zones)
			continue
		}
		common = slices.DeleteFunc(common, func(z string) bool { return !slices.Contains(zones, z) })
	}
	if len(common) == 0 {
		return nil, fmt.Errorf("no zone of %s offers all of these %ss together: %s; pick ones offered in the same zones", p.Region, p.Kind, strings.Join(sizes, ", "))
	}
	slices.Sort(common)
	return common[:min(MaxZones, len(common))], nil
}

// placeZones gives each machine a zone. A machine with a zone keeps it. Each other one, in
// order, goes to a zone offering its size: the one with the fewest machines of its own pool,
// then the fewest machines overall, then the first in the cluster's list. For a new cluster
// that is a round robin over the zones, control planes first.
func (p Placement) placeZones(machines []Machine, placed map[string]Machine, zones []string, offers map[string][]string) error {
	ofPool := map[string]map[string]int{}
	overall := map[string]int{}
	count := func(m Machine) {
		if ofPool[m.Pool] == nil {
			ofPool[m.Pool] = map[string]int{}
		}
		ofPool[m.Pool][m.Zone]++
		overall[m.Zone]++
	}
	// A zone can be "" (Azure, in a location without zones): kept tells placed machines apart.
	kept := make([]bool, len(machines))
	for i, m := range machines {
		if was, ok := placed[m.Name]; ok && slices.Contains(zones, was.Zone) {
			machines[i].Zone, kept[i] = was.Zone, true
			count(machines[i])
		}
	}
	for i, m := range machines {
		if kept[i] {
			continue
		}
		s, pool := p.size(m.InstanceType), ofPool[m.Pool]
		best, found := "", false
		for _, z := range zones {
			if !slices.Contains(offers[s], z) {
				continue
			}
			if !found || pool[z] < pool[best] || (pool[z] == pool[best] && overall[z] < overall[best]) {
				best, found = z, true
			}
		}
		if !found {
			return fmt.Errorf("pool %s: %s %s isn't offered in any of the cluster's zones (%s); pick another instanceType", m.Pool, p.Kind, s, strings.Join(zones, ", "))
		}
		machines[i].Zone = best
		count(machines[i])
	}
	return nil
}
