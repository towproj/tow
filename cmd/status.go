package cmd

import (
	"github.com/spf13/cobra"
	"github.com/towproj/tow/internal/core"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "List your clusters, or show one cluster's health",
	Long: `Without --name, list the clusters tow manages, from its saved state alone.

With --name, show one cluster's health: its machines as their provider sees
them, then Kubernetes as your kubectl does (through the kubeconfig tow gave
you): the nodes, and every pod that isn't running as it should, in any
namespace. Nothing is changed. The command fails (exit code 1) unless every
machine runs, the API answers, every node is Ready and no pod is stuck.`,
	Example: `  tow status
  tow status --name dev`,
	Args: cobra.NoArgs,
	RunE: runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
	statusCmd.Flags().String("name", "", "Cluster to show (all of them, listed, when not given)")
}

func runStatus(cmd *cobra.Command, args []string) error {
	name, _ := cmd.Flags().GetString("name")
	o := core.NewOrchestrator(false)
	if name == "" {
		return o.List()
	}
	return o.Status(cmd.Context(), name)
}
