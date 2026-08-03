package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/planner"
	"github.com/girimi/unredo/internal/ports"
)

type fakeActionStore struct {
	find   func(context.Context, string) (*ports.Action, error)
	latest func(context.Context, string) (*ports.Action, error)
}

func (f fakeActionStore) FindAction(ctx context.Context, actionID string) (*ports.Action, error) {
	return f.find(ctx, actionID)
}

func (f fakeActionStore) LatestAction(ctx context.Context, rootDigest string) (*ports.Action, error) {
	if f.latest != nil {
		return f.latest(ctx, rootDigest)
	}
	return nil, ports.ErrActionNotFound
}

func TestVerifyActionOutcomeCommittedOnlyOnExactPlanMatch(t *testing.T) {
	plan := verificationPlan()
	action := &ports.Action{
		ActionID: "01K1VERIFY00000000000000001", PlanID: plan.PlanID,
		PlanDigest: plan.Digest, SourceNativeTransactionID: plan.Source.NativeTransactionID,
	}
	result := verifyActionOutcome(context.Background(), fakeActionStore{find: func(context.Context, string) (*ports.Action, error) {
		return action, nil
	}}, "target-uuid", action.ActionID, plan, 0, 0)
	if result.Status != ports.ActionCommitted || result.Action != action {
		t.Fatalf("unexpected verification: %+v", result)
	}
}

func TestVerifyActionOutcomeRejectsMismatchedMarker(t *testing.T) {
	plan := verificationPlan()
	action := &ports.Action{ActionID: "01K1VERIFY00000000000000002", PlanID: plan.PlanID, PlanDigest: "sha256:other", SourceNativeTransactionID: plan.Source.NativeTransactionID}
	result := verifyActionOutcome(context.Background(), fakeActionStore{find: func(context.Context, string) (*ports.Action, error) {
		return action, nil
	}}, "target-uuid", action.ActionID, plan, 0, 0)
	if result.Status != ports.ActionIndeterminate || !strings.Contains(result.Message, "does not match") {
		t.Fatalf("unexpected verification: %+v", result)
	}
}

func TestVerifyActionOutcomeRejectsWrongTargetInstance(t *testing.T) {
	plan := verificationPlan()
	queried := false
	result := verifyActionOutcome(context.Background(), fakeActionStore{find: func(context.Context, string) (*ports.Action, error) {
		queried = true
		return nil, ports.ErrActionNotFound
	}}, "wrong-target-uuid", "01K1VERIFY00000000000000007", plan, 0, 0)
	if result.Status != ports.ActionIndeterminate || !strings.Contains(result.Message, "does not match") || queried {
		t.Fatalf("unexpected verification: %+v queried=%t", result, queried)
	}
}

func TestVerifyActionOutcomeNotCommittedAfterWindow(t *testing.T) {
	plan := verificationPlan()
	result := verifyActionOutcome(context.Background(), fakeActionStore{find: func(context.Context, string) (*ports.Action, error) {
		return nil, ports.ErrActionNotFound
	}}, "target-uuid", "01K1VERIFY00000000000000003", plan, 0, 0)
	if result.Status != ports.ActionNotCommitted {
		t.Fatalf("unexpected verification: %+v", result)
	}
}

func TestVerifyActionOutcomeQueryFailureIsIndeterminate(t *testing.T) {
	plan := verificationPlan()
	result := verifyActionOutcome(context.Background(), fakeActionStore{find: func(context.Context, string) (*ports.Action, error) {
		return nil, errors.New("network unavailable")
	}}, "target-uuid", "01K1VERIFY00000000000000004", plan, 0, 0)
	if result.Status != ports.ActionIndeterminate || !strings.Contains(result.Message, "network unavailable") {
		t.Fatalf("unexpected verification: %+v", result)
	}
}

func TestVerifyActionOutcomePollsUntilMarkerAppears(t *testing.T) {
	plan := verificationPlan()
	actionID := "01K1VERIFY00000000000000005"
	calls := 0
	store := fakeActionStore{find: func(context.Context, string) (*ports.Action, error) {
		calls++
		if calls == 1 {
			return nil, ports.ErrActionNotFound
		}
		return &ports.Action{ActionID: actionID, PlanID: plan.PlanID, PlanDigest: plan.Digest, SourceNativeTransactionID: plan.Source.NativeTransactionID}, nil
	}}
	result := verifyActionOutcome(context.Background(), store, "target-uuid", actionID, plan, 100*time.Millisecond, time.Millisecond)
	if result.Status != ports.ActionCommitted || calls < 2 {
		t.Fatalf("unexpected verification: %+v calls=%d", result, calls)
	}
}

func TestCommitUnknownRecoveryPrintsQueryableActionID(t *testing.T) {
	command := &cobra.Command{}
	var output bytes.Buffer
	command.SetOut(&output)
	printCommitUnknownRecovery(command, "01K1VERIFY00000000000000006", "plans/undo.json")
	got := output.String()
	for _, want := range []string{"COMMIT_UNKNOWN", "01K1VERIFY00000000000000006", "action verify", "FORBIDDEN"} {
		if !strings.Contains(got, want) {
			t.Fatalf("recovery output missing %q:\n%s", want, got)
		}
	}
}

func TestValidateActionParentAcceptsLatestAlternatingAction(t *testing.T) {
	root := verificationPlan()
	root.Mode = planner.ModeRevert
	root.ExecutionClass = planner.ClassSafe
	action := &ports.Action{
		ActionID:                  "01K1VERIFY00000000000000008",
		ActionType:                "REAPPLY",
		TargetState:               "ORIGINAL_APPLIED",
		RootPlanDigest:            root.Digest,
		PlanDigest:                "sha256:child-plan",
		SourceNativeTransactionID: root.Source.NativeTransactionID,
		ChainDepth:                1,
	}
	store := fakeActionStore{latest: func(context.Context, string) (*ports.Action, error) {
		return action, nil
	}}
	if err := validateActionParent(context.Background(), store, root, action, "REAPPLY", "ORIGINAL_APPLIED"); err != nil {
		t.Fatalf("latest alternating action rejected: %v", err)
	}
}

func TestValidateActionParentRejectsStaleAction(t *testing.T) {
	root := verificationPlan()
	root.Mode = planner.ModeRevert
	root.ExecutionClass = planner.ClassSafe
	action := &ports.Action{
		ActionID:                  "01K1VERIFY00000000000000009",
		ActionType:                "REVERT",
		TargetState:               "ORIGINAL_REVERTED",
		RootPlanDigest:            root.Digest,
		SourceNativeTransactionID: root.Source.NativeTransactionID,
		ChainDepth:                2,
	}
	store := fakeActionStore{latest: func(context.Context, string) (*ports.Action, error) {
		return &ports.Action{ActionID: "01K1VERIFY00000000000000010"}, nil
	}}
	err := validateActionParent(context.Background(), store, root, action, "REVERT", "ORIGINAL_REVERTED")
	if err == nil || !strings.Contains(err.Error(), "not the latest") {
		t.Fatalf("expected stale action rejection, got %v", err)
	}
}

func verificationPlan() *planner.Plan {
	return &planner.Plan{
		PlanID: "01K1VERIFY00000000000000000",
		Digest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		Source: core.TransactionRef{InstanceID: "target-uuid", NativeTransactionID: "server:42"},
	}
}
