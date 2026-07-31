// Package core defines database-agnostic types shared by every backend.
// It must not import any backend-specific package.
package core

import (
	"encoding/json"
	"fmt"
)

// ValueKind is the universal category of a field value.
// Backends map their native types to one of these kinds.
type ValueKind string

const (
	KindInteger  ValueKind = "integer"
	KindDecimal  ValueKind = "decimal"
	KindFloat    ValueKind = "float"
	KindText     ValueKind = "text"
	KindBinary   ValueKind = "binary"
	KindDate     ValueKind = "date"
	KindTime     ValueKind = "time"
	KindDateTime ValueKind = "datetime"
	KindJSON     ValueKind = "json"
	KindBit      ValueKind = "bit"
	KindEnum     ValueKind = "enum"
	KindSet      ValueKind = "set"
	KindUnknown  ValueKind = "unknown"
)

// NativeType is a backend-supplied type label such as "mysql:decimal(20,4)".
// It is opaque to the core but kept on the wire so backend decoding is lossless.
type NativeType string

// Value is the universal representation of a field value.
// Data is the canonical JSON form for the kind. Native preserves the
// original type tag for backend re-encoding during apply.
type Value struct {
	Kind     ValueKind   `json:"kind"`
	Null     bool        `json:"null"`
	Encoding string      `json:"encoding,omitempty"`
	Data     RawJSON     `json:"data,omitempty"`
	Native   *NativeType `json:"native,omitempty"`
}

// RawJSON is a JSON value with deterministic encoding.
type RawJSON []byte

// MarshalJSON ensures the bytes are emitted as-is.
func (r RawJSON) MarshalJSON() ([]byte, error) {
	if r == nil {
		return []byte("null"), nil
	}
	return r, nil
}

// UnmarshalJSON stores the raw bytes.
func (r *RawJSON) UnmarshalJSON(b []byte) error {
	if b == nil {
		*r = nil
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	*r = out
	return nil
}

// String returns a human-readable summary. Full values must go through Redact.
func (v Value) String() string {
	if v.Null {
		return "NULL"
	}
	if len(v.Data) == 0 {
		return ""
	}
	return string(v.Data)
}

// Equal compares two values using backend-neutral rules.
// Numeric kinds compare numerically; text and binary compare by bytes;
// NULL is only equal to NULL. Unknown kinds fall back to byte comparison.
func (v Value) Equal(other Value) bool {
	if v.Null != other.Null {
		return false
	}
	if v.Null {
		return true
	}
	if v.Kind != other.Kind {
		return false
	}
	return string(v.Data) == string(other.Data)
}

// Validate returns an error if the value is not self-consistent.
func (v Value) Validate() error {
	if v.Null {
		return nil
	}
	if v.Kind == KindUnknown {
		return fmt.Errorf("value has unknown kind")
	}
	if len(v.Data) == 0 {
		return fmt.Errorf("value has kind %q but no data", v.Kind)
	}
	if !json.Valid(v.Data) {
		return fmt.Errorf("value data is not valid JSON: %s", v.Data)
	}
	return nil
}
