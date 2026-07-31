package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/girimi/unredo/internal/config"
	"github.com/girimi/unredo/internal/planner"
	"github.com/girimi/unredo/internal/ports"
	"github.com/spf13/cobra"
)

func init() { Register(newActionCmd) }

func newActionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "action",
		Short: "Inspect successful actions and generate reapply plans",
	}
	c.AddCommand(newActionShowCmd(), newActionReapplyCmd())
	return c
}

func newActionShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show",
		Short: "Show an action marker from unredo_meta",
		RunE:  runActionShow,
	}
	c.Flags().String("action-id", "", "action id (ulid)")
	return c
}

func newActionReapplyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "reapply",
		Short: "Generate a safe reapply plan from the latest revert action",
		RunE:  runActionReapply,
	}
	c.Flags().String("action-id", "", "action id (ulid)")
	c.Flags().String("root-plan", "", "root plan file")
	c.Flags().String("output", "", "output reapply plan file")
	return c
}

func runActionShow(cmd *cobra.Command, _ []string) error {
	actionID, _ := cmd.Flags().GetString("action-id")
	if actionID == "" {
		return fmt.Errorf("--action-id is required")
	}
	store, _, err := resolveActionStore(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := actionContext(cmd)
	defer cancel()
	action, err := store.FindAction(ctx, actionID)
	if errors.Is(err, ports.ErrActionNotFound) {
		return fmt.Errorf("action %q not found", actionID)
	}
	if err != nil {
		return fmt.Errorf("find action: %w", err)
	}
	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(action)
	}
	if format != "table" {
		return fmt.Errorf("--format must be table or json, got %q", format)
	}
	printAction(cmd, action)
	return nil
}

func runActionReapply(cmd *cobra.Command, _ []string) error {
	actionID, _ := cmd.Flags().GetString("action-id")
	rootPath, _ := cmd.Flags().GetString("root-plan")
	output, _ := cmd.Flags().GetString("output")
	if actionID == "" {
		return fmt.Errorf("--action-id is required")
	}
	if rootPath == "" {
		return fmt.Errorf("--root-plan is required")
	}
	if output == "" {
		return fmt.Errorf("--output is required")
	}
	root, err := planner.ReadFile(rootPath)
	if err != nil {
		return fmt.Errorf("read root plan: %w", err)
	}
	store, profile, err := resolveActionStore(cmd)
	if err != nil {
		return err
	}
	ctx, cancel := actionContext(cmd)
	defer cancel()
	action, err := store.FindAction(ctx, actionID)
	if errors.Is(err, ports.ErrActionNotFound) {
		return fmt.Errorf("action %q not found", actionID)
	}
	if err != nil {
		return fmt.Errorf("find action: %w", err)
	}
	if err := validateReapplyParent(ctx, store, root, action); err != nil {
		return err
	}
	if profile.Policy.MaxActionDepth > 0 && int(action.ChainDepth+1) > profile.Policy.MaxActionDepth {
		return fmt.Errorf("reapply chain depth %d exceeds configured max_action_depth %d", action.ChainDepth+1, profile.Policy.MaxActionDepth)
	}
	plan, err := planner.BuildReapply(root, action.ActionID, action.ChainDepth, toolVersion())
	if err != nil {
		return fmt.Errorf("build reapply plan: %w", err)
	}
	if err := ensureParentDir(output); err != nil {
		return err
	}
	if err := planner.WriteFileLimited(plan, output, profile.Policy.MaxPlanBytes); err != nil {
		return fmt.Errorf("write reapply plan: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "plan_id:         %s\n", plan.PlanID)
	fmt.Fprintf(cmd.OutOrStdout(), "mode:            %s\n", plan.Mode)
	fmt.Fprintf(cmd.OutOrStdout(), "digest:          sha256:%s\n", planner.ShortDigest(plan.Digest))
	fmt.Fprintf(cmd.OutOrStdout(), "root_digest:     sha256:%s\n", planner.ShortDigest(plan.RootPlanDigest))
	fmt.Fprintf(cmd.OutOrStdout(), "parent_action:   %s\n", plan.ParentActionID)
	fmt.Fprintf(cmd.OutOrStdout(), "chain_depth:     %d\n", plan.ChainDepth)
	fmt.Fprintf(cmd.OutOrStdout(), "ops:             %d\n", len(plan.Operations))
	fmt.Fprintf(cmd.OutOrStdout(), "written:         %s\n", output)
	return nil
}

func resolveActionStore(cmd *cobra.Command) (ports.ActionStore, *config.Profile, error) {
	backend, profile, err := resolveBackend(cmd)
	if err != nil {
		return nil, nil, err
	}
	store, ok := backend.(ports.ActionStore)
	if !ok {
		return nil, nil, fmt.Errorf("backend %q does not implement ActionStore", backend.Name())
	}
	return store, profile, nil
}

func actionContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	timeoutValue, _ := cmd.Flags().GetString("timeout")
	duration, err := time.ParseDuration(timeoutValue)
	if err != nil || duration <= 0 {
		duration = 30 * time.Second
	}
	return context.WithTimeout(cmd.Context(), duration)
}

func validateReapplyParent(ctx context.Context, store ports.ActionStore, root *planner.Plan, action *ports.Action) error {
	if root.Mode != planner.ModeRevert || root.ExecutionClass != planner.ClassSafe {
		return fmt.Errorf("root plan must be a safe revert plan")
	}
	if action.ActionType != "REVERT" || action.TargetState != "ORIGINAL_REVERTED" {
		return fmt.Errorf("action %s cannot be reapplied: expected REVERT/ORIGINAL_REVERTED, got %s/%s", action.ActionID, action.ActionType, action.TargetState)
	}
	if action.PlanDigest != root.Digest || action.RootPlanDigest != root.Digest {
		return fmt.Errorf("action %s does not belong to root plan %s", action.ActionID, root.PlanID)
	}
	latest, err := store.LatestAction(ctx, root.Digest)
	if errors.Is(err, ports.ErrActionNotFound) {
		return fmt.Errorf("no successful actions found for root plan %s", root.PlanID)
	}
	if err != nil {
		return fmt.Errorf("find latest action: %w", err)
	}
	if latest.ActionID != action.ActionID {
		return fmt.Errorf("action %s is not the latest successful action (latest is %s)", action.ActionID, latest.ActionID)
	}
	return nil
}

func printAction(cmd *cobra.Command, action *ports.Action) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "action_id:       %s\n", action.ActionID)
	fmt.Fprintf(out, "plan_id:         %s\n", action.PlanID)
	fmt.Fprintf(out, "action_type:     %s\n", action.ActionType)
	fmt.Fprintf(out, "target_state:    %s\n", action.TargetState)
	fmt.Fprintf(out, "chain_depth:     %d\n", action.ChainDepth)
	fmt.Fprintf(out, "parent_action:   %s\n", valueOrDash(action.ParentActionID))
	fmt.Fprintf(out, "root_digest:     %s\n", action.RootPlanDigest)
	fmt.Fprintf(out, "plan_digest:     %s\n", action.PlanDigest)
	fmt.Fprintf(out, "source_txn:      %s\n", action.SourceNativeTransactionID)
	fmt.Fprintf(out, "execution_class: %s\n", action.ExecutionClass)
	fmt.Fprintf(out, "operator:        %s\n", action.OperatorName)
	fmt.Fprintf(out, "reason:          %s\n", valueOrDash(action.Reason))
	fmt.Fprintf(out, "tool_version:    %s\n", action.ToolVersion)
	fmt.Fprintf(out, "created_at:      %s\n", action.CreatedAt.Format(time.RFC3339Nano))
}

func valueOrDash(value string) string {
	if value == "" {
		return "-"
	}
	return value
}
