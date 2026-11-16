package cmd

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/towproj/tow/internal/ui"
	"github.com/towproj/tow/internal/version"
)

var rootCmd = &cobra.Command{
	Use:   "tow",
	Short: "A CLI for bootstrapping and managing Kubernetes clusters",
	Long:  `tow simplifies Kubernetes cluster provisioning, installation, and lifecycle management. No YAML hell. Just run a command and get a working cluster.`,
	Run: func(cmd *cobra.Command, args []string) {
		cmd.Help()
	},
}

func Execute() {
	if err := rootCmd.Execute(); err != nil {
		// Printed here rather than by cobra: errors carry tool output, which may hold secrets.
		fmt.Fprintln(os.Stderr, "Error:", ui.Redact(err.Error()))
		os.Exit(1)
	}
}

func init() {
	// Bind flag before subcommand logic runs. PreRun guarantees state is set.
	rootCmd.PersistentPreRun = func(cmd *cobra.Command, args []string) {
		v, _ := cmd.Flags().GetBool("verbose")
		ui.SetVerbose(v)
		ni, _ := cmd.Flags().GetBool("non-interactive")
		ui.SetNonInteractive(ni)
	}

	rootCmd.SilenceUsage = true
	rootCmd.SilenceErrors = true            // Execute prints them, redacted
	rootCmd.Version = version.Get().Short() // tow --version; 'tow version' says more
	rootCmd.SetVersionTemplate("tow {{.Version}}\n")
	rootCmd.PersistentFlags().Bool("verbose", false, "enable debug logging")
	rootCmd.PersistentFlags().Bool("non-interactive", false, "never prompt; fail instead (automatic when stdin is not a terminal)")
}
