package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() { Register(newInitCmd) }

func newInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Interactive setup wizard for a new profile (M1)",
		Long: "M0 ships a skeleton. The interactive wizard lands in M1 once\n" +
			"doctor and the change source are both stable.",
		RunE: func(cmd *cobra.Command, _ []string) error {
			fmt.Fprintln(cmd.OutOrStdout(),
				"unredo init: M1 work item. Use `unredo doctor` for now to verify prerequisites.")
			return nil
		},
	}
	c.Flags().String("profile", "default", "profile name to create or update")
	return c
}
