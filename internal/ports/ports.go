// Package ports declares the backend interfaces the core depends on.
// Implementations live under internal/backends/* and are wired by the
// registry at startup. Core must not import any concrete backend here.
package ports

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/girimi/unredo/internal/core"
)

// Stable domain errors. Backends translate native errors into one of these
// so the CLI can map them to the documented exit codes.
var (
	ErrTransactionNotFound    = errors.New("transaction not found")
	ErrUnsupportedCapability  = errors.New("unsupported capability")
	ErrCommitUnknown          = errors.New("commit result unknown")
	ErrSchemaMismatch         = errors.New("schema mismatch")
	ErrPlanDigestMismatch     = errors.New("plan digest mismatch")
	ErrInstanceMismatch       = errors.New("instance id mismatch")
)

// ScanScope narrows what a Scan call returns. Backend-specific fields
// travel inside Cursor, which is opaque to the core.
type ScanScope struct {
	FromCursor json.RawMessage
	ToCursor   json.RawMessage
	Database   string
	Table      string
	Limit      int
}

// TransactionIterator yields transactions in commit order. A nil error
// with a nil transaction signals end-of-stream.
type TransactionIterator interface {
	Next(ctx context.Context) (*core.Transaction, error)
	Close() error
}

// ChangeSource reads transaction events from a backend's log.
// Scan streams from FromCursor; Find locates a single transaction by ref.
type ChangeSource interface {
	Capabilities(ctx context.Context) (core.BackendCapabilities, error)
	Scan(ctx context.Context, scope ScanScope) (TransactionIterator, error)
	Find(ctx context.Context, ref core.TransactionRef) (*core.Transaction, error)
}

// ColumnDef describes one column as reported by the backend.
type ColumnDef = core.ColumnDef

// UniqueKey is an ordered set of columns that uniquely identify a row.
type UniqueKey = core.UniqueKey

// TableSchema is what a planner needs to construct safe operations.
type TableSchema = core.TableSchema

// SchemaFingerprint is a stable hash over the table definition. Any
// drift invalidates previously generated plans.
type SchemaFingerprint = core.SchemaFingerprint

// SchemaInspector reports table shape and stable fingerprints.
type SchemaInspector interface {
	InspectTable(ctx context.Context, table core.TableRef) (core.TableSchema, error)
	Fingerprint(ctx context.Context, table core.TableRef) (core.SchemaFingerprint, error)
}

// Conflict describes one precondition failure during plan check.
type Conflict struct {
	OperationSequence int      `json:"operation_sequence"`
	Table             core.TableRef `json:"table"`
	Kind              string   `json:"kind"`
	ExpectedDigest    string   `json:"expected_digest,omitempty"`
	ActualDigest      string   `json:"actual_digest,omitempty"`
	Message           string   `json:"message"`
}

// Plan is a backend-neutral view of an executable plan. The on-disk
// representation may add backend extensions; this is the core subset.
type Plan struct {
	Ref       core.TransactionRef `json:"source"`
	Operations []PlanOperation    `json:"operations"`
}

// PlanOperation is one row-level step the executor will perform.
type PlanOperation struct {
	Sequence int             `json:"sequence"`
	Table    core.TableRef   `json:"table"`
	Kind     core.OperationKind `json:"kind"`
	Key      core.Row        `json:"key"`
	Expect   core.Row        `json:"expect"`
	Write    core.Row        `json:"write"`
}

// ExecutionResult is the outcome of Apply.
type ExecutionResult struct {
	CompensatingGTID string `json:"compensating_gtid,omitempty"`
	AffectedRows     int    `json:"affected_rows"`
	ActionID         string `json:"action_id,omitempty"`
}

// PlanExecutor checks and applies plans against the target database.
// Check is read-only; Apply writes.
type PlanExecutor interface {
	Check(ctx context.Context, plan Plan) ([]Conflict, error)
	Apply(ctx context.Context, plan Plan, actionID string) (ExecutionResult, error)
}

// Backend bundles every interface a backend must satisfy.
type Backend interface {
	Name() string
	ChangeSource
	SchemaInspector
	PlanExecutor
}
