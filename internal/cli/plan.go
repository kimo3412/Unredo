package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/oklog/ulid/v2"
	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/buildinfo"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/planner"
	"github.com/girimi/unredo/internal/ports"
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
		Short: "Generate a self-contained compensation plan from a binlog transaction",
		RunE:  runPlanCreate,
	}
	c.Flags().String("binlog", "", "starting binlog file")
	c.Flags().Uint32("from-pos", 4, "starting position in --binlog")
	c.Flags().String("txn", "", "transaction id (uuid:gnum)")
	c.Flags().String("mode", "revert", "revert|reapply")
	c.Flags().String("output", "", "output plan file (default: ./undo-<txn>.json)")
	return c
}

func newPlanCheckCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "check",
		Short: "Verify a plan is still safe to apply (M2)",
		RunE:  runPlanCheck,
		Args:  cobra.MinimumNArgs(1),
	}
	c.Flags().Bool("show-conflicts", false, "include per-conflict details in table output")
	return c
}

// runPlanCheck loads a plan file and asks the executor to verify the
// target database. It is read-only and never writes; conflicts are
// aggregated and the CLI exits non-zero on anything other than READY.
func runPlanCheck(cmd *cobra.Command, args []string) error {
	planPath := args[0]
	plan, err := planner.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	be, _, err := resolveBackend(cmd)
	if err != nil {
		return err
	}
	planExecutor, ok := be.(ports.PlanExecutor)
	if !ok {
		return fmt.Errorf("backend %q does not implement PlanExecutor", be.Name())
	}

	timeoutStr, _ := cmd.Flags().GetString("timeout")
	dur, _ := time.ParseDuration(timeoutStr)
	if dur == 0 {
		dur = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), dur)
	defer cancel()

	result, err := planExecutor.Check(ctx, *plan.ToPorts())
	if err != nil {
		return fmt.Errorf("check: %w", err)
	}

	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	showConflicts, _ := cmd.Flags().GetBool("show-conflicts")
	printCheckHuman(cmd, result, showConflicts)

	switch result.Status {
	case "READY":
		return nil
	case "STALE_SCHEMA":
		return fmt.Errorf("plan is stale: schema drifted")
	case "SOURCE_MISMATCH":
		return fmt.Errorf("plan was generated for a different instance")
	case "CONFLICT":
		return fmt.Errorf("plan has %d conflict(s); rerun with --show-conflicts", len(result.Conflicts))
	default:
		return fmt.Errorf("plan check returned %s", result.Status)
	}
}

func newPlanApplyCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "apply",
		Short: "Execute a compensation plan (M2)",
		RunE:  runPlanApply,
		Args:  cobra.MinimumNArgs(1),
	}
	c.Flags().String("confirm-sha", "", "first 8 chars of the plan digest")
	c.Flags().Bool("non-interactive", false, "do not prompt")
	c.Flags().String("accept-risk", "", "first 8 chars of the resolved plan digest (unsafe plans only)")
	c.Flags().String("operator", os.Getenv("USER"), "operator name recorded in the action marker")
	c.Flags().String("reason", "", "free-text reason recorded in the action marker")
	return c
}

// runPlanApply loads a plan, mints an action id, and calls the
// backend's Apply. The whole operation runs in a single InnoDB
// transaction so the marker and data writes commit together.
func runPlanApply(cmd *cobra.Command, args []string) error {
	planPath := args[0]
	plan, err := planner.ReadFile(planPath)
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	be, _, err := resolveBackend(cmd)
	if err != nil {
		return err
	}
	executor, ok := be.(ports.PlanExecutor)
	if !ok {
		return fmt.Errorf("backend %q does not implement PlanExecutor", be.Name())
	}

	confirm, _ := cmd.Flags().GetString("confirm-sha")
	acceptRisk, _ := cmd.Flags().GetString("accept-risk")
	nonInteractive, _ := cmd.Flags().GetBool("non-interactive")
	operator, _ := cmd.Flags().GetString("operator")
	reason, _ := cmd.Flags().GetString("reason")
	if operator == "" {
		operator = os.Getenv("USER")
	}
	if operator == "" {
		return fmt.Errorf("--operator is required (or set $USER / $USERNAME)")
	}

	if plan.ExecutionClass == "unsafe_resolved" {
		if acceptRisk == "" {
			return fmt.Errorf("resolved plan requires --accept-risk <short-digest>")
		}
		if acceptRisk != planner.ShortDigest(plan.Digest) {
			return fmt.Errorf("--accept-risk %q does not match plan digest", acceptRisk)
		}
	}
	if !nonInteractive {
		fmt.Fprintf(cmd.OutOrStdout(),
			"About to apply plan %s\n  mode:        %s\n  digest:      sha256:%s\n  operations:  %d\n  class:       %s\n  operator:    %s\n",
			plan.PlanID, plan.Mode, planner.ShortDigest(plan.Digest), len(plan.Operations), plan.ExecutionClass, operator)
		fmt.Fprintf(cmd.OutOrStdout(), "Type the first 8 hex chars of the digest to confirm: ")
		var got string
		if _, err := fmt.Scanln(&got); err != nil {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if got != planner.ShortDigest(plan.Digest) {
			return fmt.Errorf("confirmation %q does not match; aborting", got)
		}
	} else {
		if confirm == "" {
			return fmt.Errorf("--confirm-sha is required with --non-interactive")
		}
		if confirm != planner.ShortDigest(plan.Digest) {
			return fmt.Errorf("--confirm-sha %q does not match plan digest sha256:%s", confirm, planner.ShortDigest(plan.Digest))
		}
	}

	timeoutStr, _ := cmd.Flags().GetString("timeout")
	dur, _ := time.ParseDuration(timeoutStr)
	if dur == 0 {
		dur = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), dur)
	defer cancel()

	actionID := newActionID()
	req := ports.ApplyRequest{
		ActionID:     actionID,
		OperatorName: operator,
		Reason:       reason,
		Confirm:      confirm,
	}
	result, err := executor.Apply(ctx, *plan.ToPorts(), req)
	if err != nil {
		if errors.Is(err, ports.ErrCommitUnknown) {
			if result.ActionID != "" {
				actionID = result.ActionID
			}
			printCommitUnknownRecovery(cmd, actionID, planPath)
		}
		return fmt.Errorf("apply: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "action_id:    %s\n", actionID)
	fmt.Fprintf(cmd.OutOrStdout(), "gtid:         %s\n", result.CompensatingGTID)
	fmt.Fprintf(cmd.OutOrStdout(), "affected:     %d\n", result.AffectedRows)
	if result.GTIDCorrelationWarning != "" {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: compensation committed, but exact GTID correlation failed: %s\n", result.GTIDCorrelationWarning)
	}
	return nil
}

func printCommitUnknownRecovery(cmd *cobra.Command, actionID, planPath string) {
	out := cmd.OutOrStdout()
	fmt.Fprintln(out, "status:       COMMIT_UNKNOWN")
	fmt.Fprintf(out, "action_id:    %s\n", actionID)
	fmt.Fprintf(out, "plan:         %s\n", planPath)
	fmt.Fprintln(out, "retry:        FORBIDDEN until verification")
	fmt.Fprintf(out, "verify:       unredo action verify --action-id %s --plan %s\n", actionID, planPath)
}

func newPlanResolveCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "resolve",
		Short: "Produce an audited unsafe plan from explicit conflict decisions",
		RunE:  runPlanResolve,
		Args:  cobra.MinimumNArgs(1),
	}
	c.Flags().String("output", "", "output resolved plan file")
	c.Flags().String("from-json", "", "non-interactive resolution decision file")
	c.Flags().String("operator", "", "operator recording the resolution")
	c.Flags().String("reason", "", "incident or reason for unsafe resolution")
	return c
}

type resolutionInput struct {
	Operator    string               `json:"operator"`
	Reason      string               `json:"reason"`
	Resolutions []planner.Resolution `json:"resolutions"`
}

func runPlanResolve(cmd *cobra.Command, args []string) error {
	parent, err := planner.ReadFile(args[0])
	if err != nil {
		return fmt.Errorf("read parent plan: %w", err)
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "" {
		return fmt.Errorf("--output is required")
	}
	backend, profile, err := resolveBackend(cmd)
	if err != nil {
		return err
	}
	executor, ok := backend.(ports.PlanExecutor)
	if !ok {
		return fmt.Errorf("backend %q does not implement PlanExecutor", backend.Name())
	}
	ctx, cancel := commandContext(cmd, 30*time.Second)
	defer cancel()
	check, err := executor.Check(ctx, *parent.ToPorts())
	if err != nil {
		return fmt.Errorf("check parent plan: %w", err)
	}
	if check.Status != "CONFLICT" {
		return fmt.Errorf("parent plan status is %s; only row conflicts can be resolved", check.Status)
	}

	input, err := collectResolutions(cmd, parent, check)
	if err != nil {
		return err
	}
	resolved, err := planner.BuildResolved(parent, check, planner.ResolveOptions{
		Operator: input.Operator, Reason: input.Reason,
		ToolVersion: toolVersion(), Decisions: input.Resolutions,
	})
	if err != nil {
		return fmt.Errorf("resolve plan: %w", err)
	}
	if err := ensureParentDir(output); err != nil {
		return err
	}
	if err := planner.WriteFileLimited(resolved, output, profile.Policy.MaxPlanBytes); err != nil {
		return fmt.Errorf("write resolved plan: %w", err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "plan_id:          %s\n", resolved.PlanID)
	fmt.Fprintf(cmd.OutOrStdout(), "class:            %s\n", resolved.ExecutionClass)
	fmt.Fprintf(cmd.OutOrStdout(), "digest:           sha256:%s\n", planner.ShortDigest(resolved.Digest))
	fmt.Fprintf(cmd.OutOrStdout(), "parent_digest:    %s\n", resolved.ParentPlanDigest)
	fmt.Fprintf(cmd.OutOrStdout(), "resolutions:      %d\n", len(resolved.Resolutions))
	fmt.Fprintf(cmd.OutOrStdout(), "operations:       %d\n", len(resolved.Operations))
	fmt.Fprintf(cmd.OutOrStdout(), "written:          %s\n", output)
	fmt.Fprintf(cmd.OutOrStdout(), "apply requires:   --confirm-sha %s --accept-risk %s\n", planner.ShortDigest(resolved.Digest), planner.ShortDigest(resolved.Digest))
	return nil
}

func collectResolutions(cmd *cobra.Command, parent *planner.Plan, check *ports.CheckResult) (*resolutionInput, error) {
	fromJSON, _ := cmd.Flags().GetString("from-json")
	operator, _ := cmd.Flags().GetString("operator")
	reason, _ := cmd.Flags().GetString("reason")
	var input resolutionInput
	if fromJSON != "" {
		file, err := os.Open(fromJSON)
		if err != nil {
			return nil, fmt.Errorf("open resolution file: %w", err)
		}
		defer file.Close()
		decoder := json.NewDecoder(io.LimitReader(file, 4*1024*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			return nil, fmt.Errorf("decode resolution file: %w", err)
		}
	} else {
		var err error
		input.Resolutions, err = promptResolutions(cmd, parent, check)
		if err != nil {
			return nil, err
		}
	}
	if operator != "" {
		input.Operator = operator
	}
	if reason != "" {
		input.Reason = reason
	}
	if input.Operator == "" || input.Reason == "" {
		return nil, fmt.Errorf("resolution requires --operator and --reason (or operator/reason in --from-json)")
	}
	grouped := conflictsBySequence(check.Conflicts)
	for i := range input.Resolutions {
		if input.Resolutions[i].ConflictDigest == "" {
			input.Resolutions[i].ConflictDigest = planner.ConflictDigest(parent.Digest, input.Resolutions[i].OperationSequence, grouped[input.Resolutions[i].OperationSequence])
		}
	}
	return &input, nil
}

func promptResolutions(cmd *cobra.Command, parent *planner.Plan, check *ports.CheckResult) ([]planner.Resolution, error) {
	grouped := conflictsBySequence(check.Conflicts)
	sequences := make([]int, 0, len(grouped))
	for sequence := range grouped {
		sequences = append(sequences, sequence)
	}
	sort.Ints(sequences)
	reader := bufio.NewReader(cmd.InOrStdin())
	out := cmd.OutOrStdout()
	resolutions := make([]planner.Resolution, 0, len(sequences))
	for _, sequence := range sequences {
		conflicts := grouped[sequence]
		digest := planner.ConflictDigest(parent.Digest, sequence, conflicts)
		fmt.Fprintf(out, "operation %d conflict %s\n", sequence, digest)
		for _, conflict := range conflicts {
			fmt.Fprintf(out, "  %s %s column=%s: %s\n", conflict.Table, conflict.Kind, valueOrDash(conflict.Column), conflict.Message)
		}
		fmt.Fprint(out, "decision [skip/overwrite/abort]: ")
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read resolution: %w", err)
		}
		decision := planner.ResolutionDecision(strings.ToLower(strings.TrimSpace(line)))
		switch decision {
		case planner.DecisionSkip, planner.DecisionOverwrite:
		case planner.DecisionAbort:
			return nil, fmt.Errorf("resolution aborted at operation %d", sequence)
		default:
			return nil, fmt.Errorf("invalid decision %q for operation %d", decision, sequence)
		}
		resolutions = append(resolutions, planner.Resolution{OperationSequence: sequence, Decision: decision, ConflictDigest: digest})
	}
	return resolutions, nil
}

func conflictsBySequence(conflicts []ports.Conflict) map[int][]ports.Conflict {
	grouped := make(map[int][]ports.Conflict)
	for _, conflict := range conflicts {
		grouped[conflict.OperationSequence] = append(grouped[conflict.OperationSequence], conflict)
	}
	return grouped
}

func commandContext(cmd *cobra.Command, fallback time.Duration) (context.Context, context.CancelFunc) {
	timeoutValue, _ := cmd.Flags().GetString("timeout")
	duration, err := time.ParseDuration(timeoutValue)
	if err != nil || duration <= 0 {
		duration = fallback
	}
	return context.WithTimeout(cmd.Context(), duration)
}

// runPlanCreate reads a transaction from the binlog, asks the planner
// to build a self-contained plan, and writes it to disk. It does not
// touch the target database; that is what `plan check` and `plan apply`
// do in M2.
func runPlanCreate(cmd *cobra.Command, _ []string) error {
	be, profile, err := resolveBackend(cmd)
	if err != nil {
		return err
	}
	src, ok := be.(ports.ChangeSource)
	if !ok {
		return fmt.Errorf("backend %q does not implement ChangeSource", be.Name())
	}
	insp, ok := be.(ports.SchemaInspector)
	if !ok {
		return fmt.Errorf("backend %q does not implement SchemaInspector", be.Name())
	}

	txnID, _ := cmd.Flags().GetString("txn")
	if txnID == "" {
		return fmt.Errorf("--txn is required")
	}
	binlog, _ := cmd.Flags().GetString("binlog")
	fromPos, _ := cmd.Flags().GetUint32("from-pos")
	modeStr, _ := cmd.Flags().GetString("mode")
	output, _ := cmd.Flags().GetString("output")

	mode := planner.Mode(modeStr)
	if mode != planner.ModeRevert && mode != planner.ModeReapply {
		return fmt.Errorf("--mode must be revert or reapply, got %q", modeStr)
	}
	if output == "" {
		output = defaultPlanPath(txnID, mode)
	}
	if err := ensureParentDir(output); err != nil {
		return err
	}

	cursor, _ := json.Marshal(map[string]interface{}{
		"file":      binlog,
		"start_pos": fromPos,
	})

	// Bound the read by the global --timeout so a misbehaving binlog
	// can't hang the CLI.
	timeoutStr, _ := cmd.Flags().GetString("timeout")
	dur, _ := time.ParseDuration(timeoutStr)
	if dur == 0 {
		dur = 30 * time.Second
	}
	findCtx, cancel := context.WithTimeout(cmd.Context(), dur)
	defer cancel()

	// Find the transaction through the change source.
	beImpl := be
	instanceID := ""
	if iid, ok := beImpl.(interface{ InstanceID() string }); ok {
		instanceID = iid.InstanceID()
	}
	ref := core.TransactionRef{
		Backend:             be.Name(),
		InstanceID:          instanceID,
		NativeTransactionID: txnID,
		Cursor:              cursor,
	}
	txn, err := src.Find(findCtx, ref)
	if err != nil {
		return fmt.Errorf("find transaction: %w", err)
	}

	// The planner needs to look up schema and fingerprints. We give it
	// a closure that delegates to the backend.
	deps := planner.Deps{
		SchemaFor: func(t core.TableRef) (core.TableSchema, error) {
			return insp.InspectTable(cmd.Context(), t)
		},
		FingerprintFor: func(t core.TableRef) (core.SchemaFingerprint, error) {
			return insp.Fingerprint(cmd.Context(), t)
		},
		ToolVersion: toolVersion(),
	}

	plan, err := planner.Build(txn, mode, deps)
	if err != nil {
		return fmt.Errorf("build plan: %w", err)
	}
	if err := planner.WriteFileLimited(plan, output, profile.Policy.MaxPlanBytes); err != nil {
		return fmt.Errorf("write plan: %w", err)
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "plan_id:   %s\n", plan.PlanID)
	fmt.Fprintf(out, "mode:      %s\n", plan.Mode)
	fmt.Fprintf(out, "digest:    sha256:%s\n", planner.ShortDigest(plan.Digest))
	fmt.Fprintf(out, "ops:       %d\n", len(plan.Operations))
	fmt.Fprintf(out, "written:   %s\n", output)
	return nil
}

func defaultPlanPath(txn string, mode planner.Mode) string {
	safeTxn := txn
	for i, r := range safeTxn {
		if r == ':' {
			safeTxn = safeTxn[:i] + "-" + safeTxn[i+1:]
		}
	}
	name := fmt.Sprintf("%s-%s.json", mode, safeTxn)
	return filepath.Join(".", name)
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o700)
}

func toolVersion() string {
	return buildinfo.Version
}

func printCheckHuman(cmd *cobra.Command, r *ports.CheckResult, showConflicts bool) {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "status:        %s\n", r.Status)
	fmt.Fprintf(out, "plan_digest:   %s\n", r.PlanDigest)
	fmt.Fprintf(out, "target:        %s\n", r.TargetInstance)
	fmt.Fprintf(out, "operations:    %d\n", r.OperationsTotal)
	if len(r.SchemaChecks) > 0 {
		fmt.Fprintf(out, "schema:\n")
		for _, s := range r.SchemaChecks {
			marker := "OK"
			if !s.Match {
				marker = "DRIFT"
			}
			fmt.Fprintf(out, "  %s %-30s plan=%s actual=%s\n", marker, s.Table, shortOrDash(s.PlanDigest), shortOrDash(s.ActualDigest))
		}
	}
	if len(r.Conflicts) > 0 {
		fmt.Fprintf(out, "conflicts: %d\n", len(r.Conflicts))
		if showConflicts {
			for _, c := range r.Conflicts {
				fmt.Fprintf(out, "  op=%-3d %-25s %-15s col=%s msg=%s\n",
					c.OperationSequence, c.Table, c.Kind, c.Column, c.Message)
			}
		}
	}
}

func shortOrDash(s string) string {
	if s == "" {
		return "-"
	}
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// newActionID mints a fresh ULID. M2 hands these out to action
// markers; M3 will record chain depth against the existing root.
func newActionID() string {
	return ulid.Make().String()
}
