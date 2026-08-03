//go:build integration
// +build integration

// Package integration_test runs the end-to-end M0/M1 path against a
// real MySQL 8 instance. Run with:
//
//	go test -tags=integration ./tests/integration/...
//
// The test connects to the MySQL instance configured in
// scripts/init_m0_schema.sql, inserts a fresh fixture, asks the
// unredo CLI commands to find the resulting transaction, and asserts
// the planner produced the expected plan.
package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/config"
	"github.com/girimi/unredo/internal/planner"
)

const (
	readerUser   = "unredo_reader"
	readerPass   = "unredo_reader_pw"
	executorUser = "unredo_executor"
	executorPass = "unredo_executor_pw"
	rootPass     = "123456"
)

func TestEndToEndPlanCreate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	// Seed a row with a unique marker so concurrent tests cannot collide.
	// status is varchar(16), so keep the marker short.
	marker := fmt.Sprintf("m1-it-%d", time.Now().Unix()%1000000)
	if len(marker) > 16 {
		marker = marker[:16]
	}
	markerUser := 900000 + int(time.Now().UnixNano()%99999)

	_, err := execConn.Exec(
		"INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (?, ?, ?)",
		markerUser, marker, 1.23,
	)
	if err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	t.Cleanup(func() {
		_, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE status = ?", marker)
	})

	// Capture the GTID that executed our INSERT.
	gtid, err := readExecutedGTIDFor(rootConn, marker, markerUser)
	if err != nil {
		t.Fatalf("locate fixture GTID: %v", err)
	}
	if gtid == "" {
		t.Fatal("could not find the GTID of our fixture insert")
	}
	t.Logf("fixture GTID: %s", gtid)
	binlogFile, err := readCurrentBinlogFile(rootConn)
	if err != nil {
		t.Fatalf("read current binlog: %v", err)
	}
	t.Logf("current binlog file: %s", binlogFile)

	// Build a plan via the unredo binary so we exercise the CLI surface.
	repoRoot := repoRoot(t)
	binary := filepath.Join(repoRoot, "bin", "unredo.exe")
	if _, err := os.Stat(binary); err != nil {
		t.Fatalf("expected built binary at %s; run `make build` first: %v", binary, err)
	}

	planPath := filepath.Join(t.TempDir(), "plan.json")
	cmd := exec.Command(binary,
		"--config", "unredo.yaml",
		"--profile", "local",
		"plan", "create",
		"--binlog", binlogFile,
		"--from-pos", "4",
		"--txn", gtid,
		"--output", planPath,
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"UNREDO_READER_PASSWORD="+readerPass,
		"UNREDO_EXECUTOR_PASSWORD="+executorPass,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("plan create: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "mode:      revert") {
		t.Fatalf("missing expected output, got: %s", out)
	}

	// Read the plan back and check the digest round-trip.
	plan, err := planner.ReadFile(planPath)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if plan.FormatVersion != 1 {
		t.Errorf("unexpected format version: %d", plan.FormatVersion)
	}
	if plan.Mode != planner.ModeRevert {
		t.Errorf("expected mode=revert, got %s", plan.Mode)
	}
	if len(plan.Operations) != 1 {
		t.Fatalf("expected 1 op, got %d", len(plan.Operations))
	}
	op := plan.Operations[0]
	if op.Kind != "delete" {
		t.Errorf("revert of INSERT must produce delete, got %s", op.Kind)
	}
	if op.Table.Schema != "unredo_shop" || op.Table.Name != "orders" {
		t.Errorf("wrong table: %s", op.Table)
	}
	// The key must contain exactly the primary key column.
	if len(op.Key.Columns) != 1 || op.Key.Columns[0] != "id" {
		t.Errorf("key should be the primary id column, got %v", op.Key.Columns)
	}
	if op.Key.Values[0].Null {
		t.Errorf("key id should not be null")
	}
	// The expect image must include the marker value so the executor
	// can verify the row is still there before deleting it.
	gotMarker, _ := op.Expect.Get("status")
	if gotMarker.Null || string(gotMarker.Data) != fmt.Sprintf("%q", marker) {
		// Data is stored as raw JSON; the value should be a quoted
		// string. Tolerate either raw string or quoted form by
		// stripping surrounding quotes.
		raw := strings.Trim(string(gotMarker.Data), `"`)
		if raw != marker {
			t.Errorf("status in expect image should be %q, got %q", marker, raw)
		}
	}

	// Now run plan check on a fresh target state. The row exists, so
	// check must be READY.
	planCheck(t, planPath, "READY")

	// Mutate the row and re-check; expect CONFLICT.
	_, err = execConn.Exec("UPDATE unredo_shop.orders SET status = ? WHERE user_id = ?", marker+"-mut", markerUser)
	if err != nil {
		t.Fatalf("mutate row: %v", err)
	}
	planCheck(t, planPath, "CONFLICT")

	// Restore the row so subsequent tests see a clean state.
	_, err = execConn.Exec("UPDATE unredo_shop.orders SET status = ? WHERE user_id = ?", marker, markerUser)
	if err != nil {
		t.Fatalf("restore row: %v", err)
	}
	planCheck(t, planPath, "READY")

	// Now apply. The row must be removed and a marker row inserted.
	planApply(t, planPath, planner.ShortDigest(plan.Digest), 0, true)

	// Re-apply must be blocked by the plan_id UNIQUE.
	planApply(t, planPath, planner.ShortDigest(plan.Digest), 1, false)
}

func TestEndToEndRevertUpdateDeleteAndBinary(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	marker := fmt.Sprintf("types-%d", time.Now().UnixNano()%100000000)
	res, err := execConn.Exec(
		"INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (?, ?, ?)",
		990001, marker, "10.25",
	)
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	orderID, _ := res.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE id = ?", orderID) })

	// UPDATE revert exercises text, decimal and temporal write-back.
	if _, err := execConn.Exec("UPDATE unredo_shop.orders SET status=?, amount=? WHERE id=?", "changed", "99.75", orderID); err != nil {
		t.Fatalf("update order: %v", err)
	}
	updateGTID, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	updatePlan := createPlanForGTID(t, rootConn, updateGTID)
	plan, err := planner.ReadFile(updatePlan)
	if err != nil {
		t.Fatal(err)
	}
	planApply(t, updatePlan, planner.ShortDigest(plan.Digest), 0, true)
	var status, amount string
	if err := execConn.QueryRow("SELECT status, amount FROM unredo_shop.orders WHERE id=?", orderID).Scan(&status, &amount); err != nil {
		t.Fatal(err)
	}
	if status != marker || amount != "10.25" {
		t.Fatalf("update revert restored status=%q amount=%q; want %q, 10.25", status, amount, marker)
	}

	// DELETE revert exercises full-row INSERT write-back.
	if _, err := execConn.Exec("DELETE FROM unredo_shop.orders WHERE id=?", orderID); err != nil {
		t.Fatalf("delete order: %v", err)
	}
	deleteGTID, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	deletePlan := createPlanForGTID(t, rootConn, deleteGTID)
	plan, err = planner.ReadFile(deletePlan)
	if err != nil {
		t.Fatal(err)
	}
	planApply(t, deletePlan, planner.ShortDigest(plan.Digest), 0, true)
	if err := execConn.QueryRow("SELECT status, amount FROM unredo_shop.orders WHERE id=?", orderID).Scan(&status, &amount); err != nil {
		t.Fatal(err)
	}
	if status != marker || amount != "10.25" {
		t.Fatalf("delete revert restored status=%q amount=%q", status, amount)
	}

	// Binary and NULL must survive canonical JSON and SQL binding.
	res, err = execConn.Exec("INSERT INTO unredo_shop.large_rows (payload, note) VALUES (?, NULL)", []byte{0, 1, 255})
	if err != nil {
		t.Fatalf("seed binary: %v", err)
	}
	largeID, _ := res.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.large_rows WHERE id = ?", largeID) })
	if _, err := execConn.Exec("UPDATE unredo_shop.large_rows SET payload=?, note=? WHERE id=?", []byte{9, 8, 7}, "not-null", largeID); err != nil {
		t.Fatalf("update binary: %v", err)
	}
	binaryGTID, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	binaryPlan := createPlanForGTID(t, rootConn, binaryGTID)
	plan, err = planner.ReadFile(binaryPlan)
	if err != nil {
		t.Fatal(err)
	}
	planApply(t, binaryPlan, planner.ShortDigest(plan.Digest), 0, true)
	var payload []byte
	var note sql.NullString
	if err := execConn.QueryRow("SELECT payload, note FROM unredo_shop.large_rows WHERE id=?", largeID).Scan(&payload, &note); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(payload, []byte{0, 1, 255}) || note.Valid {
		t.Fatalf("binary revert restored payload=%v note=%#v", payload, note)
	}
}

func TestApplyConflictLeavesAllRowsUntouched(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	res1, err := execConn.Exec("INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (991001, 'base-a', 1.00)")
	if err != nil {
		t.Fatal(err)
	}
	id1, _ := res1.LastInsertId()
	res2, err := execConn.Exec("INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (991002, 'base-b', 2.00)")
	if err != nil {
		t.Fatal(err)
	}
	id2, _ := res2.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE id IN (?, ?)", id1, id2) })

	tx, err := execConn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE unredo_shop.orders SET status='changed-a' WHERE id=?", id1); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE unredo_shop.orders SET status='changed-b' WHERE id=?", id2); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	gtid, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	planPath := createPlanForGTID(t, rootConn, gtid)
	plan, err := planner.ReadFile(planPath)
	if err != nil {
		t.Fatal(err)
	}

	// Drift only the second row. Apply must fail before reverting the first.
	if _, err := execConn.Exec("UPDATE unredo_shop.orders SET status='external' WHERE id=?", id2); err != nil {
		t.Fatal(err)
	}
	planApply(t, planPath, planner.ShortDigest(plan.Digest), 1, false)
	var s1, s2 string
	if err := execConn.QueryRow("SELECT status FROM unredo_shop.orders WHERE id=?", id1).Scan(&s1); err != nil {
		t.Fatal(err)
	}
	if err := execConn.QueryRow("SELECT status FROM unredo_shop.orders WHERE id=?", id2).Scan(&s2); err != nil {
		t.Fatal(err)
	}
	if s1 != "changed-a" || s2 != "external" {
		t.Fatalf("conflicting apply changed rows: first=%q second=%q", s1, s2)
	}
}

func TestEndToEndRevertThenReapply(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	marker := fmt.Sprintf("redo-%d", time.Now().UnixNano()%100000000)
	result, err := execConn.Exec(
		"INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (?, ?, ?)",
		992001, marker, "42.50",
	)
	if err != nil {
		t.Fatalf("seed insert: %v", err)
	}
	rowID, _ := result.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE id = ?", rowID) })

	gtid, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	rootPlanPath := createPlanForGTID(t, rootConn, gtid)
	rootPlan, err := planner.ReadFile(rootPlanPath)
	if err != nil {
		t.Fatal(err)
	}
	revertOutput := planApplyCapture(t, rootPlanPath, planner.ShortDigest(rootPlan.Digest), true)
	revertActionID := outputField(t, revertOutput, "action_id")

	var count int
	if err := execConn.QueryRow("SELECT COUNT(*) FROM unredo_shop.orders WHERE id = ?", rowID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("revert left row %d present", rowID)
	}
	revertAction := actionShow(t, revertActionID)
	if revertAction.ActionType != "REVERT" || revertAction.TargetState != "ORIGINAL_REVERTED" || revertAction.ChainDepth != 0 {
		t.Fatalf("unexpected revert marker: %+v", revertAction)
	}
	if revertAction.CreatedAt.IsZero() || revertAction.CreatedAt.Location() != time.UTC {
		t.Fatalf("action timestamp must be a non-zero UTC instant: %s", revertAction.CreatedAt)
	}
	if revertAction.RootPlanDigest != rootPlan.Digest || revertAction.PlanDigest != rootPlan.Digest {
		t.Fatalf("revert marker digest mismatch: %+v", revertAction)
	}

	reapplyPath := filepath.Join(t.TempDir(), "reapply.json")
	runCLI(t, true,
		"action", "reapply",
		"--action-id", revertActionID,
		"--root-plan", rootPlanPath,
		"--output", reapplyPath,
	)
	reapplyPlan, err := planner.ReadFile(reapplyPath)
	if err != nil {
		t.Fatal(err)
	}
	if reapplyPlan.RootPlanDigest != rootPlan.Digest || reapplyPlan.ParentActionID != revertActionID || reapplyPlan.ChainDepth != 1 {
		t.Fatalf("unexpected reapply plan chain: %+v", reapplyPlan)
	}
	planCheck(t, reapplyPath, "READY")
	reapplyOutput := planApplyCapture(t, reapplyPath, planner.ShortDigest(reapplyPlan.Digest), true)
	reapplyActionID := outputField(t, reapplyOutput, "action_id")

	var gotMarker, gotAmount string
	if err := execConn.QueryRow("SELECT status, amount FROM unredo_shop.orders WHERE id = ?", rowID).Scan(&gotMarker, &gotAmount); err != nil {
		t.Fatalf("read reapplied row: %v", err)
	}
	if gotMarker != marker || gotAmount != "42.50" {
		t.Fatalf("reapply restored status=%q amount=%q", gotMarker, gotAmount)
	}
	reapplyAction := actionShow(t, reapplyActionID)
	if reapplyAction.ActionType != "REAPPLY" || reapplyAction.TargetState != "ORIGINAL_APPLIED" || reapplyAction.ChainDepth != 1 {
		t.Fatalf("unexpected reapply marker: %+v", reapplyAction)
	}
	if reapplyAction.ParentActionID != revertActionID || reapplyAction.RootPlanDigest != rootPlan.Digest || reapplyAction.PlanDigest != reapplyPlan.Digest {
		t.Fatalf("reapply marker chain mismatch: %+v", reapplyAction)
	}

	// A reapply action is a terminal state for this command: it cannot be
	// fed back into action reapply to create an unbounded same-direction chain.
	runCLI(t, false,
		"action", "reapply",
		"--action-id", reapplyActionID,
		"--root-plan", rootPlanPath,
		"--output", filepath.Join(t.TempDir(), "must-not-exist.json"),
	)
}

func TestEndToEndResolveOverwriteRequiresRiskConfirmation(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	marker := fmt.Sprintf("resolve-%d", time.Now().UnixNano()%100000000)
	result, err := execConn.Exec(
		"INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (?, ?, ?)",
		993001, marker, "10.00",
	)
	if err != nil {
		t.Fatal(err)
	}
	rowID, _ := result.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE id = ?", rowID) })

	if _, err := execConn.Exec("UPDATE unredo_shop.orders SET status = 'changed', amount = '20.00' WHERE id = ?", rowID); err != nil {
		t.Fatal(err)
	}
	gtid, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	parentPath := createPlanForGTID(t, rootConn, gtid)
	parent, err := planner.ReadFile(parentPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := execConn.Exec("UPDATE unredo_shop.orders SET status = 'external' WHERE id = ?", rowID); err != nil {
		t.Fatal(err)
	}
	planCheck(t, parentPath, "CONFLICT")

	resolutionPath := filepath.Join(t.TempDir(), "resolutions.json")
	resolutionJSON := []byte(`{"operator":"incident-dba","reason":"INC-RESOLVE-1","resolutions":[{"operation_sequence":1,"decision":"overwrite"}]}`)
	if err := os.WriteFile(resolutionPath, resolutionJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	resolvedPath := filepath.Join(t.TempDir(), "resolved.json")
	runCLI(t, true,
		"plan", "resolve", parentPath,
		"--from-json", resolutionPath,
		"--output", resolvedPath,
	)
	resolved, err := planner.ReadFile(resolvedPath)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.ExecutionClass != planner.ClassUnsafeResolved || resolved.ParentPlanDigest != parent.Digest || len(resolved.Resolutions) != 1 {
		t.Fatalf("unexpected resolved plan metadata: %+v", resolved)
	}
	planCheck(t, resolvedPath, "READY")

	// The overwrite is bound to status=external. A second drift invalidates it.
	if _, err := execConn.Exec("UPDATE unredo_shop.orders SET status = 'drifted' WHERE id = ?", rowID); err != nil {
		t.Fatal(err)
	}
	planCheck(t, resolvedPath, "CONFLICT")
	if _, err := execConn.Exec("UPDATE unredo_shop.orders SET status = 'external' WHERE id = ?", rowID); err != nil {
		t.Fatal(err)
	}

	short := planner.ShortDigest(resolved.Digest)
	runCLI(t, false,
		"plan", "apply", resolvedPath,
		"--non-interactive", "--confirm-sha", short,
		"--operator", "incident-dba",
	)
	applyOutput := runCLI(t, true,
		"plan", "apply", resolvedPath,
		"--non-interactive", "--confirm-sha", short,
		"--accept-risk", short,
		"--operator", "incident-dba", "--reason", "INC-RESOLVE-1",
	)
	action := actionShow(t, outputField(t, applyOutput, "action_id"))
	if action.ExecutionClass != "UNSAFE_RESOLVED" || action.RootPlanDigest != parent.Digest || action.PlanDigest != resolved.Digest {
		t.Fatalf("unexpected resolved action marker: %+v", action)
	}
	var status, amount string
	if err := execConn.QueryRow("SELECT status, amount FROM unredo_shop.orders WHERE id = ?", rowID).Scan(&status, &amount); err != nil {
		t.Fatal(err)
	}
	if status != marker || amount != "10.00" {
		t.Fatalf("resolved overwrite restored status=%q amount=%q; want %q, 10.00", status, amount, marker)
	}
}

func TestInitGeneratesProfileAppliesMetaAndRunsDoctor(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	var mysqlServerID uint32
	if err := rootConn.QueryRow("SELECT @@global.server_id").Scan(&mysqlServerID); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "unredo.yaml")
	grantsPath := filepath.Join(dir, "grants.sql")
	repoRoot := repoRoot(t)
	cmd := exec.Command(filepath.Join(repoRoot, "bin", "unredo.exe"),
		"--config", configPath,
		"--profile", "init-integration",
		"init",
		"--non-interactive",
		"--address", "127.0.0.1:3306",
		"--reader-user", readerUser,
		"--reader-password-env", "UNREDO_READER_PASSWORD",
		"--executor-user", executorUser,
		"--executor-password-env", "UNREDO_EXECUTOR_PASSWORD",
		"--database", "unredo_shop",
		"--grants-output", grantsPath,
		"--apply-meta",
		"--admin-user", "root",
		"--admin-password-env", "UNREDO_TEST_ADMIN_PASSWORD",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"UNREDO_READER_PASSWORD="+readerPass,
		"UNREDO_EXECUTOR_PASSWORD="+executorPass,
		"UNREDO_TEST_ADMIN_PASSWORD="+rootPass,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "target.meta_schema") || !strings.Contains(string(out), "meta_schema:     applied") {
		t.Fatalf("init did not apply metadata and run doctor:\n%s", out)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := cfg.Profile("init-integration")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Source.ServerID == 0 || profile.Source.ServerID == mysqlServerID {
		t.Fatalf("generated server_id %d conflicts with MySQL server_id %d", profile.Source.ServerID, mysqlServerID)
	}
	grantSQL, err := os.ReadFile(grantsPath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(grantSQL)), "IDENTIFIED BY") {
		t.Fatal("generated grant SQL contains an account password clause")
	}
}

func createPlanForGTID(t *testing.T, rootConn *sql.DB, gtid string) string {
	t.Helper()
	binlogFile, err := readCurrentBinlogFile(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	repoRoot := repoRoot(t)
	planPath := filepath.Join(t.TempDir(), "plan.json")
	cmd := exec.Command(filepath.Join(repoRoot, "bin", "unredo.exe"),
		"--config", "unredo.yaml", "--profile", "local",
		"plan", "create", "--binlog", binlogFile, "--from-pos", "4",
		"--txn", gtid, "--output", planPath)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"UNREDO_READER_PASSWORD="+readerPass,
		"UNREDO_EXECUTOR_PASSWORD="+executorPass)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("plan create for %s: %v\n%s", gtid, err, out)
	}
	return planPath
}

func latestGTID(db *sql.DB) (string, error) {
	var s string
	if err := db.QueryRow("SELECT @@global.gtid_executed").Scan(&s); err != nil {
		return "", err
	}
	parts := strings.Split(s, ",")
	last := strings.TrimSpace(parts[len(parts)-1])
	colon := strings.LastIndex(last, ":")
	if colon < 0 {
		return "", fmt.Errorf("malformed GTID set %q", s)
	}
	uuid, seq := last[:colon], last[colon+1:]
	if dash := strings.Index(seq, "-"); dash >= 0 {
		seq = seq[dash+1:]
	}
	return uuid + ":" + seq, nil
}

func ensureFullRowMetadata(t *testing.T, db *sql.DB) {
	t.Helper()
	if _, err := db.Exec("SET GLOBAL binlog_row_metadata = 'FULL'"); err != nil {
		t.Fatalf("enable binlog_row_metadata=FULL for integration test: %v", err)
	}
}

// planApply runs the unredo CLI's apply subcommand and checks the
// exit code. The expectedExitZero bool controls which exit code is
// required. The full unredo binary is invoked so we exercise the
// end-to-end path including marker write, transaction commit, and
// (on the second call) replay protection.
func planApply(t *testing.T, planPath, confirm string, expectedExit int, expectedExitZero bool) {
	t.Helper()
	_ = planApplyCapture(t, planPath, confirm, expectedExitZero)
	_ = expectedExit
}

func planApplyCapture(t *testing.T, planPath, confirm string, expectedExitZero bool) string {
	t.Helper()
	repoRoot := repoRoot(t)
	binary := filepath.Join(repoRoot, "bin", "unredo.exe")
	cmd := exec.Command(binary,
		"--config", "unredo.yaml",
		"--profile", "local",
		"plan", "apply", planPath,
		"--non-interactive",
		"--confirm-sha", confirm,
		"--operator", "test",
		"--log-level", "error",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"UNREDO_READER_PASSWORD="+readerPass,
		"UNREDO_EXECUTOR_PASSWORD="+executorPass,
	)
	out, err := cmd.CombinedOutput()
	exitOK := err == nil
	if expectedExitZero && !exitOK {
		t.Fatalf("expected apply to succeed; got err=%v\n%s", err, out)
	}
	if !expectedExitZero && exitOK {
		t.Fatalf("expected apply to fail; got success\n%s", out)
	}
	return string(out)
}

func runCLI(t *testing.T, expectSuccess bool, args ...string) string {
	t.Helper()
	repoRoot := repoRoot(t)
	base := []string{"--config", "unredo.yaml", "--profile", "local"}
	cmd := exec.Command(filepath.Join(repoRoot, "bin", "unredo.exe"), append(base, args...)...)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"UNREDO_READER_PASSWORD="+readerPass,
		"UNREDO_EXECUTOR_PASSWORD="+executorPass,
	)
	out, err := cmd.CombinedOutput()
	if expectSuccess && err != nil {
		t.Fatalf("unredo %s failed: %v\n%s", strings.Join(args, " "), err, out)
	}
	if !expectSuccess && err == nil {
		t.Fatalf("unredo %s unexpectedly succeeded\n%s", strings.Join(args, " "), out)
	}
	return string(out)
}

func actionShow(t *testing.T, actionID string) struct {
	ActionID       string    `json:"action_id"`
	ParentActionID string    `json:"parent_action_id"`
	RootPlanDigest string    `json:"root_plan_digest"`
	PlanDigest     string    `json:"plan_digest"`
	ActionType     string    `json:"action_type"`
	TargetState    string    `json:"target_state"`
	ChainDepth     uint32    `json:"chain_depth"`
	CreatedAt      time.Time `json:"created_at"`
	ExecutionClass string    `json:"execution_class"`
} {
	t.Helper()
	out := runCLI(t, true, "--format", "json", "action", "show", "--action-id", actionID)
	var action struct {
		ActionID       string    `json:"action_id"`
		ParentActionID string    `json:"parent_action_id"`
		RootPlanDigest string    `json:"root_plan_digest"`
		PlanDigest     string    `json:"plan_digest"`
		ActionType     string    `json:"action_type"`
		TargetState    string    `json:"target_state"`
		ChainDepth     uint32    `json:"chain_depth"`
		CreatedAt      time.Time `json:"created_at"`
		ExecutionClass string    `json:"execution_class"`
	}
	if err := json.Unmarshal([]byte(out), &action); err != nil {
		t.Fatalf("decode action show: %v\n%s", err, out)
	}
	if action.ActionID != actionID {
		t.Fatalf("action show returned %q, want %q", action.ActionID, actionID)
	}
	return action
}

func outputField(t *testing.T, output, name string) string {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 && strings.TrimSpace(parts[0]) == name {
			return strings.TrimSpace(parts[1])
		}
	}
	t.Fatalf("output field %q missing from:\n%s", name, output)
	return ""
}

// planCheck runs the CLI's plan check command and asserts the status
// appears in the output. The unredo binary is invoked as a subprocess
// so we exercise the same code path the user will.
func planCheck(t *testing.T, planPath, wantStatus string) {
	t.Helper()
	repoRoot := repoRoot(t)
	binary := filepath.Join(repoRoot, "bin", "unredo.exe")
	cmd := exec.Command(binary,
		"--config", "unredo.yaml",
		"--profile", "local",
		"plan", "check", planPath,
		"--log-level", "error",
	)
	cmd.Dir = repoRoot
	cmd.Env = append(os.Environ(),
		"UNREDO_READER_PASSWORD="+readerPass,
		"UNREDO_EXECUTOR_PASSWORD="+executorPass,
	)
	out, err := cmd.CombinedOutput()
	// We only care about the textual status; CONFLICT exits non-zero
	// by design. Use a substring check.
	got := string(out)
	if !strings.Contains(got, "status:        "+wantStatus) {
		t.Fatalf("plan check did not report %s; got:\n%s", wantStatus, got)
	}
	if wantStatus == "CONFLICT" && err == nil {
		t.Fatalf("plan check with CONFLICT must exit non-zero; err=nil output=%s", got)
	}
	if wantStatus == "READY" && err != nil {
		t.Fatalf("plan check with READY must exit zero; err=%v output=%s", err, got)
	}
	_ = err
}

func readExecutedGTIDFor(db *sql.DB, marker string, userID int) (string, error) {
	err := db.QueryRow("SELECT user_id FROM unredo_shop.orders WHERE status = ? AND user_id = ?", marker, userID).Scan(new(interface{}))
	if err != nil {
		return "", err
	}
	// After the INSERT is durable on the connection that issued it, its
	// GTID is appended to @@global.gtid_executed. The set is a comma-
	// separated list of "uuid:start-end" ranges; we want the highest
	// single GTID, which is the end of the last range.
	var s string
	if err := db.QueryRow("SELECT @@global.gtid_executed").Scan(&s); err != nil {
		return "", err
	}
	parts := strings.Split(s, ",")
	if len(parts) == 0 {
		return "", fmt.Errorf("empty gtid_executed")
	}
	last := strings.TrimSpace(parts[len(parts)-1])
	if last == "" {
		return "", fmt.Errorf("empty trailing range in gtid_executed=%q", s)
	}
	// last is "uuid:start-end" or "uuid:gnum".
	colon := strings.LastIndex(last, ":")
	if colon < 0 {
		return "", fmt.Errorf("malformed GTID %q", last)
	}
	uuid := last[:colon]
	seq := last[colon+1:]
	// Strip "-end" suffix if present.
	if dash := strings.Index(seq, "-"); dash >= 0 {
		seq = seq[dash+1:]
	}
	return uuid + ":" + seq, nil
}

func readCurrentBinlogFile(db *sql.DB) (string, error) {
	var f string
	if err := db.QueryRow("SHOW BINARY LOG STATUS").Scan(&f, new(interface{}), new(interface{}), new(interface{}), new(interface{})); err != nil {
		return "", err
	}
	return f, nil
}

func openRoot(t *testing.T, _ string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("root:%s@tcp(127.0.0.1:3306)/?parseTime=false&loc=UTC", rootPass)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open root: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Skipf("MySQL not reachable, skipping integration test: %v", err)
	}
	return db
}

func openExecutor(t *testing.T, _ string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("%s:%s@tcp(127.0.0.1:3306)/?parseTime=false&loc=UTC", executorUser, executorPass)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatalf("open executor: %v", err)
	}
	if err := db.Ping(); err != nil {
		t.Skipf("executor not reachable: %v", err)
	}
	return db
}

// findMySQLBin returns the path to the bundled mysql client. The test
// uses the client to format queries the same way the manual workflow
// does, avoiding subtle DSN differences.
func findMySQLBin(t *testing.T) string {
	t.Helper()
	for _, p := range []string{
		`D:\tool\mysql\bin\mysql.exe`,
		`C:\Program Files\MySQL\MySQL Server 8.0\bin\mysql.exe`,
		`/usr/bin/mysql`,
		`/usr/local/mysql/bin/mysql`,
	} {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	t.Skip("no mysql client found; install MySQL 8 client or set UNREDO_MYSQL_BIN")
	return ""
}

func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	dir := filepath.Dir(here)
	// tests/integration -> repo root
	return filepath.Clean(filepath.Join(dir, "..", ".."))
}

// silence unused imports in some toolchains
var _ = json.Marshal
