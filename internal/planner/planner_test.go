package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

func TestBuildRevertInsertBecomesDelete(t *testing.T) {
	txn := sampleTxn(t, []core.RowChange{
		mkRow(1, core.OpInsert, core.Row{}, sampleOrders()),
	})
	p, err := Build(txn, ModeRevert, depsFor(t, sampleOrdersSchema()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(p.Operations) != 1 {
		t.Fatalf("expected 1 op, got %d", len(p.Operations))
	}
	op := p.Operations[0]
	if op.Kind != core.OpDelete {
		t.Errorf("expected DELETE, got %s", op.Kind)
	}
	if op.Sequence != 1 {
		t.Errorf("revert keeps order for single op; got seq=%d", op.Sequence)
	}
	if !equalRow(op.Expect, sampleOrders()) {
		t.Errorf("expect image mismatch")
	}
	if len(op.Write.Columns) != 0 {
		t.Errorf("delete should have empty write")
	}
}

func TestBuildRevertUpdateReversed(t *testing.T) {
	txn := sampleTxn(t, []core.RowChange{
		mkRow(1, core.OpInsert, core.Row{}, sampleOrders()),
		mkRow(2, core.OpUpdate, core.Row{}, withStatus(sampleOrders(), "cancelled")),
	})
	p, err := Build(txn, ModeRevert, depsFor(t, sampleOrdersSchema()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if len(p.Operations) != 2 {
		t.Fatalf("expected 2 ops, got %d", len(p.Operations))
	}
	// Revert must apply in reverse: the UPDATE comes first (sequence 1),
	// the DELETE second (sequence 2).
	if p.Operations[0].Kind != core.OpUpdate {
		t.Errorf("op[0] expected UPDATE (the later original), got %s", p.Operations[0].Kind)
	}
	if p.Operations[1].Kind != core.OpDelete {
		t.Errorf("op[1] expected DELETE (the earlier original), got %s", p.Operations[1].Kind)
	}
}

func TestBuildReapplyForwardOrder(t *testing.T) {
	txn := sampleTxn(t, []core.RowChange{
		mkRow(1, core.OpInsert, core.Row{}, sampleOrders()),
		mkRow(2, core.OpUpdate, core.Row{}, withStatus(sampleOrders(), "cancelled")),
	})
	p, err := Build(txn, ModeReapply, depsFor(t, sampleOrdersSchema()))
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if p.Operations[0].Kind != core.OpInsert {
		t.Errorf("reapply: op[0] should be INSERT, got %s", p.Operations[0].Kind)
	}
	if p.Operations[1].Kind != core.OpUpdate {
		t.Errorf("reapply: op[1] should be UPDATE, got %s", p.Operations[1].Kind)
	}
}

func TestBuildReapplyInvertsRootRevertAndPreservesChain(t *testing.T) {
	root := &Plan{
		FormatVersion:      FormatVersion,
		PlanID:             "01K1TEST000000000000000001",
		Mode:               ModeRevert,
		ExecutionClass:     ClassSafe,
		ToolVersion:        "test-root",
		Source:             sampleTxn(t, nil).Ref,
		SchemaFingerprints: map[string]string{"unredo_shop.orders": "sha256:abc"},
		Operations: []ports.PlanOperation{
			{Sequence: 1, Table: sampleOrdersSchema().Table, Kind: core.OpUpdate, Key: idRow(8), Expect: orderWithID(8), Write: orderWithID(7)},
			{Sequence: 2, Table: sampleOrdersSchema().Table, Kind: core.OpDelete, Key: idRow(9), Expect: orderWithID(9)},
		},
	}
	root.Digest = computeDigest(root)

	child, err := BuildReapply(root, "01K1TEST000000000000000002", 0, "test-child")
	if err != nil {
		t.Fatalf("BuildReapply: %v", err)
	}
	if child.Mode != ModeReapply || child.RootPlanDigest != root.Digest || child.ChainDepth != 1 {
		t.Fatalf("unexpected chain metadata: mode=%s root=%s depth=%d", child.Mode, child.RootPlanDigest, child.ChainDepth)
	}
	if child.ParentActionID != "01K1TEST000000000000000002" || len(child.Operations) != 2 {
		t.Fatalf("unexpected parent or operation count")
	}
	// Reapply reverses the revert execution order. The root DELETE becomes
	// an INSERT, followed by the inverse UPDATE.
	if child.Operations[0].Kind != core.OpInsert || child.Operations[1].Kind != core.OpUpdate {
		t.Fatalf("unexpected operation order: %s, %s", child.Operations[0].Kind, child.Operations[1].Kind)
	}
	if recomputeDigest(t, child) != child.Digest {
		t.Fatal("child digest is not stable")
	}
}

func TestBuildReapplyUsesRevertedPrimaryKeyForPKChangingUpdate(t *testing.T) {
	root := &Plan{
		FormatVersion:  FormatVersion,
		PlanID:         "01K1TEST000000000000000003",
		Mode:           ModeRevert,
		ExecutionClass: ClassSafe,
		Source:         sampleTxn(t, nil).Ref,
		Operations: []ports.PlanOperation{{
			Sequence: 1,
			Table:    sampleOrdersSchema().Table,
			Kind:     core.OpUpdate,
			Key:      idRow(8),
			Expect:   orderWithID(8),
			Write:    orderWithID(7),
		}},
	}
	root.Digest = computeDigest(root)
	child, err := BuildReapply(root, "01K1TEST000000000000000004", 0, "test")
	if err != nil {
		t.Fatal(err)
	}
	op := child.Operations[0]
	value, ok := op.Key.Get("id")
	if !ok || !value.Equal(idRow(7).Values[0]) {
		t.Fatalf("reapply key must address reverted id=7 row, got %+v", op.Key)
	}
	if !equalRow(op.Expect, orderWithID(7)) || !equalRow(op.Write, orderWithID(8)) {
		t.Fatal("reapply UPDATE images were not inverted")
	}
}

func TestBuildChainedRevertReusesRootDirectionAndAdvancesChain(t *testing.T) {
	root := &Plan{
		FormatVersion:      FormatVersion,
		PlanID:             "01K1TEST000000000000000005",
		Mode:               ModeRevert,
		ExecutionClass:     ClassSafe,
		Source:             sampleTxn(t, nil).Ref,
		SchemaFingerprints: map[string]string{"unredo_shop.orders": "sha256:abc"},
		Operations: []ports.PlanOperation{
			{Sequence: 1, Table: sampleOrdersSchema().Table, Kind: core.OpUpdate, Key: idRow(8), Expect: orderWithID(8), Write: orderWithID(7)},
			{Sequence: 2, Table: sampleOrdersSchema().Table, Kind: core.OpDelete, Key: idRow(9), Expect: orderWithID(9)},
		},
	}
	root.Digest = computeDigest(root)

	child, err := BuildChainedRevert(root, "01K1TEST000000000000000006", 1, "test-child")
	if err != nil {
		t.Fatalf("BuildChainedRevert: %v", err)
	}
	if child.Mode != ModeRevert || child.RootPlanDigest != root.Digest || child.ChainDepth != 2 {
		t.Fatalf("unexpected chain metadata: mode=%s root=%s depth=%d", child.Mode, child.RootPlanDigest, child.ChainDepth)
	}
	if child.ParentActionID != "01K1TEST000000000000000006" || len(child.Operations) != len(root.Operations) {
		t.Fatalf("unexpected parent or operation count")
	}
	if child.Operations[0].Kind != core.OpUpdate || child.Operations[1].Kind != core.OpDelete {
		t.Fatalf("chained revert changed root operation direction/order")
	}
	if recomputeDigest(t, child) != child.Digest {
		t.Fatal("child digest is not stable")
	}

	child.Operations[0].Key.Columns[0] = "mutated"
	if root.Operations[0].Key.Columns[0] == "mutated" {
		t.Fatal("chained plan shares mutable row storage with root plan")
	}
}

func TestPlanDigestIsStable(t *testing.T) {
	txn := sampleTxn(t, []core.RowChange{
		mkRow(1, core.OpInsert, core.Row{}, sampleOrders()),
	})
	schema := sampleOrdersSchema()
	d1, err := Build(txn, ModeRevert, depsFor(t, schema))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(d1.Digest, "sha256:") {
		t.Errorf("digest missing prefix: %s", d1.Digest)
	}
	if recomputeDigest(t, d1) != d1.Digest {
		t.Errorf("digest not stable on recompute")
	}
}

func TestPlanDigestExcludesOwnField(t *testing.T) {
	txn := sampleTxn(t, []core.RowChange{
		mkRow(1, core.OpInsert, core.Row{}, sampleOrders()),
	})
	p, err := Build(txn, ModeRevert, depsFor(t, sampleOrdersSchema()))
	if err != nil {
		t.Fatal(err)
	}
	original := p.Digest
	p.Digest = "sha256:aaaa"
	if recomputeDigest(t, p) != original {
		t.Errorf("digest should not depend on its own field")
	}
}

func TestPlanReadWriteRoundTrip(t *testing.T) {
	txn := sampleTxn(t, []core.RowChange{
		mkRow(1, core.OpInsert, core.Row{}, sampleOrders()),
		mkRow(2, core.OpUpdate, core.Row{}, withStatus(sampleOrders(), "cancelled")),
	})
	p, err := Build(txn, ModeRevert, depsFor(t, sampleOrdersSchema()))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := dir + "/plan.json"
	if err := WriteFile(p, path); err != nil {
		t.Fatal(err)
	}
	read, err := ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if read.Digest != p.Digest {
		t.Errorf("digest mismatch on round-trip: %s vs %s", read.Digest, p.Digest)
	}
}

func TestWriteFileLimitedRejectsOversizePlan(t *testing.T) {
	txn := sampleTxn(t, []core.RowChange{mkRow(1, core.OpInsert, core.Row{}, sampleOrders())})
	p, err := Build(txn, ModeRevert, depsFor(t, sampleOrdersSchema()))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteFileLimited(p, t.TempDir()+"/too-large.json", 1); err == nil {
		t.Fatal("expected plan size limit error")
	}
}

func TestUniqueKeySelectionPicksPrimary(t *testing.T) {
	sch := sampleOrdersSchema()
	key, err := selectUniqueKey(sch, sampleOrders())
	if err != nil {
		t.Fatal(err)
	}
	if !key.IsPrimary {
		t.Errorf("expected PRIMARY key, got %s", key.Name)
	}
	if len(key.Columns) == 0 || key.Columns[0] != "id" {
		t.Errorf("expected id column, got %v", key.Columns)
	}
}

func TestUniqueKeySelectionRejectsIncompleteCompositeKey(t *testing.T) {
	sch := core.TableSchema{
		Table: core.TableRef{Schema: "shop", Name: "legacy"},
		Columns: []core.ColumnDef{
			{Name: "tenant_id", Nullable: false},
			{Name: "row_id", Nullable: false},
		},
		UniqueKeys: []core.UniqueKey{{Name: "PRIMARY", IsPrimary: true, Columns: []string{"tenant_id", "row_id"}}},
	}
	row := core.Row{Columns: []string{"tenant_id"}, Values: []core.Value{{Kind: core.KindInteger, Data: core.RawJSON(`1`)}}}
	if _, err := selectUniqueKey(sch, row); err == nil {
		t.Fatal("expected incomplete composite key to be rejected")
	}
}

// --- helpers ---

func sampleTxn(t *testing.T, rows []core.RowChange) *core.Transaction {
	t.Helper()
	return &core.Transaction{
		Ref: core.TransactionRef{
			Backend:             "mysql",
			InstanceID:          "uuid",
			NativeTransactionID: "uuid:1",
		},
		GTID:       "uuid:1",
		Executable: true,
		Rows:       rows,
	}
}

func mkRow(seq int, op core.OperationKind, _ core.Row, body core.Row) core.RowChange {
	return core.RowChange{
		Sequence:  seq,
		Table:     core.TableRef{Schema: "unredo_shop", Name: "orders"},
		Operation: op,
		Key:       body,
		After:     body,
	}
}

func sampleOrders() core.Row {
	return core.Row{
		Columns: []string{"id", "user_id", "status", "amount"},
		Values: []core.Value{
			{Kind: core.KindInteger, Data: jsonRaw(7), Native: ptr("bigint")},
			{Kind: core.KindInteger, Data: jsonRaw(1001), Native: ptr("bigint")},
			{Kind: core.KindText, Data: jsonRaw("paid"), Native: ptr("varchar(16)")},
			{Kind: core.KindDecimal, Encoding: "string", Data: jsonRaw("99.00"), Native: ptr("decimal(12,2)")},
		},
	}
}

func orderWithID(id int) core.Row {
	out := sampleOrders()
	out.Values[0] = core.Value{Kind: core.KindInteger, Data: jsonRaw(id), Native: ptr("bigint")}
	return out
}

func idRow(id int) core.Row {
	return core.Row{Columns: []string{"id"}, Values: []core.Value{{Kind: core.KindInteger, Data: jsonRaw(id), Native: ptr("bigint")}}}
}

func withStatus(in core.Row, s string) core.Row {
	out := core.Row{Columns: append([]string(nil), in.Columns...), Values: append([]core.Value(nil), in.Values...)}
	for i, c := range out.Columns {
		if c == "status" {
			out.Values[i] = core.Value{Kind: core.KindText, Data: jsonRaw(s), Native: ptr("varchar(16)")}
		}
	}
	return out
}

func sampleOrdersSchema() core.TableSchema {
	return core.TableSchema{
		Table:  core.TableRef{Schema: "unredo_shop", Name: "orders"},
		Engine: "InnoDB",
		Columns: []core.ColumnDef{
			{Name: "id", Ordinal: 1, NativeType: "bigint", Nullable: false},
			{Name: "user_id", Ordinal: 2, NativeType: "bigint", Nullable: false},
			{Name: "status", Ordinal: 3, NativeType: "varchar(16)", Nullable: false},
			{Name: "amount", Ordinal: 4, NativeType: "decimal(12,2)", Nullable: false},
		},
		UniqueKeys: []core.UniqueKey{
			{Name: "PRIMARY", IsPrimary: true, Columns: []string{"id"}},
		},
	}
}

func depsFor(t *testing.T, sch core.TableSchema) Deps {
	t.Helper()
	return Deps{
		SchemaFor: func(tr core.TableRef) (core.TableSchema, error) {
			return sch, nil
		},
		FingerprintFor: func(tr core.TableRef) (core.SchemaFingerprint, error) {
			return "sha256:abc", nil
		},
		ToolVersion: "test",
	}
}

func jsonRaw(v interface{}) core.RawJSON {
	b, _ := json.Marshal(v)
	return b
}

func TestCanonicalJSONPreservesArbitraryPrecisionInteger(t *testing.T) {
	input := struct {
		Value core.RawJSON `json:"value"`
	}{Value: core.RawJSON(`18446744073709551615`)}
	raw, err := canonicalJSON(input)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"value":18446744073709551615}` {
		t.Fatalf("canonical JSON lost integer precision: %s", raw)
	}
}

func ptr(s string) *core.NativeType { n := core.NativeType(s); return &n }

func equalRow(a, b core.Row) bool {
	if len(a.Columns) != len(b.Columns) {
		return false
	}
	for i, c := range a.Columns {
		if c != b.Columns[i] {
			return false
		}
		if !a.Values[i].Equal(b.Values[i]) {
			return false
		}
	}
	return true
}

func recomputeDigest(t *testing.T, p *Plan) string {
	t.Helper()
	tmp := *p
	tmp.Digest = ""
	raw, err := canonicalJSON(&tmp)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// keep ports referenced for the future
var _ = ports.PlanOperation{}
