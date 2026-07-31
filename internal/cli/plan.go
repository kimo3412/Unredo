package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

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
