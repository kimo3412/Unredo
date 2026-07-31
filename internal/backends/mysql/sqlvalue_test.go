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
