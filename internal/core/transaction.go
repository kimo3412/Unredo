package core

import (
	"encoding/json"
	"fmt"
	"time"
)

// OperationKind names the type of row change in a transaction.
type OperationKind string

const (
	OpInsert OperationKind = "insert"
	OpUpdate OperationKind = "update"
	OpDelete OperationKind = "delete"
)

// Valid reports whether k is a known operation kind.
func (k OperationKind) Valid() bool {
	switch k {
	case OpInsert, OpUpdate, OpDelete:
		return true
	}
	return false
}

// TableRef names a table by its fully-qualified schema and name.
type TableRef struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// String returns schema.name.
func (t TableRef) String() string { return fmt.Sprintf("%s.%s", t.Schema, t.Name) }

// Row is an ordered set of column values keyed by column name.
// The order matches the schema at event time, recorded in RowChange.SchemaColumns.
type Row struct {
	Columns []string `json:"columns"`
	Values  []Value  `json:"values"`
}

// Get returns the value for column, or false if absent.
func (r Row) Get(column string) (Value, bool) {
	for i, c := range r.Columns {
		if c == column && i < len(r.Values) {
			return r.Values[i], true
		}
	}
	return Value{}, false
}

// Equal compares two rows column-by-column.
func (r Row) Equal(other Row) bool {
	if len(r.Columns) != len(other.Columns) {
		return false
	}
	for i, c := range r.Columns {
		if c != other.Columns[i] {
			return false
		}
		if !r.Values[i].Equal(other.Values[i]) {
			return false
		}
	}
	return true
}

// RowChange is one row event within a transaction.
// Sequence is 0-based and reflects the original event order; revert
// applies operations in reverse Sequence order.
type RowChange struct {
	Sequence          int           `json:"sequence"`
	Table             TableRef      `json:"table"`
	Operation         OperationKind `json:"operation"`
	Key               Row           `json:"key"`
	Before            Row           `json:"before"`
	After             Row           `json:"after"`
	SchemaColumns     []string      `json:"schema_columns,omitempty"`
	SchemaFingerprint string        `json:"schema_fingerprint,omitempty"`
}

// TransactionRef identifies a transaction in a way the core can pass around
// without knowing how the backend internally represents it.
type TransactionRef struct {
	Backend             string          `json:"backend"`
	InstanceID          string          `json:"instance_id"`
	NativeTransactionID string          `json:"native_transaction_id"`
	Cursor              json.RawMessage `json:"cursor,omitempty"`
}

// Transaction is the materialized body of one transaction.
type Transaction struct {
	Ref        TransactionRef `json:"ref"`
	StartTime  time.Time      `json:"start_time"`
	CommitTime time.Time      `json:"commit_time"`
	GTID       string         `json:"gtid,omitempty"`
	RowCount   int            `json:"row_count"`
	Tables     []TableRef     `json:"tables,omitempty"`
	Rows       []RowChange    `json:"rows"`

	// Executable reports whether the planner can turn this transaction
	// into a safe revert/reapply plan given the backend capabilities.
	// Reasons is non-empty when Executable is false.
	Executable bool     `json:"executable"`
	Reasons    []string `json:"reasons,omitempty"`
}

// BackendCapabilities describes what a backend instance can deliver.
// Core consults these before assuming full before/after images or atomic
// action markers; missing capabilities are fail-closed, never downgraded.
type BackendCapabilities struct {
	FullBeforeImage       bool `json:"full_before_image"`
	FullAfterImage        bool `json:"full_after_image"`
	StableTransactionID   bool `json:"stable_transaction_id"`
	TransactionBoundaries bool `json:"transaction_boundaries"`
	AtomicActionMarker    bool `json:"atomic_action_marker"`
	SchemaAtEventTime     bool `json:"schema_at_event_time"`
	SupportsReapply       bool `json:"supports_reapply"`
}

// SchemaFingerprint is a stable hash over a table definition. Any drift
// invalidates previously generated plans; the planner refuses to execute.
type SchemaFingerprint string

// ColumnDef describes one column as reported by the backend.
type ColumnDef struct {
	Name       string     `json:"name"`
	Ordinal    int        `json:"ordinal"`
	NativeType NativeType `json:"native_type"`
	Nullable   bool       `json:"nullable"`
	Charset    string     `json:"charset,omitempty"`
	Collation  string     `json:"collation,omitempty"`
	Generated  bool       `json:"generated,omitempty"`
}

// UniqueKey is an ordered set of columns that uniquely identify a row.
type UniqueKey struct {
	Name      string   `json:"name"`
	Columns   []string `json:"columns"`
	IsPrimary bool     `json:"is_primary"`
}

// TableSchema is what a planner needs to construct safe operations.
type TableSchema struct {
	Table      TableRef    `json:"table"`
	Engine     string      `json:"engine"`
	Columns    []ColumnDef `json:"columns"`
	UniqueKeys []UniqueKey `json:"unique_keys"`
	Charset    string      `json:"charset,omitempty"`
	Collation  string      `json:"collation,omitempty"`
}

// All reports whether every supported flag is set. Convenience for tests
// and for doctor checks that demand a fully usable backend.
func (c BackendCapabilities) All() bool {
	return c.FullBeforeImage && c.FullAfterImage &&
		c.StableTransactionID && c.TransactionBoundaries &&
		c.AtomicActionMarker && c.SchemaAtEventTime &&
		c.SupportsReapply
}
