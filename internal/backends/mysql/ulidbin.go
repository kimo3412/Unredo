package mysql

import (
	"fmt"

	"github.com/oklog/ulid/v2"
)

// ulidBytes converts a ULID string into the 16-byte binary form the
// action_markers schema expects for action_id and plan_id. We keep
// the conversion in this file so the rest of the package only has to
// hand around []byte.
func ulidBytes(s string) ([]byte, error) {
	id, err := ulid.Parse(s)
	if err != nil {
		return nil, err
	}
	out := make([]byte, len(id))
	copy(out, id[:])
	return out, nil
}

func ulidString(b []byte) (string, error) {
	var id ulid.ULID
	if len(b) != len(id) {
		return "", fmt.Errorf("ULID binary length is %d, want %d", len(b), len(id))
	}
	copy(id[:], b)
	return id.String(), nil
}

// newULID returns a fresh ULID as a string. Used when the CLI needs
// to mint an action id for the apply path.
func newULID() string {
	return ulid.Make().String()
}
