package core

import (
	"fmt"
	"testing"
)

func TestValueEqual(t *testing.T) {
	intVal := func(n int) Value {
		return Value{Kind: KindInteger, Data: RawJSON([]byte(fmt.Sprintf("%d", n)))}
	}
	decVal := func(s string) Value {
		return Value{Kind: KindDecimal, Encoding: "string", Data: RawJSON(quoteJSON(t, s))}
	}
	textVal := func(s string) Value {
		return Value{Kind: KindText, Data: RawJSON(quoteJSON(t, s))}
	}

	cases := []struct {
		name string
		a, b Value
		want bool
	}{
		{"same int", intVal(1), intVal(1), true},
		{"diff int", intVal(1), intVal(99), false},
		{"diff kind", intVal(1), textVal("42"), false},
		{"null vs value", intVal(1), Value{Kind: KindInteger, Null: true}, false},
		{"null vs null", Value{Kind: KindInteger, Null: true}, Value{Kind: KindInteger, Null: true}, true},
		{"decimal text", decVal("199.00"), decVal("199.00"), true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.a.Equal(c.b); got != c.want {
				t.Fatalf("Equal(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
			}
		})
	}
}

func TestValueValidate(t *testing.T) {
	v := Value{Kind: KindInteger, Data: RawJSON([]byte("1"))}
	if err := v.Validate(); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
	bad := Value{Kind: KindDecimal, Data: RawJSON([]byte("not-json"))}
	if err := bad.Validate(); err == nil {
		t.Fatalf("expected error for invalid JSON")
	}
}

func TestOperationKindValid(t *testing.T) {
	for _, ok := range []OperationKind{OpInsert, OpUpdate, OpDelete} {
		if !ok.Valid() {
			t.Fatalf("%q should be valid", ok)
		}
	}
	if OperationKind("nope").Valid() {
		t.Fatalf("nope should be invalid")
	}
}

func TestCapabilitiesAll(t *testing.T) {
	c := BackendCapabilities{
		FullBeforeImage:       true,
		FullAfterImage:        true,
		StableTransactionID:   true,
		TransactionBoundaries: true,
		AtomicActionMarker:    true,
		SchemaAtEventTime:     true,
		SupportsReapply:       true,
	}
	if !c.All() {
		t.Fatal("all-true caps should report All")
	}
	c.SupportsReapply = false
	if c.All() {
		t.Fatal("missing flag should drop All")
	}
}

func quoteJSON(t *testing.T, s string) []byte {
	t.Helper()
	out, err := jsonMarshalString(s)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return out
}
