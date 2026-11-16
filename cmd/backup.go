package cmd

import (
	"github.com/spf13/cobra"
	"github.com/towproj/tow/internal/core"
)

var backupCmd = &cobra.Command{
	Use:   "backup",
	Short: "Take an etcd snapshot of a cluster now and keep a copy on this machine",
	Long: `Take an etcd snapshot now on one of the cluster's control planes (the first
that answers), copy it to this machine and check the copy's sha256. The copy
goes to ~/.tow/backups/<cluster>/ (the folder is yours alone, each file
0600), with a .json beside it: the cluster, its Kubernetes release and
distribution, the machine and time it was taken, its sha256. Backups stay
when the cluster is deleted.

The snapshot holds everything in etcd, the cluster's Secrets included: in
plain text unless they're encrypted at rest. Keep it as safe as the cluster's
credentials.

k3s also keeps its own snapshots on the control planes (spec.kubernetes.backups
sets when and how many; k3s takes one every 12 hours otherwise); on kubeadm,
spec.kubernetes.backups puts a timer on each control plane. --list shows the
copies on this machine. A local (k3d) cluster has no snapshots.`,
	Example: `  tow backup --name prod
  tow backup --name prod --list`,
	Args: cobra.NoArgs,
	RunE: runBackup,
}

func init() {
	rootCmd.AddCommand(backupCmd)
	backupCmd.Flags().String("name", "", "Cluster to back up")
	backupCmd.Flags().Bool("list", false, "List the cluster's backups on this machine instead")
	backupCmd.MarkFlagRequired("name")
}

func runBackup(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	if list, _ := cmd.Flags().GetBool("list"); list {
		return core.PrintBackups(name)
	}
	_, err := core.NewOrchestrator(false).Backup(cmd.Context(), name)
	return err
}
