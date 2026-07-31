package mysql

import (
	"testing"

	"github.com/oklog/ulid/v2"
)

func TestULIDBinaryRoundTrip(t *testing.T) {
	want := ulid.Make().String()
	b, err := ulidBytes(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ulidString(b)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("round trip=%s, want %s", got, want)
	}
}
