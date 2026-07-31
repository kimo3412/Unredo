// Package executor checks and applies plans. It is the read/write
// counterpoint to the planner: the planner produces a Plan, this
// package consumes one and validates or executes it.
//
// The executor is database-agnostic; concrete value reading lives in
// the backend adapter. This file only owns the comparison rules and
// the conflict shape.
package executor

import (
	"context"
	"fmt"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

// Status is the high-level outcome of a check.
type Status string

const (
	StatusReady          Status = "READY"
	StatusConflict       Status = "CONFLICT"
	StatusStaleSchema    Status = "STALE_SCHEMA"
	StatusUnsupported    Status = "UNSUPPORTED"
	StatusSourceMismatch Status = "SOURCE_MISMATCH"
)

// SchemaCheck is the per-table fingerprint comparison result.
type SchemaCheck struct {
	Table        core.TableRef `json:"table"`
	PlanDigest   string        `json:"plan_digest"`
	ActualDigest string        `json:"actual_digest"`
	Match        bool          `json:"match"`
}

// ConflictKind classifies what went wrong.
type ConflictKind string

const (
	ConflictRowMissing      ConflictKind = "row_missing"    // expect a row, none found
	ConflictRowExists       ConflictKind = "row_exists"     // expect no row (insert), found one
	ConflictRowMismatch     ConflictKind = "row_mismatch"   // value differs
	ConflictKeyMissing      ConflictKind = "key_missing"    // key column not in row image
	ConflictUnsupportedOp   ConflictKind = "unsupported_op" // op kind we can't check
	ConflictInstanceDiffers ConflictKind = "instance_differs"
)

// Row is what the backend read at check time. It is a frontend to
// core.Value to keep this package from importing any backend code.
type Row struct {
	Columns []string
	Values  []core.Value
}

// Get returns the value of a column by name, or false.
func (r Row) Get(column string) (core.Value, bool) {
	for i, c := range r.Columns {
		if c == column && i < len(r.Values) {
			return r.Values[i], true
		}
	}
	return core.Value{}, false
}

// Conflict is one precondition failure that blocks apply.
type Conflict struct {
	OperationSequence int           `json:"operation_sequence"`
	Table             core.TableRef `json:"table"`
	Kind              ConflictKind  `json:"kind"`
	Column            string        `json:"column,omitempty"`
	Expected          core.Value    `json:"expected,omitempty"`
	Actual            core.Value    `json:"actual,omitempty"`
	Current           core.Row      `json:"current,omitempty"`
	Message           string        `json:"message"`
}

// CheckResult is the full check outcome for one plan.
type CheckResult struct {
	Status          Status        `json:"status"`
	PlanDigest      string        `json:"plan_digest"`
	TargetInstance  string        `json:"target_instance"`
	SchemaChecks    []SchemaCheck `json:"schema_checks"`
	Conflicts       []Conflict    `json:"conflicts"`
	OperationsTotal int           `json:"operations_total"`
}

// Reader is what the executor needs from a backend to check a plan.
// It hides the MySQL driver behind a Row-shaped value.
type Reader interface {
	// ReadByKey returns the current row identified by the key columns,
	// or false if no row matches.
	ReadByKey(ctx context.Context, table core.TableRef, keyColumns []string, key core.Row) (Row, bool, error)
	// Fingerprint recomputes the schema fingerprint.
	Fingerprint(ctx context.Context, table core.TableRef) (core.SchemaFingerprint, error)
	// TargetInstanceID is the instance UUID the reader is bound to.
	TargetInstanceID() string
}

// Check runs the read-only verification described in DESIGN.md §9.2.
// It is total: it always returns a result. Conflicts are aggregated;
// the caller decides what to do.
func Check(ctx context.Context, plan *ports.Plan, reader Reader) (*CheckResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("executor: nil plan")
	}
	result := &CheckResult{
		PlanDigest:      plan.Digest,
		TargetInstance:  reader.TargetInstanceID(),
		OperationsTotal: len(plan.Operations),
	}
	if plan.Ref.Backend != "" && result.TargetInstance != "" &&
		plan.Ref.InstanceID != "" && plan.Ref.InstanceID != result.TargetInstance {
		result.Status = StatusSourceMismatch
		result.Conflicts = append(result.Conflicts, Conflict{
			Kind: ConflictInstanceDiffers,
			Message: fmt.Sprintf("plan was built for instance %s, current target is %s",
				plan.Ref.InstanceID, result.TargetInstance),
		})
		return result, nil
	}

	// 1. Schema fingerprints: collect unique tables from operations.
	seen := map[core.TableRef]bool{}
	for _, op := range plan.Operations {
		if seen[op.Table] {
			continue
		}
		seen[op.Table] = true
		fp, err := reader.Fingerprint(ctx, op.Table)
		if err != nil {
			result.Conflicts = append(result.Conflicts, Conflict{
				Table:   op.Table,
				Kind:    ConflictUnsupportedOp,
				Message: "fingerprint failed: " + err.Error(),
			})
			continue
		}
		planFp := plan.SchemaFingerprints[op.Table.String()]
		c := SchemaCheck{
			Table:        op.Table,
			PlanDigest:   planFp,
			ActualDigest: string(fp),
			Match:        planFp != "" && planFp == string(fp),
		}
		result.SchemaChecks = append(result.SchemaChecks, c)
		if !c.Match {
			result.Status = StatusStaleSchema
		}
	}

	// 2. Row checks per operation.
	for _, op := range plan.Operations {
		if conflicts := checkOneOperation(ctx, op, reader); len(conflicts) > 0 {
			result.Conflicts = append(result.Conflicts, conflicts...)
		}
	}

	if result.Status == "" {
		if len(result.Conflicts) > 0 {
			result.Status = StatusConflict
		} else {
			result.Status = StatusReady
		}
	}
	return result, nil
}

func checkOneOperation(ctx context.Context, op ports.PlanOperation, reader Reader) []Conflict {
	// Reject unknown op kinds up front.
	switch op.Kind {
	case core.OpInsert, core.OpUpdate, core.OpDelete:
	default:
		return []Conflict{{
			OperationSequence: op.Sequence,
			Table:             op.Table,
			Kind:              ConflictUnsupportedOp,
			Message:           fmt.Sprintf("unsupported op kind %q", op.Kind),
		}}
	}

	// Key columns must be present in the key image.
	for _, kc := range op.Key.Columns {
		if _, ok := op.Key.Get(kc); !ok {
			return []Conflict{{
				OperationSequence: op.Sequence,
				Table:             op.Table,
				Kind:              ConflictKeyMissing,
				Column:            kc,
				Message:           fmt.Sprintf("key column %q not present in plan key image", kc),
			}}
		}
	}

	row, found, err := reader.ReadByKey(ctx, op.Table, op.Key.Columns, op.Key)
	if err != nil {
		return []Conflict{{
			OperationSequence: op.Sequence,
			Table:             op.Table,
			Kind:              ConflictUnsupportedOp,
			Message:           "read current row: " + err.Error(),
		}}
	}

	switch op.Kind {
	case core.OpInsert:
		// expect: row should NOT exist
		if found {
			return []Conflict{{
				OperationSequence: op.Sequence,
				Table:             op.Table,
				Kind:              ConflictRowExists,
				Current:           executorRowToCore(row),
				Message:           "insert plan, but a row with the same key already exists",
			}}
		}
		return nil
	case core.OpDelete:
		// expect: row should exist and match the expect image
		if !found {
			return []Conflict{{
				OperationSequence: op.Sequence,
				Table:             op.Table,
				Kind:              ConflictRowMissing,
				Message:           "delete plan, but the row is already missing",
			}}
		}
		return compareRow(op.Sequence, op.Table, op.Expect, row)
	case core.OpUpdate:
		if !found {
			return []Conflict{{
				OperationSequence: op.Sequence,
				Table:             op.Table,
				Kind:              ConflictRowMissing,
				Message:           "update plan, but the row is missing",
			}}
		}
		return compareRow(op.Sequence, op.Table, op.Expect, row)
	}
	return nil
}

// compareRow returns a conflict for each column where current != expect.
func compareRow(seq int, table core.TableRef, expect core.Row, current Row) []Conflict {
	var out []Conflict
	currentRow := executorRowToCore(current)
	for i, col := range expect.Columns {
		want := expect.Values[i]
		got, ok := current.Get(col)
		if !ok {
			out = append(out, Conflict{
				OperationSequence: seq,
				Table:             table,
				Kind:              ConflictRowMismatch,
				Column:            col,
				Expected:          want,
				Actual:            core.Value{Kind: want.Kind, Null: true},
				Current:           currentRow,
				Message:           "column not present in current row",
			})
			continue
		}
		if !valuesEqual(want, got) {
			out = append(out, Conflict{
				OperationSequence: seq,
				Table:             table,
				Kind:              ConflictRowMismatch,
				Column:            col,
				Expected:          want,
				Actual:            got,
				Current:           currentRow,
				Message:           fmt.Sprintf("current value does not match plan expect image (want=%s got=%s)", string(want.Data), string(got.Data)),
			})
		}
	}
	return out
}

func executorRowToCore(row Row) core.Row {
	return core.Row{
		Columns: append([]string(nil), row.Columns...),
		Values:  append([]core.Value(nil), row.Values...),
	}
}

// valuesEqual compares the canonical typed form without lossy numeric
// coercion. Both the binlog and SELECT paths use the same decoder.
func valuesEqual(a, b core.Value) bool {
	// Compare the canonical typed representation exactly. Converting JSON
	// numbers through float64 can make adjacent BIGINT values appear equal.
	return a.Equal(b)
}
