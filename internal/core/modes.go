package core

import (
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// A mode decides who owns the machines, and state remembers that owner. Changing the mode in
// place would make tow forget what the old mode created: provision → byoh, for one, would
// leave OpenTofu's servers running and billing, since a byoh delete never touches machines.

// describeBuilt says what an existing cluster is made of, from its state.
func describeBuilt(st *state.ClusterState) string {
	switch st.Mode {
	case config.ModeLocal:
		return "a k3d cluster on this machine"
	case config.ModeProvision:
		if st.Spec != nil && st.Spec.Machines != nil {
			m := st.Spec.Machines
			return fmt.Sprintf("machines OpenTofu created (%s %s, %s)", m.Cloud, m.Region, ui.Plural(m.Total(), "machine"))
		}
		return "machines OpenTofu created"
	case config.ModeBYOH:
		if st.Spec != nil {
			return fmt.Sprintf("your own machines (%s)", ui.Plural(len(st.Spec.Nodes), "machine"))
		}
		return "your own machines"
	default:
		return "the " + st.Provider + " provider"
	}
}

// describeWanted says what a config file asks for.
func describeWanted(cfg *config.Config) string {
	switch cfg.Mode {
	case config.ModeLocal:
		return "a k3d cluster on this machine"
	case config.ModeProvision:
		return fmt.Sprintf("machines OpenTofu creates (%s %s)", cfg.Provider(), cfg.Location())
	default:
		return fmt.Sprintf("your own machines (%s listed)", ui.Plural(cfg.MachineCount(), "machine"))
	}
}

func modeSwitchError(prev *state.ClusterState, cfg *config.Config) error {
	was := "was built from"
	if prev.Status == state.StatusFailed {
		was = "failed partway while building it from"
	}
	return fmt.Errorf(`cluster '%s' %s %s; this file now describes %s.
A cluster can't change mode in place: tow would lose track of what the old mode created.
  → keep the old cluster: give this one a new name
  → replace it: tow delete --name %s, then apply again`,
		prev.Name, was, describeBuilt(prev), describeWanted(cfg), prev.Name)
}

// applyLabel and deleteLabel name the slow step in words that fit the mode.
func applyLabel(mode string, name string, cfg *config.Config) string {
	switch {
	case mode == config.ModeProvision && cfg != nil:
		return fmt.Sprintf("Creating %s for '%s' in %s %s", ui.Plural(cfg.MachineCount(), "machine"), name, cfg.Provider(), cfg.Location())
	case mode == config.ModeBYOH && cfg != nil:
		return fmt.Sprintf("Checking your %s for '%s'", ui.Plural(cfg.MachineCount(), "machine"), name)
	default:
		return fmt.Sprintf("Creating k3d cluster '%s' on this machine", name)
	}
}

func deleteLabel(st *state.ClusterState) string {
	switch st.Mode {
	case config.ModeProvision:
		if st.Spec != nil && st.Spec.Machines != nil {
			return fmt.Sprintf("Destroying the machines of '%s' in %s %s", st.Name, st.Spec.Machines.Cloud, st.Spec.Machines.Region)
		}
		return fmt.Sprintf("Destroying the machines of '%s'", st.Name)
	case config.ModeBYOH:
		return fmt.Sprintf("Forgetting cluster '%s'", st.Name)
	case config.ModeLocal:
		return fmt.Sprintf("Deleting k3d cluster '%s'", st.Name)
	default:
		return fmt.Sprintf("Deleting %s cluster '%s'", st.Provider, st.Name)
	}
}

// machine is one address a cluster uses. Machines listed by hand (byoh) are told apart by
// port too, since VMs behind one router are often reached through forwarded ports on the same
// IP; for machines tow created, one address is one machine, whatever the port.
type machine struct {
	host  string
	port  int
	byoh  bool
	owner string // what the machine is to its cluster, for the message
}

func normHost(h string) string { return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), ".")) }

func (a machine) same(b machine) bool {
	if normHost(a.host) != normHost(b.host) {
		return false
	}
	return a.port == b.port || !a.byoh || !b.byoh
}

func (a machine) String() string { return net.JoinHostPort(a.host, strconv.Itoa(a.port)) }

// machinesOf lists the addresses a saved cluster uses: the nodes a byoh cluster adopted, or
// the servers OpenTofu created. Local clusters and failed provisions have none on record.
// Private IPs are left out: a cloud's 10.0.1.5 and a homelab's 10.0.1.5 are different machines.
func machinesOf(st *state.ClusterState) []machine {
	var out []machine
	switch {
	case st.Mode == config.ModeBYOH && st.Spec != nil:
		for _, n := range st.Spec.Nodes {
			out = append(out, machine{host: n.Host, port: n.Port, byoh: true, owner: "one of its machines"})
		}
	case st.Mode == config.ModeProvision && st.Outputs != nil:
		for _, n := range st.Outputs.Nodes {
			out = append(out, machine{host: n.Host, port: n.Port, owner: "a server OpenTofu created for it"})
		}
	}
	return out
}

// checkMachinesFree refuses to adopt a machine another cluster already uses: Phase 2 would
// install Kubernetes on it twice, and deleting the other cluster could destroy it.
func checkMachinesFree(cfg *config.Config) error {
	names, err := state.List()
	if err != nil {
		return fmt.Errorf("list existing clusters: %w", err)
	}

	var clashes []string
	others := map[string]bool{}
	for _, name := range names {
		if name == cfg.Name() {
			continue
		}
		st, err := state.Load(name)
		if err != nil {
			ui.Debug("skipping cluster %s while checking machines: %v", name, err)
			continue
		}
		taken := machinesOf(st)
		for _, n := range specFromConfig(cfg, "").Nodes {
			want := machine{host: n.Host, port: n.Port, byoh: true}
			for _, m := range taken {
				if want.same(m) {
					clashes = append(clashes, fmt.Sprintf("%s already belongs to cluster '%s' (%s)", want, name, m.owner))
					others[name] = true
					break
				}
			}
		}
	}
	if len(clashes) == 0 {
		return nil
	}

	var deletes []string
	for _, name := range names {
		if others[name] {
			deletes = append(deletes, "tow delete --name "+name)
		}
	}
	return fmt.Errorf("%s\nA machine can only be in one cluster:\n  → remove it from this file, or delete the other cluster first (%s)",
		strings.Join(clashes, "\n"), strings.Join(deletes, ", "))
}
