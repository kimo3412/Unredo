package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/backends/mysql"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/executor"
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
	src, ok := be.(ports.ChangeSource)
	if !ok {
		return fmt.Errorf("backend %q does not implement ChangeSource", be.Name())
	}
	_ = src // source unused by check, but used to confirm backend is alive

	timeoutStr, _ := cmd.Flags().GetString("timeout")
	dur, _ := time.ParseDuration(timeoutStr)
	if dur == 0 {
		dur = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), dur)
	defer cancel()

	reader := mysqlBackendCheckReader(be)
	if reader == nil {
		return fmt.Errorf("backend %q does not support plan check", be.Name())
	}
	result, err := executor.Check(ctx, plan.ToPorts(), reader)
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
	case executor.StatusReady:
		return nil
	case executor.StatusStaleSchema:
		return fmt.Errorf("plan is stale: schema drifted")
	case executor.StatusSourceMismatch:
		return fmt.Errorf("plan was generated for a different instance")
	case executor.StatusConflict:
		return fmt.Errorf("plan has %d conflict(s); rerun with --show-conflicts", len(result.Conflicts))
	default:
		return fmt.Errorf("plan check returned %s", result.Status)
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
			"%s: scheduled for a later milestone (M2 check/apply, M3 reapply). Run `unredo doctor` or `unredo txn list/show` to validate the M0 binlog path.\n", name)
		return nil
	}
}

// runPlanCreate reads a transaction from the binlog, asks the planner
// to build a self-contained plan, and writes it to disk. It does not
// touch the target database; that is what `plan check` and `plan apply`
// do in M2.
func runPlanCreate(cmd *cobra.Command, _ []string) error {
	be, _, err := resolveBackend(cmd)
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
	if err := planner.WriteFile(plan, output); err != nil {
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
	// M1: a static version. M3 will read this from build-time -ldflags.
	return "0.1.0-m1"
}

// mysqlBackendCheckReader pulls the MySQL backend's check reader out
// of the resolved backend. Other backends would provide their own
// type assertion here; for now MySQL is the only one.
func mysqlBackendCheckReader(be ports.Backend) executor.Reader {
	mbe, ok := be.(*mysql.Backend)
	if !ok {
		return nil
	}
	return mysql.NewCheckReaderFromBackend(mbe)
}

func printCheckHuman(cmd *cobra.Command, r *executor.CheckResult, showConflicts bool) {
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
