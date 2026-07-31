package mysql

import (
	"encoding/binary"
	"fmt"
	"time"

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
	out := make([]byte, 16)
	binary.BigEndian.PutUint64(out, id.Time())
	entropy := id.Entropy()
	copy(out[8:], entropy)
	return out, nil
}

// newULID returns a fresh ULID as a string. Used when the CLI needs
// to mint an action id for the apply path.
func newULID() string {
	return ulid.Make().String()
}

// planIDFromDigest is a stable derivation of plan_id from the digest
// string. M2 doesn't actually need it (the planner already writes a
// ULID plan_id into the plan file), but the function lives here for
// callers that want to synthesise a deterministic id from a digest.
func planIDFromDigest(digest string) string {
	t := time.Unix(0, 0)
	_ = t
	// Stub: derive a ULID from the digest prefix. Real callers should
	// use the plan_id from the file; this exists so the function
	// signature is stable while we iterate.
	return ""
}

// silence unused
var _ = fmt.Sprintf
