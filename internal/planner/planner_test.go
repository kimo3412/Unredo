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
