package planner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// WriteFile serialises a Plan to disk with deterministic formatting and
// restrictive file permissions. The on-disk file is what `plan check`
// and `plan apply` consume; it must be byte-stable.
func WriteFile(p *Plan, path string) error {
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
	var v interface{}
	if err := json.Unmarshal(in, &v); err != nil {
		return nil, err
	}
	return marshalPretty(v, "", 0)
}

func marshalPretty(v interface{}, indent string, depth int) ([]byte, error) {
	pad := indent + repeat("  ", depth)
	inner := indent + repeat("  ", depth+1)
	switch x := v.(type) {
	case map[string]interface{}:
		if len(x) == 0 {
			return []byte("{}"), nil
		}
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		// sort.Strings is fine here because we're only using it for
		// human presentation, not the digest.
		sortStrings(keys)
		out := []byte("{\n")
		for i, k := range keys {
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			val, err := marshalPretty(x[k], indent, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, []byte(inner)...)
			out = append(out, kb...)
			out = append(out, ':')
			// Always pad value with a space for readability.
			out = append(out, ' ')
			out = append(out, val...)
			if i < len(keys)-1 {
				out = append(out, ',')
			}
			out = append(out, '\n')
		}
		out = append(out, []byte(pad)...)
		out = append(out, '}')
		return out, nil
	case []interface{}:
		if len(x) == 0 {
			return []byte("[]"), nil
		}
		out := []byte("[\n")
		for i, e := range x {
			val, err := marshalPretty(e, indent, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, []byte(inner)...)
			out = append(out, val...)
			if i < len(x)-1 {
				out = append(out, ',')
			}
			out = append(out, '\n')
		}
		out = append(out, []byte(pad)...)
		out = append(out, ']')
		return out, nil
	default:
		return json.Marshal(v)
	}
}

func repeat(s string, n int) string {
	if n <= 0 {
		return ""
	}
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

func sortStrings(a []string) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}
