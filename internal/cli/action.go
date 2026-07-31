package cli

import (
	"github.com/spf13/cobra"
)

func init() { Register(newActionCmd) }

func newActionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "action",
		Short: "Action marker inspection and reapply (M2/M3)",
	}
	c.AddCommand(newActionShowCmd(), newActionReapplyCmd())
	return c
}

func newActionShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show",
		Short: "Show an action marker from unredo_meta (M2)",
		RunE:  notImplemented("action show"),
	}
	c.Flags().String("action-id", "", "action id (ulid)")
	return c
}

func newActionReapplyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reapply",
		Short: "Generate a reapply plan from an action and the root plan (M3)",
		RunE:  notImplemented("action reapply"),
	}
	c.Flags().String("action-id", "", "action id (ulid)")
	c.Flags().String("root-plan", "", "root plan file")
	c.Flags().String("output", "", "output reapply plan file")
	return c
}
