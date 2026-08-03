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

func TestDecodeFloatAcceptsBinlogNumericValues(t *testing.T) {
	tests := []struct {
		native string
		raw    interface{}
		want   string
	}{
		{native: "float", raw: float32(1.25), want: `"1.25"`},
		{native: "double", raw: float64(-12345.6789012345), want: `"-12345.6789012345"`},
	}
	for _, test := range tests {
		value, err := Decode(ColumnType{Name: "f", ColumnType: test.native}, test.raw)
		if err != nil {
			t.Fatal(err)
		}
		if string(value.Data) != test.want {
			t.Fatalf("Decode(%s)=%s, want %s", test.native, value.Data, test.want)
		}
	}
}

func TestDecodeBit64PreservesHighBitFromSignedBinlogContainer(t *testing.T) {
	value, err := Decode(ColumnType{Name: "bits", ColumnType: "bit(64)"}, int64(-81985529216486896))
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != `"18364758544493064720"` {
		t.Fatalf("unexpected BIT(64) value %s", value.Data)
	}
}

func TestDecodeBigIntUnsignedPreservesHighBitFromSignedBinlogContainer(t *testing.T) {
	value, err := Decode(ColumnType{Name: "id", ColumnType: "bigint unsigned"}, int64(-1))
	if err != nil {
		t.Fatal(err)
	}
	if string(value.Data) != `18446744073709551615` {
		t.Fatalf("unexpected BIGINT UNSIGNED value %s", value.Data)
	}
}

func TestDecodeEnumAndSetFromBinlogIndexes(t *testing.T) {
	enumValue, err := Decode(ColumnType{Name: "choice", ColumnType: "enum('alpha','beta','雪')"}, int64(3))
	if err != nil {
		t.Fatal(err)
	}
	if string(enumValue.Data) != `"雪"` {
		t.Fatalf("unexpected enum value %s", enumValue.Data)
	}
	setValue, err := Decode(ColumnType{Name: "flags", ColumnType: "set('red','green','蓝')"}, int64(5))
	if err != nil {
		t.Fatal(err)
	}
	if string(setValue.Data) != `"red,蓝"` {
		t.Fatalf("unexpected set value %s", setValue.Data)
	}
}
