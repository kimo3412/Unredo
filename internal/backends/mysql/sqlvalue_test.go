package mysql

import (
	"bytes"
	"testing"

	"github.com/girimi/unredo/internal/core"
)

func TestDriverValueDecodesJSONStringAndBinary(t *testing.T) {
	text, err := driverValue(core.Value{Kind: core.KindText, Data: core.RawJSON(`"pending"`), Encoding: "utf8"})
	if err != nil || text != "pending" {
		t.Fatalf("text value = %#v, err=%v; want pending", text, err)
	}
	dec, err := driverValue(core.Value{Kind: core.KindDecimal, Data: core.RawJSON(`"16.16"`), Encoding: "string"})
	if err != nil || dec != "16.16" {
		t.Fatalf("decimal value = %#v, err=%v; want 16.16", dec, err)
	}
	binaryValue, err := driverValue(core.Value{Kind: core.KindBinary, Data: core.RawJSON(`"AP8="`), Encoding: "base64"})
	if err != nil || !bytes.Equal(binaryValue.([]byte), []byte{0, 255}) {
		t.Fatalf("binary value = %#v, err=%v; want 00ff", binaryValue, err)
	}
}

func TestBuildPredicateUsesIsNull(t *testing.T) {
	row := core.Row{
		Columns: []string{"id", "note"},
		Values: []core.Value{
			{Kind: core.KindInteger, Data: core.RawJSON(`1`)},
			{Kind: core.KindText, Null: true},
		},
	}
	where, args, err := buildPredicate(row)
	if err != nil {
		t.Fatal(err)
	}
	if where != "`id` = ? AND `note` IS NULL" {
		t.Fatalf("where = %q", where)
	}
	if len(args) != 1 || args[0] != "1" {
		t.Fatalf("args = %#v", args)
	}
}

func TestBuildPredicateCastsJSONParameter(t *testing.T) {
	row := core.Row{
		Columns: []string{"id", "doc"},
		Values: []core.Value{
			{Kind: core.KindInteger, Data: core.RawJSON(`1`)},
			{Kind: core.KindJSON, Data: core.RawJSON(`"{\"a\":1}"`), Encoding: "json"},
		},
	}
	where, args, err := buildPredicate(row)
	if err != nil {
		t.Fatal(err)
	}
	if where != "`id` = ? AND `doc` = CAST(? AS JSON)" {
		t.Fatalf("where = %q", where)
	}
	if len(args) != 2 || args[0] != "1" || args[1] != `{"a":1}` {
		t.Fatalf("args = %#v", args)
	}
}

func TestParameterExpressionUsesNativeMySQLTypes(t *testing.T) {
	tests := []struct {
		kind core.ValueKind
		want string
	}{
		{core.KindJSON, "CAST(? AS JSON)"},
		{core.KindBit, "?"},
		{core.KindText, "?"},
	}
	for _, tt := range tests {
		if got := parameterExpression(core.Value{Kind: tt.kind}); got != tt.want {
			t.Errorf("parameterExpression(%s) = %q, want %q", tt.kind, got, tt.want)
		}
	}
}

func TestDriverValueEncodesBitAsFixedWidthBinary(t *testing.T) {
	native := core.NativeType("bit(64)")
	got, err := driverValue(core.Value{
		Kind: core.KindBit, Data: core.RawJSON(`"18364758544493064720"`),
		Encoding: "bigint", Native: &native,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0xfe, 0xdc, 0xba, 0x98, 0x76, 0x54, 0x32, 0x10}
	if !bytes.Equal(got.([]byte), want) {
		t.Fatalf("bit bytes = %x, want %x", got, want)
	}
}

func TestBuildPredicateComparesBitAsBinary(t *testing.T) {
	native := core.NativeType("bit(64)")
	row := core.Row{Columns: []string{"bits"}, Values: []core.Value{{
		Kind: core.KindBit, Data: core.RawJSON(`"1"`), Encoding: "bigint", Native: &native,
	}}}
	where, args, err := buildPredicate(row)
	if err != nil {
		t.Fatal(err)
	}
	if where != "CAST(`bits` AS BINARY) = ?" {
		t.Fatalf("where = %q", where)
	}
	if len(args) != 1 || !bytes.Equal(args[0].([]byte), []byte{0, 0, 0, 0, 0, 0, 0, 1}) {
		t.Fatalf("args = %#v", args)
	}
}
