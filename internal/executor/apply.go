package executor

import (
	"context"
	"errors"
	"fmt"

	"github.com/girimi/unredo/internal/ports"
)

// ApplyOptions is the runtime configuration for an apply.
type ApplyOptions struct {
	// PlanID is the per-plan unique id used as the marker table's
	// UNIQUE constraint. Repeated apply of the same plan must fail
	// at the marker INSERT.
	PlanID []byte
	// ActionID is the new action's primary key.
	ActionID []byte
	// ActionType is "REVERT" or "REAPPLY".
	ActionType string
	// TargetState is "ORIGINAL_APPLIED" or "ORIGINAL_REVERTED".
	TargetState string
	// ChainDepth is 0 for the first action against a root plan.
	ChainDepth uint32
	// ParentActionID is nil for the first action.
	ParentActionID []byte
	// OperatorName is recorded in the marker; it is required.
	OperatorName string
	// Reason is optional free text (e.g. incident ticket).
	Reason string
	// ExecutionClass is "SAFE" for default plans.
	ExecutionClass string
	// SourceNativeTransactionID is the original txn id the plan reverts.
	SourceNativeTransactionID string
	// Confirm matches the first 8 chars of the plan digest. Plans
	// carry it in ports.Plan.Digest; the caller is responsible for
	// matching. An empty string skips the check (M2 build mode).
	Confirm string
}

// Validate returns an error if the options are incomplete or have an
// invalid combination. This is the place to enforce the schema
// constraints DESIGN.md §8 documents for action_markers.
func (o ApplyOptions) Validate() error {
	if len(o.PlanID) != 16 {
		return fmt.Errorf("executor: plan_id must be 16 bytes, got %d", len(o.PlanID))
	}
	if len(o.ActionID) != 16 {
		return fmt.Errorf("executor: action_id must be 16 bytes, got %d", len(o.ActionID))
	}
	switch o.ActionType {
	case "REVERT", "REAPPLY":
	default:
		return fmt.Errorf("executor: action_type must be REVERT or REAPPLY, got %q", o.ActionType)
	}
	switch o.TargetState {
	case "ORIGINAL_APPLIED", "ORIGINAL_REVERTED":
	default:
		return fmt.Errorf("executor: target_state must be ORIGINAL_APPLIED or ORIGINAL_REVERTED, got %q", o.TargetState)
	}
	switch o.ExecutionClass {
	case "SAFE", "UNSAFE_RESOLVED":
	default:
		return fmt.Errorf("executor: execution_class must be SAFE or UNSAFE_RESOLVED, got %q", o.ExecutionClass)
	}
	if o.OperatorName == "" {
		return fmt.Errorf("executor: operator_name is required")
	}
	if o.SourceNativeTransactionID == "" {
		return fmt.Errorf("executor: source_native_transaction_id is required")
	}
	return nil
}

// Applier is what the executor needs from a backend to apply a plan.
// It is paired with Reader for the read side.
type Applier interface {
	Apply(ctx context.Context, plan *ports.Plan, opts ApplyOptions) (ports.ExecutionResult, error)
}

// ApplyResult is the parsed outcome. The MySQL backend captures
// affected_rows, the action_id, and the GTID of the compensating
// transaction when the session exposes it.
type ApplyResult = ports.ExecutionResult

// ErrApplyConflict is returned when an operation's precondition fails
// between check and apply. The error wraps the per-op conflict
// message so the CLI can surface the row/column that drifted.
var ErrApplyConflict = errors.New("executor: apply precondition failed")

// ErrApplyReplayed is returned when the plan_id UNIQUE constraint
// blocks a second apply. Callers should NOT retry; the data was
// already compensated on the first run.
var ErrApplyReplayed = errors.New("executor: plan already applied (plan_id already in action_markers)")
