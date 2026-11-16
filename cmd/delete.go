package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/towproj/tow/internal/core"
	"github.com/towproj/tow/internal/ui"
)

var deleteCmd = &cobra.Command{
	Use:   "delete",
	Short: "Delete a cluster and the infrastructure tow created for it",
	Long: `Delete a cluster tow manages, then forget it.
Provisioned servers are destroyed with OpenTofu (when tofu is missing, tow
asks before installing it), and the cloud is checked for anything still tagged
with the cluster's name. Local k3d clusters are deleted.

On your own machines (byoh), Kubernetes is removed from them: k3s's own
uninstaller, or kubeadm reset and what tow installed for it. What tow
prepared there (packages, firewall rules, swap, kernel settings) stays unless
you pass --reset or answer yes when asked; only what tow recorded in
/etc/tow/prepared.json is undone, never what was there before.
--forget forgets the cluster without touching any machine, for one that's
gone for good.

Volumes on EBS or Proxmox disks outlive the machines, and on your own machines
volumes keep their data on the machines' disks: a cluster that has some isn't
deleted unless you pass -v, which deletes them with it (through Kubernetes, so
their drivers delete the data).`,
	Example: `  tow delete --name dev
  tow delete --name ci-123 --yes
  tow delete --name lab -v
  tow delete --name homelab --reset
  tow delete --name homelab --forget`,
	Args: cobra.NoArgs,
	RunE: runDelete,
}

func init() {
	rootCmd.AddCommand(deleteCmd)

	deleteCmd.Flags().String("name", "", "Cluster to delete")
	deleteCmd.Flags().BoolP("yes", "y", false, "Skip the confirmation prompt")
	deleteCmd.Flags().BoolP("volumes", "v", false, "Delete the cluster's volumes too, with their data (on EBS, Proxmox disks, or your machines' disks)")
	deleteCmd.Flags().Bool("reset", false, "On your own machines, also undo what tow prepared there")
	deleteCmd.Flags().Bool("forget", false, "On your own machines, forget the cluster without touching any machine")
	deleteCmd.MarkFlagsMutuallyExclusive("reset", "forget")
	cobra.CheckErr(deleteCmd.MarkFlagRequired("name")) // fails only on a flag that doesn't exist
}

func runDelete(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	yes, _ := cmd.Flags().GetBool("yes")
	volumes, _ := cmd.Flags().GetBool("volumes")
	reset, _ := cmd.Flags().GetBool("reset")
	forget, _ := cmd.Flags().GetBool("forget")

	// Destroying servers needs an explicit yes; without a terminal it must come from --yes.
	if !yes && !ui.Interactive() {
		return fmt.Errorf("refusing to delete '%s' without confirmation in a non-interactive session; pass --yes", name)
	}
	confirm := func(summary string) bool { return yes || ui.Confirm(summary+". Continue?") }
	o := core.NewOrchestrator(false)
	if volumes {
		o.WithVolumes()
	}
	switch {
	case reset:
		o.WithReset()
	case forget:
		o.Forget()
	case !yes:
		// --yes alone keeps the preparation: undoing it unattended takes --reset.
		o.AskReset(ui.Confirm)
	}
	return o.Delete(cmd.Context(), name, confirm)
}
