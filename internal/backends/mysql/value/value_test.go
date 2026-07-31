package value

import (
	"testing"

	"github.com/girimi/unredo/internal/core"
)

func TestTemporalKinds(t *testing.T) {
	tests := map[string]core.ValueKind{
		"date":         core.KindDate,
		"time(6)":      core.KindTime,
		"datetime(6)":  core.KindDateTime,
		"timestamp(6)": core.KindDateTime,
	}
	for native, want := range tests {
		if got := kindForType(native); got != want {
			t.Errorf("kindForType(%q)=%q, want %q", native, got, want)
		}
	}
}
