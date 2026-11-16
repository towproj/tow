package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"github.com/towproj/tow/internal/version"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Show tow's version, the commit it was built from, and its platform",
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Fprint(cmd.OutOrStdout(), version.Get().String())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
