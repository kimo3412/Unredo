package planner

import (
	"testing"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

func TestBuildResolvedOverwriteBindsCurrentRow(t *testing.T) {
	parent := resolvedTestParent(core.OpUpdate, orderWithID(7), orderWithID(7), withStatus(orderWithID(7), "paid"))
	current := withStatus(orderWithID(7), "external")
	conflict := ports.Conflict{
		OperationSequence: 1, Table: sampleOrdersSchema().Table,
		Kind: "row_mismatch", Column: "status", Current: current,
	}
	check := resolvedCheck(parent, conflict)
	digest := ConflictDigest(parent.Digest, 1, check.Conflicts)

	resolved, err := BuildResolved(parent, check, ResolveOptions{
		Operator: "alice", Reason: "INC-42", ToolVersion: "test",
		Decisions: []Resolution{{OperationSequence: 1, Decision: DecisionOverwrite, ConflictDigest: digest}},
	})
	if err != nil {
		t.Fatalf("BuildResolved: %v", err)
	}
	if resolved.ExecutionClass != ClassUnsafeResolved || resolved.ParentPlanDigest != parent.Digest || resolved.RootPlanDigest != parent.Digest {
		t.Fatalf("unexpected resolved metadata: %+v", resolved)
	}
	if len(resolved.Operations) != 1 || !equalRow(resolved.Operations[0].Expect, current) {
		t.Fatalf("resolved expectation was not bound to current row")
	}
	if resolved.ResolutionOperator != "alice" || resolved.ResolutionReason != "INC-42" {
		t.Fatalf("resolution audit fields missing")
	}
}

func TestBuildResolvedSkipRemovesOnlyConflictingOperation(t *testing.T) {
	parent := resolvedTestParent(core.OpDelete, orderWithID(7), orderWithID(7), core.Row{})
	parent.Operations = append(parent.Operations, ports.PlanOperation{
		Sequence: 2, Table: sampleOrdersSchema().Table, Kind: core.OpDelete,
		Key: idRow(8), Expect: orderWithID(8),
	})
	parent.Digest = computeDigest(parent)
	conflict := ports.Conflict{OperationSequence: 1, Table: sampleOrdersSchema().Table, Kind: "row_missing"}
	check := resolvedCheck(parent, conflict)
	resolved, err := BuildResolved(parent, check, ResolveOptions{
		Operator: "alice", Reason: "already gone", ToolVersion: "test",
		Decisions: []Resolution{{OperationSequence: 1, Decision: DecisionSkip, ConflictDigest: ConflictDigest(parent.Digest, 1, check.Conflicts)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Operations) != 1 || resolved.Operations[0].Sequence != 1 {
		t.Fatalf("skip produced unexpected operations: %+v", resolved.Operations)
	}
	value, _ := resolved.Operations[0].Key.Get("id")
	if !value.Equal(idRow(8).Values[0]) {
		t.Fatal("skip removed the wrong operation")
	}
}

func TestBuildResolvedOverwriteConvertsMissingUpdateToInsert(t *testing.T) {
	parent := resolvedTestParent(core.OpUpdate, orderWithID(7), orderWithID(7), withStatus(orderWithID(7), "restored"))
	conflict := ports.Conflict{OperationSequence: 1, Table: sampleOrdersSchema().Table, Kind: "row_missing"}
	check := resolvedCheck(parent, conflict)
	resolved, err := BuildResolved(parent, check, ResolveOptions{
		Operator: "alice", Reason: "restore missing row", ToolVersion: "test",
		Decisions: []Resolution{{OperationSequence: 1, Decision: DecisionOverwrite, ConflictDigest: ConflictDigest(parent.Digest, 1, check.Conflicts)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Operations) != 1 || resolved.Operations[0].Kind != core.OpInsert {
		t.Fatalf("missing UPDATE should become INSERT: %+v", resolved.Operations)
	}
}

func TestBuildResolvedOverwriteConvertsExistingInsertToUpdate(t *testing.T) {
	desired := withStatus(orderWithID(7), "restored")
	parent := resolvedTestParent(core.OpInsert, orderWithID(7), core.Row{}, desired)
	current := withStatus(orderWithID(7), "external")
	conflict := ports.Conflict{OperationSequence: 1, Table: sampleOrdersSchema().Table, Kind: "row_exists", Current: current}
	check := resolvedCheck(parent, conflict)
	resolved, err := BuildResolved(parent, check, ResolveOptions{
		Operator: "alice", Reason: "replace external row", ToolVersion: "test",
		Decisions: []Resolution{{OperationSequence: 1, Decision: DecisionOverwrite, ConflictDigest: ConflictDigest(parent.Digest, 1, check.Conflicts)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Operations) != 1 || resolved.Operations[0].Kind != core.OpUpdate || !equalRow(resolved.Operations[0].Expect, current) {
		t.Fatalf("existing INSERT should become current-bound UPDATE: %+v", resolved.Operations)
	}
}

func TestBuildResolvedRejectsStaleConflictDigest(t *testing.T) {
	parent := resolvedTestParent(core.OpDelete, orderWithID(7), orderWithID(7), core.Row{})
	check := resolvedCheck(parent, ports.Conflict{OperationSequence: 1, Table: sampleOrdersSchema().Table, Kind: "row_missing"})
	_, err := BuildResolved(parent, check, ResolveOptions{
		Operator: "alice", Reason: "test", ToolVersion: "test",
		Decisions: []Resolution{{OperationSequence: 1, Decision: DecisionSkip, ConflictDigest: "sha256:stale"}},
	})
	if err == nil {
		t.Fatal("expected stale conflict digest to be rejected")
	}
}

func TestBuildResolvedOverwriteDropsUpdateWhenTargetAlreadyReached(t *testing.T) {
	desired := withStatus(orderWithID(7), "restored")
	parent := resolvedTestParent(core.OpUpdate, orderWithID(7), withStatus(orderWithID(7), "changed"), desired)
	conflict := ports.Conflict{OperationSequence: 1, Table: sampleOrdersSchema().Table, Kind: "row_mismatch", Current: desired}
	check := resolvedCheck(parent, conflict)
	resolved, err := BuildResolved(parent, check, ResolveOptions{
		Operator: "alice", Reason: "target already reached", ToolVersion: "test",
		Decisions: []Resolution{{OperationSequence: 1, Decision: DecisionOverwrite, ConflictDigest: ConflictDigest(parent.Digest, 1, check.Conflicts)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Operations) != 0 || len(resolved.Resolutions) != 1 {
		t.Fatalf("already-achieved update should be an audited no-op: %+v", resolved)
	}
}

func resolvedTestParent(kind core.OperationKind, keySource, expect, write core.Row) *Plan {
	p := &Plan{
		FormatVersion: FormatVersion, PlanID: "01K1TEST000000000000000010",
		Mode: ModeRevert, ExecutionClass: ClassSafe,
		Source:             core.TransactionRef{Backend: "mysql", InstanceID: "uuid", NativeTransactionID: "uuid:1"},
		SchemaFingerprints: map[string]string{"unredo_shop.orders": "sha256:abc"},
		Operations: []ports.PlanOperation{{
			Sequence: 1, Table: sampleOrdersSchema().Table, Kind: kind,
			Key: projectColumns(keySource, []string{"id"}), Expect: expect, Write: write,
		}},
	}
	p.Digest = computeDigest(p)
	return p
}

func resolvedCheck(parent *Plan, conflicts ...ports.Conflict) *ports.CheckResult {
	return &ports.CheckResult{Status: "CONFLICT", PlanDigest: parent.Digest, Conflicts: conflicts, OperationsTotal: len(parent.Operations)}
}
