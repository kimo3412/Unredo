package ports

import "encoding/json"

// Cursor is the on-disk representation of a backend-specific scan position.
// It is opaque to the core; only the originating backend decodes it.
type Cursor json.RawMessage

// MarshalJSON returns the raw bytes.
func (c Cursor) MarshalJSON() ([]byte, error) {
	if c == nil {
		return []byte("null"), nil
	}
	return c, nil
}

// UnmarshalJSON stores the bytes.
func (c *Cursor) UnmarshalJSON(b []byte) error {
	if b == nil {
		*c = nil
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	*c = out
	return nil
}
