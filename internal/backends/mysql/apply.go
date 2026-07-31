// Package mysql — apply.go implements executor.Applier for MySQL.
//
// The apply path runs in a single InnoDB transaction so the action
// marker and the data writes commit or rollback together. Each row
// operation is locked with SELECT ... FOR UPDATE before the write,
// and the write is conditional on the expect image when one exists.
package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/backends/mysql/schema"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/executor"
	"github.com/girimi/unredo/internal/ports"
)

// ApplyWriter executes plans against a target MySQL instance. It owns
// the InnoDB transaction and the marker write.
type ApplyWriter struct {
	targetDSN       string
	instanceID      string
	inspector       *schema.Inspector
	lockWaitTimeout time.Duration
}

// NewApplyWriter wires a writer to the target DSN.
func NewApplyWriter(targetDSN, instanceID string, lockWaitTimeout time.Duration) *ApplyWriter {
	if lockWaitTimeout <= 0 {
		lockWaitTimeout = 5 * time.Second
	}
	return &ApplyWriter{
		targetDSN:       targetDSN,
		instanceID:      instanceID,
		inspector:       schema.NewInspector(targetDSN),
		lockWaitTimeout: lockWaitTimeout,
	}
}

// NewApplyWriterFromBackend pulls the target DSN and instance id out
// of a Backend.
func NewApplyWriterFromBackend(b *Backend) *ApplyWriter {
	return NewApplyWriter(b.targetDSN, b.targetInstanceID, b.policy.LockWaitTimeout)
}

// Apply runs the plan in a single transaction. The execution_class
// and the per-op conditional check together guarantee:
//   - the marker and data writes commit atomically
//   - a re-apply of the same plan is blocked by uq_plan_id
//   - concurrent changes are detected via the expect image in WHERE
func (w *ApplyWriter) Apply(ctx context.Context, plan *ports.Plan, opts executor.ApplyOptions) (ports.ExecutionResult, error) {
	if plan == nil {
		return ports.ExecutionResult{}, fmt.Errorf("mysql apply: nil plan")
	}
	if err := opts.Validate(); err != nil {
		return ports.ExecutionResult{}, err
	}
	if err := w.confirm(opts.Confirm, plan.Digest); err != nil {
		return ports.ExecutionResult{}, err
	}

	db, err := sql.Open("mysql", w.targetDSN)
	if err != nil {
		return ports.ExecutionResult{}, fmt.Errorf("mysql apply: open: %w", err)
	}
	defer db.Close()

	conn, err := db.Conn(ctx)
	if err != nil {
		return ports.ExecutionResult{}, fmt.Errorf("mysql apply: conn: %w", err)
	}
	defer conn.Close()

	if err := w.beginTx(ctx, conn); err != nil {
		return ports.ExecutionResult{}, err
	}
	// From here on, any error must roll back. We track that with the
	// committed flag.
	committed := false
	rollback := func() {
		if !committed {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
		}
	}
	defer rollback()

	// Acquire metadata locks for every target table, then re-check the
	// fingerprints while those locks are held. This closes the schema race
	// between the pre-apply check and the first row write.
	if err := w.lockAndCheckSchemas(ctx, conn, plan); err != nil {
		return ports.ExecutionResult{}, err
	}

	// 1. Insert action marker. UNIQUE(plan_id) is what stops a re-apply.
	if err := w.insertMarker(ctx, conn, plan, opts); err != nil {
		return ports.ExecutionResult{}, err
	}

	// 2. Apply each operation. Failures here are conditional
	// preconditions; we map them to ErrApplyConflict.
	affected := 0
	for _, op := range plan.Operations {
		n, err := w.applyOp(ctx, conn, op)
		if err != nil {
			return ports.ExecutionResult{}, err
		}
		affected += n
	}

	if _, err := conn.ExecContext(ctx, "COMMIT"); err != nil {
		// A transport failure around COMMIT does not prove rollback. The
		// action marker is in the same transaction, so callers can use it
		// to resolve the outcome before any retry.
		return ports.ExecutionResult{}, fmt.Errorf("%w: action_id=%s: %v", ports.ErrCommitUnknown, encodeHex(opts.ActionID), err)
	}
	committed = true

	return ports.ExecutionResult{
		// Do not guess this from @@global.gtid_executed: another concurrent
		// transaction may own the newest GTID. The marker/binlog correlation
		// path will populate it when action inspection is implemented.
		CompensatingGTID: "",
		AffectedRows:     affected,
		ActionID:         encodeHex(opts.ActionID),
	}, nil
}

func (w *ApplyWriter) lockAndCheckSchemas(ctx context.Context, conn *sql.Conn, plan *ports.Plan) error {
	seen := make(map[core.TableRef]struct{})
	for _, op := range plan.Operations {
		if _, ok := seen[op.Table]; ok {
			continue
		}
		seen[op.Table] = struct{}{}
		q := "SELECT 1 FROM " + quoteIdent(op.Table.Schema) + "." + quoteIdent(op.Table.Name) + " LIMIT 0 FOR UPDATE"
		rows, err := conn.QueryContext(ctx, q)
		if err != nil {
			return fmt.Errorf("mysql apply: metadata lock %s: %w", op.Table, err)
		}
		_ = rows.Close()
	}
	for table := range seen {
		want, ok := plan.SchemaFingerprints[table.Schema+"."+table.Name]
		if !ok || want == "" {
			return fmt.Errorf("mysql apply: missing schema fingerprint for %s", table)
		}
		got, err := w.inspector.Fingerprint(ctx, table)
		if err != nil {
			return fmt.Errorf("mysql apply: fingerprint %s: %w", table, err)
		}
		if string(got) != want {
			return fmt.Errorf("%w: %s changed after pre-apply check", ports.ErrSchemaMismatch, table)
		}
	}
	return nil
}

func (w *ApplyWriter) beginTx(ctx context.Context, conn *sql.Conn) error {
	// Keep lock waits short so a stuck row doesn't hang the CLI.
	seconds := int64(w.lockWaitTimeout / time.Second)
	if seconds < 1 {
		seconds = 1
	}
	if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET innodb_lock_wait_timeout = %d", seconds)); err != nil {
		return fmt.Errorf("mysql apply: set lock wait timeout: %w", err)
	}
	if _, err := conn.ExecContext(ctx, "START TRANSACTION"); err != nil {
		return fmt.Errorf("mysql apply: begin: %w", err)
	}
	return nil
}

func (w *ApplyWriter) insertMarker(ctx context.Context, conn *sql.Conn, plan *ports.Plan, opts executor.ApplyOptions) error {
	digestBin, err := hexDecode(plan.Digest[len("sha256:"):])
	if err != nil {
		return fmt.Errorf("mysql apply: plan digest: %w", err)
	}
	rootBin := digestBin // for M2 the root digest equals the plan digest
	q := `INSERT INTO unredo_meta.action_markers
		(action_id, plan_id, parent_action_id, root_plan_digest,
		 action_type, target_state, chain_depth,
		 source_native_transaction_id, plan_digest, execution_class,
		 reason, tool_version, operator_name)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	_, err = conn.ExecContext(ctx, q,
		opts.ActionID,
		opts.PlanID,
		nullIfEmpty(opts.ParentActionID),
		rootBin,
		opts.ActionType,
		opts.TargetState,
		opts.ChainDepth,
		opts.SourceNativeTransactionID,
		digestBin,
		opts.ExecutionClass,
		nullString(opts.Reason),
		"0.1.0-m2",
		opts.OperatorName,
	)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return executor.ErrApplyReplayed
		}
		return fmt.Errorf("mysql apply: insert marker: %w", err)
	}
	return nil
}

// applyOp locks the target row, runs the conditional write, and
// checks the affected-row count.
func (w *ApplyWriter) applyOp(ctx context.Context, conn *sql.Conn, op ports.PlanOperation) (int, error) {
	// Lock-and-check. We do the lock first so the subsequent write
	// is held under the same row lock; the conditional WHERE adds
	// belt-and-braces against the gap between check and apply.
	if err := w.lockForOp(ctx, conn, op); err != nil {
		return 0, err
	}
	switch op.Kind {
	case core.OpInsert:
		return w.writeInsert(ctx, conn, op)
	case core.OpDelete:
		return w.writeDelete(ctx, conn, op)
	case core.OpUpdate:
		return w.writeUpdate(ctx, conn, op)
	default:
		return 0, fmt.Errorf("mysql apply: unsupported op kind %q", op.Kind)
	}
}

func (w *ApplyWriter) lockForOp(ctx context.Context, conn *sql.Conn, op ports.PlanOperation) error {
	where, args, err := buildPredicate(op.Key)
	if err != nil {
		return fmt.Errorf("mysql apply: key predicate: %w", err)
	}
	// For INSERT, locking the unique-key gap prevents another transaction
	// from inserting the same key before our INSERT. For existing rows, the
	// full expect image is checked by the write itself while this key lock is
	// held.
	query := "SELECT 1 FROM " + quoteIdent(op.Table.Schema) + "." + quoteIdent(op.Table.Name) + " WHERE " + where + " FOR UPDATE"
	var x int
	err = conn.QueryRowContext(ctx, query, args...).Scan(&x)
	switch {
	case err == nil:
		if op.Kind == core.OpInsert {
			return fmt.Errorf("%w: insert op, but a row with the same key already exists", executor.ErrApplyConflict)
		}
		return nil
	case errors.Is(err, sql.ErrNoRows):
		if op.Kind == core.OpInsert {
			return nil // expected: no row, ready to insert
		}
		return fmt.Errorf("%w: %s op, but the row is missing", executor.ErrApplyConflict, op.Kind)
	default:
		return fmt.Errorf("mysql apply: lock: %w", err)
	}
}

func (w *ApplyWriter) writeInsert(ctx context.Context, conn *sql.Conn, op ports.PlanOperation) (int, error) {
	if len(op.Write.Columns) == 0 {
		return 0, fmt.Errorf("mysql apply: insert op has no write columns")
	}
	cols := quoteNames(op.Write.Columns)
	placeholders := strings.Repeat("?,", len(op.Write.Columns))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]interface{}, len(op.Write.Values))
	for i, v := range op.Write.Values {
		arg, err := driverValue(v)
		if err != nil {
			return 0, fmt.Errorf("mysql apply: insert value: %w", err)
		}
		args[i] = arg
	}
	q := "INSERT INTO " + quoteIdent(op.Table.Schema) + "." + quoteIdent(op.Table.Name) + " (" + cols + ") VALUES (" + placeholders + ")"
	res, err := conn.ExecContext(ctx, q, args...)
	if err != nil {
		var me *mysql.MySQLError
		if errors.As(err, &me) && me.Number == 1062 {
			return 0, fmt.Errorf("%w: insert op duplicate key", executor.ErrApplyConflict)
		}
		return 0, fmt.Errorf("mysql apply: insert: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return 0, fmt.Errorf("mysql apply: insert affected %d rows, want 1", n)
	}
	return int(n), nil
}

func (w *ApplyWriter) writeDelete(ctx context.Context, conn *sql.Conn, op ports.PlanOperation) (int, error) {
	where, args, err := buildPredicate(op.Key, op.Expect)
	if err != nil {
		return 0, fmt.Errorf("mysql apply: delete predicate: %w", err)
	}
	q := "DELETE FROM " + quoteIdent(op.Table.Schema) + "." + quoteIdent(op.Table.Name) + " WHERE " + where
	res, err := conn.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("mysql apply: delete: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return 0, fmt.Errorf("%w: delete op affected %d rows, want 1 (row drifted since check)", executor.ErrApplyConflict, n)
	}
	return int(n), nil
}

func (w *ApplyWriter) writeUpdate(ctx context.Context, conn *sql.Conn, op ports.PlanOperation) (int, error) {
	if len(op.Write.Columns) == 0 {
		return 0, fmt.Errorf("mysql apply: update op has no write columns")
	}
	// SET clause: write cols only.
	sets := make([]string, 0, len(op.Write.Columns))
	args := make([]interface{}, 0, len(op.Write.Values))
	for i, c := range op.Write.Columns {
		sets = append(sets, quoteIdent(c)+" = ?")
		arg, err := driverValue(op.Write.Values[i])
		if err != nil {
			return 0, fmt.Errorf("mysql apply: update value: %w", err)
		}
		args = append(args, arg)
	}
	// WHERE: key columns + every expect column. The expect image is
	// what the plan saw at create time; if any has drifted, the
	// UPDATE matches no row and we surface a conflict.
	where, whereArgs, err := buildPredicate(op.Key, op.Expect)
	if err != nil {
		return 0, fmt.Errorf("mysql apply: update predicate: %w", err)
	}
	args = append(args, whereArgs...)
	q := "UPDATE " + quoteIdent(op.Table.Schema) + "." + quoteIdent(op.Table.Name) + " SET " + strings.Join(sets, ", ") +
		" WHERE " + where
	res, err := conn.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, fmt.Errorf("mysql apply: update: %w", err)
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return 0, fmt.Errorf("%w: update op affected %d rows, want 1 (row drifted since check)", executor.ErrApplyConflict, n)
	}
	return int(n), nil
}

func quoteNames(cols []string) string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = quoteIdent(c)
	}
	return strings.Join(out, ", ")
}

func nullIfEmpty(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

func (w *ApplyWriter) confirm(want, planDigest string) error {
	if want == "" {
		// Empty confirm is allowed for non-interactive CI flows that
		// have already validated. The CLI uses --non-interactive +
		// --confirm-sha; if --confirm-sha is missing, we fail in the
		// CLI before getting here.
		return nil
	}
	got := stripDigestPrefix(planDigest)
	if len(got) < len(want) || got[:len(want)] != want {
		return fmt.Errorf("mysql apply: confirm-sha %q does not match plan digest %s", want, got)
	}
	return nil
}

func stripDigestPrefix(d string) string {
	const p = "sha256:"
	if len(d) > len(p) && d[:len(p)] == p {
		return d[len(p):]
	}
	return d
}

func encodeHex(b []byte) string {
	const hex = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = hex[c>>4]
		out[i*2+1] = hex[c&0x0f]
	}
	return string(out)
}

func hexDecode(s string) ([]byte, error) {
	if len(s)%2 != 0 {
		return nil, fmt.Errorf("odd hex length %d", len(s))
	}
	out := make([]byte, len(s)/2)
	for i := 0; i < len(out); i++ {
		hi, ok1 := unhex(s[i*2])
		lo, ok2 := unhex(s[i*2+1])
		if !ok1 || !ok2 {
			return nil, fmt.Errorf("invalid hex at offset %d", i*2)
		}
		out[i] = (hi << 4) | lo
	}
	return out, nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
