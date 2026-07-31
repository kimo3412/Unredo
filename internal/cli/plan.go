package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() { Register(newPlanCmd) }

func newPlanCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "plan",
		Short: "Compensation plan lifecycle (create, check, apply, resolve)",
	}
	c.AddCommand(
		newPlanCreateCmd(),
		newPlanCheckCmd(),
		newPlanApplyCmd(),
		newPlanResolveCmd(),
	)
	return c
}

func newPlanCreateCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "create",
		Short: "Generate a self-contained compensation plan (M1)",
		RunE:  notImplemented("plan create"),
	}
	c.Flags().String("binlog", "", "binlog file")
	c.Flags().String("txn", "", "transaction id")
	c.Flags().String("mode", "revert", "revert|reapply")
	c.Flags().String("output", "", "output plan file")
	return c
}

func newPlanCheckCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check",
		Short: "Verify a plan is still safe to apply (M2)",
		RunE:  notImplemented("plan check"),
		Args:  cobra.MinimumNArgs(1),
	}
}

func newPlanApplyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apply",
		Short: "Execute a compensation plan (M2)",
		RunE:  notImplemented("plan apply"),
		Args:  cobra.MinimumNArgs(1),
	}
	c.Flags().String("confirm-sha", "", "first 8 chars of the plan digest")
	c.Flags().Bool("non-interactive", false, "do not prompt")
	c.Flags().String("accept-risk", "", "first 8 chars of the resolved plan digest (unsafe plans only)")
	return c
}

func newPlanResolveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "resolve",
		Short: "Produce a resolved plan for a conflict (M2)",
		RunE:  notImplemented("plan resolve"),
		Args:  cobra.MinimumNArgs(1),
	}
	c.Flags().String("output", "", "output resolved plan file")
	return c
}

func notImplemented(name string) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, _ []string) error {
		fmt.Fprintf(cmd.OutOrStdout(),
			"%s: scheduled for a later milestone (M1 plan create, M2 check/apply). Run `unredo doctor` or `unredo txn list/show` to validate the M0 binlog path.\n", name)
		return nil
	}
}
