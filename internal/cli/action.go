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
	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"
)

func init() { Register(newActionCmd) }

func newActionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "action",
		Short: "Inspect successful actions and generate alternating plans",
	}
	c.AddCommand(newActionShowCmd(), newActionVerifyCmd(), newActionReapplyCmd(), newActionRevertCmd())
	return c
}

func newActionVerifyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "verify",
		Short: "Resolve a commit-unknown action against its exact plan",
		RunE:  runActionVerify,
	}
	c.Flags().String("action-id", "", "action id printed by the failed apply")
	c.Flags().String("plan", "", "exact plan file used by the failed apply")
	c.Flags().Duration("wait", 5*time.Second, "settling window before declaring NOT_COMMITTED")
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

func newActionRevertCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "revert",
		Short: "Generate a safe chained revert plan from the latest reapply action",
		RunE:  runActionRevert,
	}
	c.Flags().String("action-id", "", "latest reapply action id (ulid)")
	c.Flags().String("root-plan", "", "root revert plan file")
	c.Flags().String("output", "", "output chained revert plan file")
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

func runActionVerify(cmd *cobra.Command, _ []string) error {
	actionID, _ := cmd.Flags().GetString("action-id")
	planPath, _ := cmd.Flags().GetString("plan")
	wait, _ := cmd.Flags().GetDuration("wait")
	if actionID == "" {
		return fmt.Errorf("--action-id is required")
	}
	if _, err := ulid.ParseStrict(actionID); err != nil {
		return fmt.Errorf("--action-id is not a valid ULID: %w", err)
	}
	if planPath == "" {
		return fmt.Errorf("--plan is required to bind verification to the exact plan")
	}
	if wait < 0 {
		return fmt.Errorf("--wait must not be negative")
	}
	plan, err := planner.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	backend, _, err := resolveBackend(cmd)
	if err != nil {
		result := &ports.ActionVerification{Status: ports.ActionIndeterminate, ActionID: actionID, PlanID: plan.PlanID, Message: "target could not be queried: " + err.Error()}
		_ = printVerification(cmd, result)
		return fmt.Errorf("action outcome is indeterminate: %w", err)
	}
	store, ok := backend.(ports.ActionStore)
	if !ok {
		result := &ports.ActionVerification{Status: ports.ActionIndeterminate, ActionID: actionID, PlanID: plan.PlanID, Message: "backend does not support action lookup"}
		_ = printVerification(cmd, result)
		return fmt.Errorf("action outcome is indeterminate: backend %q does not implement ActionStore", backend.Name())
	}
	identity, ok := backend.(ports.TargetIdentifier)
	if !ok {
		result := &ports.ActionVerification{Status: ports.ActionIndeterminate, ActionID: actionID, PlanID: plan.PlanID, Message: "backend cannot prove target instance identity"}
		_ = printVerification(cmd, result)
		return fmt.Errorf("action outcome is indeterminate: backend %q does not expose target identity", backend.Name())
	}
	ctx, cancel := actionContext(cmd)
	defer cancel()
	result := verifyActionOutcome(ctx, store, identity.TargetInstanceID(), actionID, plan, wait, 250*time.Millisecond)
	if err := printVerification(cmd, result); err != nil {
		return err
	}
	if result.Status == ports.ActionIndeterminate {
		return fmt.Errorf("action outcome is indeterminate; do not retry")
	}
	return nil
}

func verifyActionOutcome(ctx context.Context, store ports.ActionStore, targetInstanceID, actionID string, plan *planner.Plan, wait, pollInterval time.Duration) *ports.ActionVerification {
	result := &ports.ActionVerification{Status: ports.ActionIndeterminate, ActionID: actionID}
	if plan == nil {
		result.Message = "plan is nil"
		return result
	}
	result.PlanID = plan.PlanID
	if plan.Source.InstanceID == "" || targetInstanceID == "" || plan.Source.InstanceID != targetInstanceID {
		result.Message = fmt.Sprintf("target instance %q does not match plan instance %q; do not retry", targetInstanceID, plan.Source.InstanceID)
		return result
	}
	deadline := time.Now().Add(wait)
	for {
		action, err := store.FindAction(ctx, actionID)
		switch {
		case err == nil:
			result.Action = action
			if action.ActionID != actionID || action.PlanID != plan.PlanID || action.PlanDigest != plan.Digest || action.SourceNativeTransactionID != plan.Source.NativeTransactionID {
				result.Message = "marker exists but does not match action id, plan id, digest, and source transaction; do not retry"
				return result
			}
			result.Status = ports.ActionCommitted
			result.Message = "matching marker committed atomically with the compensation; do not retry"
			return result
		case !errors.Is(err, ports.ErrActionNotFound):
			result.Message = "target query failed: " + err.Error() + "; do not retry"
			return result
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			result.Status = ports.ActionNotCommitted
			result.Message = "no marker appeared during the settling window; rerun plan check before any new apply"
			return result
		}
		delay := pollInterval
		if delay <= 0 || delay > remaining {
			delay = remaining
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			result.Message = "verification context ended: " + ctx.Err().Error() + "; do not retry"
			return result
		case <-timer.C:
		}
	}
}

func printVerification(cmd *cobra.Command, result *ports.ActionVerification) error {
	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		encoder := json.NewEncoder(cmd.OutOrStdout())
		encoder.SetIndent("", "  ")
		return encoder.Encode(result)
	}
	if format != "table" {
		return fmt.Errorf("--format must be table or json, got %q", format)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "status:      %s\n", result.Status)
	fmt.Fprintf(cmd.OutOrStdout(), "action_id:   %s\n", result.ActionID)
	fmt.Fprintf(cmd.OutOrStdout(), "plan_id:     %s\n", result.PlanID)
	fmt.Fprintf(cmd.OutOrStdout(), "message:     %s\n", result.Message)
	return nil
}

func runActionReapply(cmd *cobra.Command, _ []string) error {
	return runActionNext(cmd, planner.ModeReapply)
}

func runActionRevert(cmd *cobra.Command, _ []string) error {
	return runActionNext(cmd, planner.ModeRevert)
}

func runActionNext(cmd *cobra.Command, mode planner.Mode) error {
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
	wantType, wantState := "REVERT", "ORIGINAL_REVERTED"
	if mode == planner.ModeRevert {
		wantType, wantState = "REAPPLY", "ORIGINAL_APPLIED"
	}
	if err := validateActionParent(ctx, store, root, action, wantType, wantState); err != nil {
		return err
	}
	if profile.Policy.MaxActionDepth > 0 && int(action.ChainDepth+1) > profile.Policy.MaxActionDepth {
		return fmt.Errorf("%s chain depth %d exceeds configured max_action_depth %d", mode, action.ChainDepth+1, profile.Policy.MaxActionDepth)
	}
	var plan *planner.Plan
	switch mode {
	case planner.ModeReapply:
		plan, err = planner.BuildReapply(root, action.ActionID, action.ChainDepth, toolVersion())
	case planner.ModeRevert:
		plan, err = planner.BuildChainedRevert(root, action.ActionID, action.ChainDepth, toolVersion())
	default:
		return fmt.Errorf("unsupported action plan mode %q", mode)
	}
	if err != nil {
		return fmt.Errorf("build %s plan: %w", mode, err)
	}
	if err := ensureParentDir(output); err != nil {
		return err
	}
	if err := planner.WriteFileLimited(plan, output, profile.Policy.MaxPlanBytes); err != nil {
		return fmt.Errorf("write %s plan: %w", mode, err)
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

func validateActionParent(ctx context.Context, store ports.ActionStore, root *planner.Plan, action *ports.Action, wantType, wantState string) error {
	if root.Mode != planner.ModeRevert || root.ExecutionClass != planner.ClassSafe {
		return fmt.Errorf("root plan must be a safe revert plan")
	}
	if action.ActionType != wantType || action.TargetState != wantState {
		return fmt.Errorf("action %s cannot continue chain: expected %s/%s, got %s/%s", action.ActionID, wantType, wantState, action.ActionType, action.TargetState)
	}
	if action.RootPlanDigest != root.Digest || action.SourceNativeTransactionID != root.Source.NativeTransactionID {
		return fmt.Errorf("action %s does not belong to root plan %s", action.ActionID, root.PlanID)
	}
	if action.ChainDepth == 0 && action.PlanDigest != root.Digest {
		return fmt.Errorf("root action %s plan digest does not match root plan %s", action.ActionID, root.PlanID)
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
