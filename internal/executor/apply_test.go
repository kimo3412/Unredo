package executor

import (
	"testing"

	"github.com/girimi/unredo/internal/ports"
)

func TestApplyOptionsValidate(t *testing.T) {
	good := ApplyOptions{
		PlanID:                    make([]byte, 16),
		ActionID:                  make([]byte, 16),
		ActionType:                "REVERT",
		TargetState:               "ORIGINAL_REVERTED",
		ChainDepth:                0,
		OperatorName:              "tester",
		SourceNativeTransactionID: "uuid:1",
		ExecutionClass:            "SAFE",
	}
	if err := good.Validate(); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}

	cases := []struct {
		name string
		mut  func(*ApplyOptions)
	}{
		{"short plan_id", func(o *ApplyOptions) { o.PlanID = []byte{1} }},
		{"short action_id", func(o *ApplyOptions) { o.ActionID = []byte{1} }},
		{"bad action_type", func(o *ApplyOptions) { o.ActionType = "ROTATE" }},
		{"bad target_state", func(o *ApplyOptions) { o.TargetState = "WHATEVER" }},
		{"bad execution_class", func(o *ApplyOptions) { o.ExecutionClass = "MAYBE" }},
		{"missing operator", func(o *ApplyOptions) { o.OperatorName = "" }},
		{"missing source txn", func(o *ApplyOptions) { o.SourceNativeTransactionID = "" }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := good
			c.mut(&o)
			if err := o.Validate(); err == nil {
				t.Fatalf("expected error for %s", c.name)
			}
		})
	}
}

func TestApplyRequestThroughPorts(t *testing.T) {
	// ports.ApplyRequest is the type the backend interface accepts; this
	// test guards against accidental field renames.
	req := ports.ApplyRequest{
		ActionID:     "01J",
		OperatorName: "ops",
		Reason:       "incident",
		Confirm:      "abcdef12",
	}
	if req.ActionID != "01J" {
		t.Fatal("ActionID should round-trip")
	}
}
