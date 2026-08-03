package planner

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile serialises a Plan to disk with deterministic formatting and
// restrictive file permissions. The on-disk file is what `plan check`
// and `plan apply` consume; it must be byte-stable.
func WriteFile(p *Plan, path string) error {
	return WriteFileLimited(p, path, 0)
}

// WriteFileLimited is WriteFile with a hard encoded-size ceiling. A zero
// limit disables the ceiling. The check happens before creating the file.
func WriteFileLimited(p *Plan, path string, maxBytes int64) error {
	if p == nil {
		return fmt.Errorf("planner: nil plan")
	}
	// We must emit the canonical form so that the digest is recoverable
	// from the on-disk bytes. MarshalIndent would re-order keys, so we
	// build the canonical form by hand.
	raw, err := canonicalJSON(p)
	if err != nil {
		return fmt.Errorf("planner: marshal: %w", err)
	}
	// Pretty-print lightly: keep newlines between top-level keys for
	// human review while preserving byte stability through the
	// canonical form. We do this by re-serialising from the parsed
	// value with a stable indent helper.
	pretty, err := stablePretty(raw)
	if err != nil {
		return fmt.Errorf("planner: pretty: %w", err)
	}
	if maxBytes > 0 && int64(len(pretty)+1) > maxBytes {
		return fmt.Errorf("planner: encoded plan is %d bytes, exceeds limit %d", len(pretty)+1, maxBytes)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("planner: mkdir: %w", err)
	}
	if err := os.WriteFile(path, append(pretty, '\n'), 0o600); err != nil {
		return fmt.Errorf("planner: write: %w", err)
	}
	return nil
}

// ReadFile loads a plan and verifies its digest. It refuses to return
// a plan that doesn't match its declared digest, because tampering or
// partial writes would silently change the executor's behaviour.
func ReadFile(path string) (*Plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("planner: read: %w", err)
	}
	var p Plan
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, fmt.Errorf("planner: parse: %w", err)
	}
	if p.FormatVersion != FormatVersion {
		return nil, fmt.Errorf("planner: unsupported format_version %d (want %d)", p.FormatVersion, FormatVersion)
	}
	want := computeDigest(&p)
	if want != p.Digest {
		return nil, fmt.Errorf("planner: digest mismatch (declared %s, computed %s)", p.Digest, want)
	}
	return &p, nil
}

// ShortDigest returns the first 8 hex characters of a digest, dropping
// the leading "sha256:" prefix. The CLI's --confirm-sha is matched
// against this short form per DESIGN.md §6.7.
func ShortDigest(d string) string {
	const prefix = "sha256:"
	if len(d) > len(prefix) && d[:len(prefix)] == prefix {
		hex := d[len(prefix):]
		if len(hex) > 8 {
			return hex[:8]
		}
		return hex
	}
	if len(d) > 8 {
		return d[:8]
	}
	return d
}

// stablePretty re-indents a compact canonical-JSON byte stream so that
// humans can read the file while leaving the parse structure untouched.
// Two equivalent plans still hash to the same digest because the parser
// re-discards whitespace.
func stablePretty(in []byte) ([]byte, error) {
	var out bytes.Buffer
	out.Grow(len(in) + len(in)/4)
	if err := json.Indent(&out, in, "", "  "); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
