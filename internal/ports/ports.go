// Package ports declares the backend interfaces the core depends on.
// Implementations live under internal/backends/* and are wired by the
// registry at startup. Core must not import any concrete backend here.
package ports

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/girimi/unredo/internal/core"
)

// Stable domain errors. Backends translate native errors into one of these
// so the CLI can map them to the documented exit codes.
var (
	ErrTransactionNotFound   = errors.New("transaction not found")
	ErrUnsupportedCapability = errors.New("unsupported capability")
	ErrCommitUnknown         = errors.New("commit result unknown")
	ErrSchemaMismatch        = errors.New("schema mismatch")
	ErrPlanDigestMismatch    = errors.New("plan digest mismatch")
	ErrInstanceMismatch      = errors.New("instance id mismatch")
	ErrActionNotFound        = errors.New("action not found")
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

// LogFile is one finite archived change-log file available to a backend.
// Name is opaque to core callers and is passed back in a backend cursor.
type LogFile struct {
	Name       string    `json:"name"`
	Size       int64     `json:"size"`
	ModifiedAt time.Time `json:"modified_at"`
}

// LogCatalog enumerates finite archive files for offline indexing. Live
// replication backends may return ErrUnsupportedCapability.
type LogCatalog interface {
	ListLogFiles(ctx context.Context) ([]LogFile, error)
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
	OperationSequence int           `json:"operation_sequence"`
	Table             core.TableRef `json:"table"`
	Kind              string        `json:"kind"`
	Column            string        `json:"column,omitempty"`
	Expected          core.Value    `json:"expected,omitempty"`
	Actual            core.Value    `json:"actual,omitempty"`
	ExpectedDigest    string        `json:"expected_digest,omitempty"`
	ActualDigest      string        `json:"actual_digest,omitempty"`
	Current           core.Row      `json:"current,omitempty"`
	Message           string        `json:"message"`
}

type SchemaCheck struct {
	Table        core.TableRef `json:"table"`
	PlanDigest   string        `json:"plan_digest"`
	ActualDigest string        `json:"actual_digest"`
	Match        bool          `json:"match"`
}

type CheckResult struct {
	Status          string        `json:"status"`
	PlanDigest      string        `json:"plan_digest"`
	TargetInstance  string        `json:"target_instance"`
	SchemaChecks    []SchemaCheck `json:"schema_checks"`
	Conflicts       []Conflict    `json:"conflicts"`
	OperationsTotal int           `json:"operations_total"`
}

// Plan is a backend-neutral view of an executable plan. The on-disk
// representation may add backend extensions; this is the core subset.
type Plan struct {
	PlanID             string              `json:"plan_id,omitempty"`
	ToolVersion        string              `json:"tool_version,omitempty"`
	Mode               string              `json:"mode,omitempty"`            // "revert" or "reapply"
	ExecutionClass     string              `json:"execution_class,omitempty"` // "safe" or "unsafe_resolved"
	Ref                core.TransactionRef `json:"source"`
	Operations         []PlanOperation     `json:"operations"`
	SchemaFingerprints map[string]string   `json:"schema_fingerprints,omitempty"`
	Digest             string              `json:"digest,omitempty"`
	RootPlanDigest     string              `json:"root_plan_digest,omitempty"`
	ParentActionID     string              `json:"parent_action_id,omitempty"`
	ChainDepth         uint32              `json:"chain_depth,omitempty"`
	ParentPlanDigest   string              `json:"parent_plan_digest,omitempty"`
}

// PlanOperation is one row-level step the executor will perform.
type PlanOperation struct {
	Sequence int                `json:"sequence"`
	Table    core.TableRef      `json:"table"`
	Kind     core.OperationKind `json:"kind"`
	Key      core.Row           `json:"key"`
	Expect   core.Row           `json:"expect"`
	Write    core.Row           `json:"write"`
}

// ExecutionResult is the outcome of Apply.
type ExecutionResult struct {
	CompensatingGTID       string `json:"compensating_gtid,omitempty"`
	GTIDCorrelationWarning string `json:"gtid_correlation_warning,omitempty"`
	AffectedRows           int    `json:"affected_rows"`
	ActionID               string `json:"action_id,omitempty"`
}

// ApplyRequest is the per-invocation state an executor needs that is
// not already on the plan: an action id, the operator name, the
// reason, and the short-confirm required for non-interactive flows.
type ApplyRequest struct {
	ActionID     string
	OperatorName string
	Reason       string
	Confirm      string
}

type Action struct {
	ActionID                  string    `json:"action_id"`
	PlanID                    string    `json:"plan_id"`
	ParentActionID            string    `json:"parent_action_id,omitempty"`
	RootPlanDigest            string    `json:"root_plan_digest"`
	ActionType                string    `json:"action_type"`
	TargetState               string    `json:"target_state"`
	ChainDepth                uint32    `json:"chain_depth"`
	SourceNativeTransactionID string    `json:"source_native_transaction_id"`
	PlanDigest                string    `json:"plan_digest"`
	ExecutionClass            string    `json:"execution_class"`
	Reason                    string    `json:"reason,omitempty"`
	ToolVersion               string    `json:"tool_version"`
	OperatorName              string    `json:"operator_name"`
	CreatedAt                 time.Time `json:"created_at"`
}

type ActionStore interface {
	FindAction(ctx context.Context, actionID string) (*Action, error)
	LatestAction(ctx context.Context, rootPlanDigest string) (*Action, error)
}

// TargetIdentifier exposes the concrete execution target identity used to
// prevent an absence check against the wrong instance.
type TargetIdentifier interface {
	TargetInstanceID() string
}

type ActionVerificationStatus string

const (
	ActionCommitted     ActionVerificationStatus = "COMMITTED"
	ActionNotCommitted  ActionVerificationStatus = "NOT_COMMITTED"
	ActionIndeterminate ActionVerificationStatus = "INDETERMINATE"
)

type ActionVerification struct {
	Status   ActionVerificationStatus `json:"status"`
	ActionID string                   `json:"action_id"`
	PlanID   string                   `json:"plan_id"`
	Message  string                   `json:"message"`
	Action   *Action                  `json:"action,omitempty"`
}

// PlanExecutor checks and applies plans against the target database.
// Check is read-only; Apply writes.
type PlanExecutor interface {
	Check(ctx context.Context, plan Plan) (*CheckResult, error)
	Apply(ctx context.Context, plan Plan, req ApplyRequest) (ExecutionResult, error)
}

// Backend bundles every interface a backend must satisfy.
type Backend interface {
	Name() string
	ChangeSource
	SchemaInspector
	PlanExecutor
}
