// Package redact masks sensitive fields in logs and table output. The
// default is to never print full row images; callers pass the
// `--show-values` flag to opt in.
package redact

import (
	"fmt"

	"github.com/girimi/unredo/internal/core"
)

// Mode controls whether full values are emitted.
type Mode int

const (
	// ModeSafe hides full values; only key columns and a short summary show.
	ModeSafe Mode = iota
	// ModeFull prints every value. The caller is responsible for warning
	// the user and limiting where this is used.
	ModeFull
)

// RowSummary returns a one-line summary safe to print in any log.
// It only includes column names and a digest of the value bytes.
func RowSummary(r core.Row) string {
	if len(r.Columns) == 0 {
		return "<empty>"
	}
	out := "{"
	for i, c := range r.Columns {
		if i > 0 {
			out += ", "
		}
		v := "<nil>"
		if i < len(r.Values) {
			v = valueSummary(r.Values[i])
		}
		out += fmt.Sprintf("%s=%s", c, v)
	}
	return out + "}"
}

func valueSummary(v core.Value) string {
	if v.Null {
		return "NULL"
	}
	if v.Kind == core.KindBinary || v.Kind == core.KindBit {
		return fmt.Sprintf("<%s len=%d>", v.Kind, len(v.Data))
	}
	if len(v.Data) > 32 {
		return fmt.Sprintf("<%s len=%d>", v.Kind, len(v.Data))
	}
	return string(v.Data)
}
