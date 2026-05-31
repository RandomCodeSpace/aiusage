package cmd

import (
	"github.com/spf13/cobra"

	"aiusage/internal/buildinfo"
)

// newVersionCmd prints the build identity used to keep the CLI and daemon in
// sync. It is in daemonSkip, so running it never spawns or restarts the daemon.
func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:           "version",
		Short:         "Print the aiusage build identity",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(c *cobra.Command, _ []string) error {
			_, err := c.OutOrStdout().Write([]byte(buildinfo.Identity() + "\n"))
			return err
		},
	}
}
