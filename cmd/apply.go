package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/towproj/tow/internal/config"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/ui"
)

var applyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Create a cluster from a tow.yaml file",
	Long: `Read a cluster description from tow.yaml and build it.
spec.infrastructure.provider decides where the machines come from: aws, gcp,
azure and proxmox create them with OpenTofu, byoh uses the machines you list,
local runs k3d on this machine. A flavor is an optional preset; anything it sets can be
overridden.

Machines are then prepared for Kubernetes as the official docs ask (packages,
kernel settings, swap, firewall), changing only what's missing. On your own
machines tow lists the changes and asks first; every change is recorded on
the machine in /etc/tow/prepared.json.

With k3s, Kubernetes then goes on the machines: the first control plane starts
the cluster, the others join it one at a time, then the workers. The kubeconfig
is saved in ~/.tow/clusters/<name>/ and added to yours under the file's
context name. An apply that stops partway picks up from there when run again.

The file's Helm charts are then installed with helm, from this machine, in order.
When a tool tow runs here is missing or unsupported (helm older than 3.8,
tofu older than 1.12, kubectl of another minor release than the cluster's),
tow says so and asks before installing a release it works with into
~/.local/bin.

On a cluster that already runs, apply compares the file with what's really there
and shows a plan in two parts, infrastructure then cluster: nodes' labels and
taints, the kubeconfig, add-ons, and anything OpenTofu would put back. It asks,
then makes the changes in that order. A change the cluster can't follow is
refused with the reason, before anything else. Only one apply or delete runs on
a cluster at a time.

Machines the file adds are created (or checked, on your own), prepared and
joined first; machines it removes leave last, one at a time: drained (never
forced), Kubernetes removed from them, then destroyed. On your own machines
their preparation stays unless --reset.`,
	Example: `  tow apply
  tow apply -f examples/local.yaml
  tow apply --dry-run -f examples/dev-test-aws.yaml
  tow apply -f examples/homelab.yaml --yes   # don't ask before preparing your machines
  tow apply -f examples/local-helm.yaml      # a local cluster with a Helm chart`,
	Args: cobra.NoArgs,
	RunE: runApply,
}

func init() {
	rootCmd.AddCommand(applyCmd)

	applyCmd.Flags().StringP("file", "f", config.DefaultFileName, "Path to the cluster config")
	applyCmd.Flags().Bool("dry-run", false, "Show what apply would do; create nothing and save no state")
	applyCmd.Flags().BoolP("yes", "y", false, "Make the changes without asking: on your own machines, and on a running cluster (they're listed either way)")
	applyCmd.Flags().Bool("install-tools", false, "Install a tool tow needs on this machine (helm, kubectl, tofu) into ~/.local/bin without asking, when it's missing or unsupported")
	applyCmd.Flags().Bool("reset", false, "On your own machines leaving the cluster, also undo what tow prepared there")
	applyCmd.Flags().Duration("drain-timeout", core.DefaultDrainTimeout, "How long a machine leaving the cluster gets to move its pods away; nothing is ever forced")
	applyCmd.Flags().Bool("renew-certificates", false, "Renew a running cluster's certificates now (apply does when they expire within 30 days), and fetch your kubeconfig again")
}

func runApply(cmd *cobra.Command, args []string) error {
	path, _ := cmd.Flags().GetString("file")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	yes, _ := cmd.Flags().GetBool("yes")
	installTools, _ := cmd.Flags().GetBool("install-tools")
	reset, _ := cmd.Flags().GetBool("reset")
	drain, _ := cmd.Flags().GetDuration("drain-timeout")
	renew, _ := cmd.Flags().GetBool("renew-certificates")

	// Fail fast: nothing touches infra until the whole file is valid.
	cfg, err := loadConfig(path)
	if err != nil {
		return err
	}
	ui.Info(fmt.Sprintf("Loaded %s: %s", path, describe(cfg)))
	for _, w := range cfg.Warnings() {
		ui.Warn(w)
	}
	if pending := core.Pending(cfg); len(pending) > 0 {
		ui.Warn("not installed yet by this version: " + strings.Join(pending, "; ") + "\n  → each arrives with its Phase 2 step")
	}

	o := core.NewOrchestrator(dryRun)
	if yes {
		o.AssumeYes()
	}
	if installTools {
		o.InstallTools()
	}
	if reset {
		o.WithReset()
	}
	o.WithDrainTimeout(drain)
	if renew {
		o.WithCertRenewal()
	}
	if err := o.Apply(cmd.Context(), cfg); err != nil {
		// Every failed run ends with how to carry on: the same command again.
		return fmt.Errorf("%w\n  → to run it again: %s", err, commandLine(os.Args))
	}
	return nil
}

// commandLine is the command as it was typed, quoted for a shell where needed.
func commandLine(args []string) string {
	quoted := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t\n'\"\\$`*?[]{}()<>|&;#~!") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		quoted[i] = a
	}
	return ui.Redact(strings.Join(quoted, " "))
}

// loadConfig turns config errors into one message listing every problem.
func loadConfig(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		return cfg, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%s not found (copy one from examples/ or pass -f <file>)", path)
	}

	var joined interface{ Unwrap() []error }
	if !errors.As(err, &joined) {
		return nil, fmt.Errorf("%s is invalid: %w", path, err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s is invalid:", path)
	for _, e := range joined.Unwrap() {
		fmt.Fprintf(&b, "\n  - %s", e)
	}
	return nil, errors.New(b.String())
}

// describe sums up what the file asks for, after defaults: "cluster 'lab': kubeadm + calico
// on aws eu-west-3, 1 control plane + 2 workers (flavor dev-test)".
func describe(cfg *config.Config) string {
	k := cfg.Spec.Kubernetes
	what := k.Distribution + " + " + k.Networking.CNI
	if k.Networking.CNI == config.CNINone {
		what = k.Distribution + " (no CNI)"
	}
	where := cfg.Provider()
	if loc := cfg.Location(); loc != "" {
		where += " " + loc
	}
	machines := plural(cfg.ControlPlaneCount(), "control plane")
	if n := cfg.WorkerCount(); n > 0 {
		machines += " + " + plural(n, "worker")
	}
	out := fmt.Sprintf("cluster '%s': %s on %s, %s", cfg.Name(), what, where, machines)
	if f := cfg.Spec.Flavor; f != "" {
		out += fmt.Sprintf(" (flavor %s)", f)
	}
	return out
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
