package cmd

import (
	"github.com/spf13/cobra"
	"github.com/towproj/tow/internal/core"
)

var restoreCmd = &cobra.Command{
	Use:   "restore",
	Short: "Put a cluster's etcd back to one of its backups, on the same machines",
	Long: `Put a running cluster's etcd back to a snapshot 'tow backup' (or an
upgrade) copied to this machine: the newest one, unless --snapshot names
another ('tow backup --name <cluster> --list' shows them). Everything the
cluster's API knew comes back as it was then, and every change since is lost.
Volumes' data isn't in etcd and stays as it is.

Only a snapshot of the release the cluster runs can be restored, or the one
taken right before upgrading to it (the release stays the upgraded one).
Kubernetes can't go back to an older release.

A safety backup of the cluster as it is now is taken first, unless you pass
--no-backup. The plan is shown and asked about first; --yes doesn't ask,
--dry-run only shows it.

k3s: k3s is stopped on every control plane, the first one is reset to the
snapshot ('k3s server --cluster-reset'), then the others join it again.
kubeadm: etcd and the API servers stop on every control plane, every etcd
member is restored from the snapshot, then they start again and the kubelets
restart. The old etcd data is moved aside, not deleted. Workers keep running.

Afterwards, run 'tow apply' with the cluster's file: the snapshot holds the
add-ons and charts as they were then.`,
	Example: `  tow restore --name prod
  tow restore --name prod --snapshot prod-20261113T021206Z --dry-run
  tow restore --name prod --yes --no-backup`,
	Args: cobra.NoArgs,
	RunE: runRestore,
}

func init() {
	rootCmd.AddCommand(restoreCmd)
	restoreCmd.Flags().String("name", "", "Cluster to restore")
	restoreCmd.Flags().String("snapshot", "", "Backup to restore (its file, as --list shows it); the newest when not given")
	restoreCmd.Flags().BoolP("yes", "y", false, "Restore without asking")
	restoreCmd.Flags().Bool("dry-run", false, "Only show what the restore would do")
	restoreCmd.Flags().Bool("no-backup", false, "Take no safety backup of the cluster as it is now first")
	cobra.CheckErr(restoreCmd.MarkFlagRequired("name")) // fails only on a flag that doesn't exist
}

func runRestore(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	snapshot, _ := cmd.Flags().GetString("snapshot")
	yes, _ := cmd.Flags().GetBool("yes")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	noBackup, _ := cmd.Flags().GetBool("no-backup")
	o := core.NewOrchestrator(dryRun)
	if yes {
		o.AssumeYes()
	}
	if noBackup {
		o.WithoutBackup()
	}
	return o.Restore(cmd.Context(), name, snapshot)
}
