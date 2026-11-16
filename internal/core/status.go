package core

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/kubeapi"
	"github.com/towproj/tow/internal/providers"
	"github.com/towproj/tow/internal/secretenv"
	"github.com/towproj/tow/internal/state"
	"github.com/towproj/tow/internal/ui"
)

// maxProblems is how many stuck pods status lists before it only counts the rest.
const maxProblems = 20

// Swappable so tests run without a cluster.
var clusterHealth = func(ctx context.Context, kubeconfig string) (kubeapi.Health, error) {
	api, err := kubeapiFromFile(kubeconfig)
	if err != nil {
		return kubeapi.Health{}, err
	}
	return api.Health(ctx)
}

// Report is what 'tow status' shows: the machines as the provider sees them, then
// Kubernetes as the user's kubectl does, through the kubeconfig tow handed out.
type Report struct {
	Name, Where, Kubernetes, Endpoint string
	Machines                          providers.StatusResult
	Installed                         bool   // Kubernetes was installed (else: which stage the cluster reached)
	Stage                             string // the last stage finished
	APIError                          string // the API didn't answer
	Nodes                             []kubeapi.Node
	Problems                          []string
	Unknown                           []string // nodes tow didn't make (joined outside tow?)
	Missing                           []string // nodes tow made that the API doesn't have
	Newer                             []string // add-ons this tow installs a newer release of: "longhorn 1.13.0 (1.12.1 runs)"
	Releases                          string   // the nodes run different releases: an upgrade stopped partway
	NewerKubernetes                   string   // a newer patch of the running minor release is out
	// When the API server's certificate and the kubeconfig's client certificate expire
	// (zero: not read).
	APICertExpires, KubeconfigExpires time.Time
}

// Healthy: every machine runs, the API answers, every node is Ready, and no pod is stuck.
func (r Report) Healthy() bool {
	if !r.Machines.Healthy || !r.Installed || r.APIError != "" || len(r.Nodes) == 0 || len(r.Problems) > 0 {
		return false
	}
	for _, n := range r.Nodes {
		if !n.Ready {
			return false
		}
	}
	return true
}

// Report reads a cluster's health. It changes nothing; the API gets 30 seconds to answer.
func (o *Orchestrator) Report(ctx context.Context, name string) (Report, error) {
	st, err := state.Load(name)
	if err != nil {
		return Report{}, fmt.Errorf("no cluster named '%s' (run 'tow status' to list them)", name)
	}
	p, err := newProvider(st.Provider)
	if err != nil {
		return Report{}, err
	}
	spec := specFromState(st)
	secretenv.Hide(spec.SecretEnv...)
	r := Report{Name: name, Where: where(st), Kubernetes: kubernetesOf(st), Stage: st.Stage}
	if st.Outputs != nil {
		r.Endpoint = st.Outputs.Endpoint
	}
	if r.Machines, err = p.Status(ctx, spec); err != nil {
		return Report{}, err
	}
	r.Installed = st.Outputs != nil && st.Outputs.Kubeconfig != "" &&
		(st.Status == state.StatusReady || st.Stage == state.StageKubernetes || st.Stage == state.StageAddons)
	if !r.Installed {
		return r, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	h, err := clusterHealth(ctx, st.Outputs.Kubeconfig)
	if err != nil {
		r.APIError = err.Error()
		return r, nil
	}
	r.Nodes, r.Problems = h.Nodes, h.Problems
	r.Newer = olderBuiltins(ctx, st)
	if st.Mode != config.ModeLocal && st.Spec != nil {
		r.Releases = mixedReleases(h.Nodes)
		r.NewerKubernetes = newerKubernetes(ctx, st.Spec.Engine, st.K8sVersion)
		r.APICertExpires, r.KubeconfigExpires, _ = certExpiry(ctx, st.Outputs.Kubeconfig)
	}
	// Machines tow made carry their node's name; k3d names its own, so there's nothing to compare.
	if len(st.Outputs.Nodes) > 0 {
		ours, seen := map[string]bool{}, map[string]bool{}
		for _, n := range st.Outputs.Nodes {
			ours[n.Name] = true
		}
		for _, n := range h.Nodes {
			seen[n.Name] = true
			if !ours[n.Name] {
				r.Unknown = append(r.Unknown, n.Name)
			}
		}
		for _, n := range st.Outputs.Nodes {
			if !seen[n.Name] {
				r.Missing = append(r.Missing, n.Name)
			}
		}
	}
	return r, nil
}

// certificatesLine says when the API server's certificate and the kubeconfig's expire; one
// within 30 days says the next apply renews it.
func certificatesLine(api, client, now time.Time) string {
	var parts []string
	soon := false
	for _, c := range []struct {
		what string
		t    time.Time
	}{{"the API server's", api}, {"your kubeconfig's", client}} {
		if c.t.IsZero() {
			continue
		}
		parts = append(parts, c.what+" "+expiryNote(c.t, now))
		soon = soon || c.t.Sub(now) < renewWithin
	}
	if len(parts) == 0 {
		return ""
	}
	line := "Certificates: " + strings.Join(parts, "; ")
	if soon {
		line += " (soon: 'tow apply' renews them, and fetches your kubeconfig again)"
	}
	return line
}

// mixedReleases says which nodes run which release when they don't all run the same: an
// upgrade that stopped partway, which the next apply finishes.
func mixedReleases(nodes []kubeapi.Node) string {
	byRelease := map[string][]string{}
	var order []string
	for _, n := range nodes {
		if byRelease[n.Version] == nil {
			order = append(order, n.Version)
		}
		byRelease[n.Version] = append(byRelease[n.Version], n.Name)
	}
	if len(order) < 2 {
		return ""
	}
	var parts []string
	for _, v := range order {
		parts = append(parts, fmt.Sprintf("%s run %s", strings.Join(byRelease[v], ", "), v))
	}
	return strings.Join(parts, "; ") + " (an upgrade that stopped partway: 'tow apply' finishes it)"
}

// Status prints a cluster's health; an unhealthy cluster is an error (exit code 1).
func (o *Orchestrator) Status(ctx context.Context, name string) error {
	r, err := o.Report(ctx, name)
	if err != nil {
		return err
	}
	printReport(os.Stdout, r)
	if !r.Healthy() {
		return fmt.Errorf("cluster '%s' isn't healthy", name)
	}
	return nil
}

func printReport(w io.Writer, r Report) {
	icon, verdict := "✓", "healthy"
	if !r.Healthy() {
		icon, verdict = "✗", "not healthy"
	}
	head := fmt.Sprintf("%s %s: %s, %s on %s", icon, r.Name, verdict, r.Kubernetes, r.Where)
	if r.Endpoint != "" {
		head += ", API https://" + r.Endpoint
	}
	fmt.Fprintln(w, ui.Redact(head))
	m := r.Machines
	line := fmt.Sprintf("  Machines: control planes %s, workers %s running", m.ControlPlanes, m.Workers)
	if m.State == "not_found" {
		line = "  Machines: none found"
	}
	if m.Message != "" {
		line += " (" + m.Message + ")"
	}
	fmt.Fprintln(w, ui.Redact(line))
	switch {
	case !r.Installed:
		fmt.Fprintf(w, "  Kubernetes: not installed yet (the last stage done: %s); 'tow apply' carries on\n", r.Stage)
		return
	case r.APIError != "":
		fmt.Fprintln(w, ui.Redact("  Kubernetes: the API doesn't answer: "+r.APIError))
		return
	}
	fmt.Fprintln(w, "  Nodes:")
	tw := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
	for _, n := range r.Nodes {
		role, ready := "worker", "Ready"
		if n.ControlPlane {
			role = "control plane"
		}
		if !n.Ready {
			ready = "NotReady"
		}
		fmt.Fprintf(tw, "    %s\t%s\t%s\t%s\n", n.Name, role, ready, n.Version)
	}
	tw.Flush()
	if len(r.Unknown) > 0 {
		fmt.Fprintf(w, "  Not made by tow (joined outside tow?): %s; tow leaves them alone\n", strings.Join(r.Unknown, ", "))
	}
	if len(r.Missing) > 0 {
		fmt.Fprintf(w, "  Made by tow but not in the cluster: %s\n", strings.Join(r.Missing, ", "))
	}
	if r.Releases != "" {
		fmt.Fprintf(w, "  Releases: %s\n", r.Releases)
	}
	if r.NewerKubernetes != "" {
		fmt.Fprintf(w, "  Upgrade: %s\n", r.NewerKubernetes)
	}
	if line := certificatesLine(r.APICertExpires, r.KubeconfigExpires, time.Now()); line != "" {
		fmt.Fprintln(w, "  "+line)
	}
	if len(r.Newer) > 0 {
		fmt.Fprintf(w, "  Add-ons: %s\n", newerNote(r.Newer))
	}
	if len(r.Problems) > 0 {
		fmt.Fprintln(w, "  Problems:")
		for i, p := range r.Problems {
			if i == maxProblems {
				fmt.Fprintf(w, "    … and %d more (kubectl get pods -A)\n", len(r.Problems)-maxProblems)
				break
			}
			fmt.Fprintln(w, "    "+ui.Redact(p))
		}
	}
}

// List prints every cluster tow keeps state for, from that state alone: no network.
func (o *Orchestrator) List() error {
	names, err := state.List()
	if err != nil {
		return err
	}
	if len(names) == 0 {
		fmt.Println("No clusters yet: 'tow apply -f <file>' creates one")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(tw, "NAME\tWHERE\tKUBERNETES\tSTATE")
	for _, name := range names {
		st, err := state.Load(name)
		if err != nil {
			fmt.Fprintf(tw, "%s\t?\t?\tstate unreadable: %v\n", name, err)
			continue
		}
		s := st.Status
		if st.Status != state.StatusReady && st.Stage != "" {
			s += ", last stage done: " + st.Stage
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", name, where(st), kubernetesOf(st), s)
	}
	tw.Flush()
	fmt.Println("Details: tow status --name <name>")
	return nil
}

// where says where a cluster's machines are.
func where(st *state.ClusterState) string {
	switch {
	case st.Mode == config.ModeBYOH:
		return "your machines"
	case st.Mode == config.ModeLocal:
		return "k3d (this machine)"
	case st.Spec != nil && st.Spec.Machines != nil && st.Spec.Machines.Cloud == "aws":
		return "AWS " + st.Spec.Machines.Region
	case st.Spec != nil && st.Spec.Machines != nil && st.Spec.Machines.Cloud == "gcp":
		return "GCP " + st.Spec.Machines.Region + " (project " + st.Spec.Machines.Project + ")"
	case st.Spec != nil && st.Spec.Machines != nil && st.Spec.Machines.Cloud == "azure":
		return "Azure " + st.Spec.Machines.Region + " (resource group tow-" + st.Name + ")"
	case st.Spec != nil && st.Spec.Machines != nil && st.Spec.Machines.Cloud == "proxmox":
		return "Proxmox node " + st.Spec.Machines.Region
	case st.Spec != nil && st.Spec.Machines != nil:
		return st.Spec.Machines.Cloud + " " + st.Spec.Machines.Region
	}
	return st.Provider
}

// kubernetesOf names the distribution and release: "k3s v1.37.2+k3s1".
func kubernetesOf(st *state.ClusterState) string {
	d := ""
	if st.Spec != nil {
		d = st.Spec.Engine
	}
	return strings.TrimSpace(d + " " + st.K8sVersion)
}
