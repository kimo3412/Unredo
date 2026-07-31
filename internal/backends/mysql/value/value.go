// Package value converts between MySQL column values and core.Value.
// Only types called out in DESIGN.md §12 are supported in M0; everything
// else is returned as KindUnknown so the planner can fail closed.
package value

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/girimi/unredo/internal/core"
)

// ColumnType is the MySQL column metadata we care about.
type ColumnType struct {
	Database   string
	Table      string
	Name       string
	ColumnType string // e.g. "decimal(12,2)", "bigint unsigned"
	Nullable   string // "YES" / "NO"
	Charset    string
}

// Decode turns a raw MySQL driver value into a core.Value.
// raw is whatever the driver returns for the column; for binlog events
// this is usually []byte for numerics and strings.
func Decode(ct ColumnType, raw interface{}) (core.Value, error) {
	if raw == nil {
		return core.Value{Kind: kindForType(ct.ColumnType), Null: true, Native: ptrNative(ct.ColumnType)}, nil
	}
	switch k := kindForType(ct.ColumnType); k {
	case core.KindInteger:
		return decodeInteger(ct, raw)
	case core.KindDecimal:
		return decodeDecimal(ct, raw)
	case core.KindFloat:
		return decodeFloat(ct, raw)
	case core.KindText:
		return decodeText(ct, raw)
	case core.KindBinary:
		return decodeBinary(ct, raw)
	case core.KindDate, core.KindTime, core.KindDateTime:
		return decodeTemporal(ct, raw, k)
	case core.KindJSON:
		return decodeJSON(ct, raw)
	case core.KindBit:
		return decodeBit(ct, raw)
	case core.KindEnum, core.KindSet:
		return decodeStringish(ct, raw, k)
	default:
		return core.Value{}, fmt.Errorf("mysql: unsupported type %q for column %s.%s.%s", ct.ColumnType, ct.Database, ct.Table, ct.Name)
	}
}

func kindForType(mysqlType string) core.ValueKind {
	t := strings.ToLower(mysqlType)
	switch {
	case strings.HasPrefix(t, "tinyint"), strings.HasPrefix(t, "smallint"),
		strings.HasPrefix(t, "mediumint"), strings.HasPrefix(t, "int"),
		strings.HasPrefix(t, "bigint"), strings.HasPrefix(t, "year"):
		return core.KindInteger
	case strings.HasPrefix(t, "decimal"), strings.HasPrefix(t, "numeric"):
		return core.KindDecimal
	case strings.HasPrefix(t, "float"), strings.HasPrefix(t, "double"), strings.HasPrefix(t, "real"):
		return core.KindFloat
	case strings.HasPrefix(t, "char"), strings.HasPrefix(t, "varchar"),
		strings.HasPrefix(t, "tinytext"), strings.HasPrefix(t, "text"),
		strings.HasPrefix(t, "mediumtext"), strings.HasPrefix(t, "longtext"):
		return core.KindText
	case strings.HasPrefix(t, "binary"), strings.HasPrefix(t, "varbinary"),
		strings.HasPrefix(t, "tinyblob"), strings.HasPrefix(t, "blob"),
		strings.HasPrefix(t, "mediumblob"), strings.HasPrefix(t, "longblob"):
		return core.KindBinary
	case strings.HasPrefix(t, "date"):
		return core.KindDate
	case strings.HasPrefix(t, "time"):
		return core.KindTime
	case strings.HasPrefix(t, "datetime"), strings.HasPrefix(t, "timestamp"):
		return core.KindDateTime
	case strings.HasPrefix(t, "json"):
		return core.KindJSON
	case strings.HasPrefix(t, "bit"):
		return core.KindBit
	case strings.HasPrefix(t, "enum"):
		return core.KindEnum
	case strings.HasPrefix(t, "set"):
		return core.KindSet
	}
	return core.KindUnknown
}

func ptrNative(s string) *core.NativeType { n := core.NativeType(s); return &n }

func decodeInteger(ct ColumnType, raw interface{}) (core.Value, error) {
	// The driver may hand us int64 (preferred) or the textual form as
	// []byte. We always want a canonical JSON-number Data, so we
	// normalise to a string and then strip the surrounding quotes.
	// json.Marshal on a string would base64-encode the bytes, so we
	// never go through that path for integers.
	var s string
	switch x := raw.(type) {
	case []byte:
		s = string(x)
	case string:
		s = x
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		out, _ := json.Marshal(x)
		return core.Value{
			Kind:     core.KindInteger,
			Encoding: "json",
			Data:     core.RawJSON(out),
			Native:   ptrNative(ct.ColumnType),
		}, nil
	default:
		out, err := json.Marshal(x)
		if err != nil {
			return core.Value{}, fmt.Errorf("mysql: integer column %q: %w", ct.Name, err)
		}
		return core.Value{
			Kind:     core.KindInteger,
			Encoding: "json",
			Data:     core.RawJSON(out),
			Native:   ptrNative(ct.ColumnType),
		}, nil
	}
	clean, err := parseIntegerLiteral(s)
	if err != nil {
		return core.Value{}, fmt.Errorf("mysql: integer column %q: %w", ct.Name, err)
	}
	return core.Value{
		Kind:     core.KindInteger,
		Encoding: "json",
		Data:     core.RawJSON(clean),
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func parseIntegerLiteral(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("empty integer")
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return "", fmt.Errorf("no digits")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return "", fmt.Errorf("non-digit %q", c)
		}
	}
	if neg {
		return "-" + s, nil
	}
	return s, nil
}

func decodeDecimal(ct ColumnType, raw interface{}) (core.Value, error) {
	// MySQL returns DECIMAL as string. Preserve as JSON string; do not
	// round-trip through float per DESIGN.md §12.
	s, ok := asString(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: decimal column %q: expected string, got %T", ct.Name, raw)
	}
	s = strings.TrimSpace(s)
	// Validate that it parses as a decimal so we fail closed on garbage.
	if _, _, err := parseDecimalParts(s); err != nil {
		return core.Value{}, fmt.Errorf("mysql: decimal column %q: %w", ct.Name, err)
	}
	data, _ := json.Marshal(s)
	return core.Value{
		Kind:     core.KindDecimal,
		Encoding: "string",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func parseDecimalParts(s string) (string, int, error) {
	if s == "" {
		return "", 0, fmt.Errorf("empty decimal")
	}
	neg := false
	if s[0] == '-' || s[0] == '+' {
		neg = s[0] == '-'
		s = s[1:]
	}
	whole, frac, hasDot := strings.Cut(s, ".")
	intPart := whole + frac
	if intPart == "" {
		return "", 0, fmt.Errorf("no digits")
	}
	for _, c := range intPart {
		if c < '0' || c > '9' {
			return "", 0, fmt.Errorf("non-digit %q", c)
		}
	}
	out := intPart
	if hasDot {
		out = whole + "." + frac
	}
	if neg {
		out = "-" + out
	}
	scale := 0
	if hasDot {
		scale = len(frac)
	}
	return out, scale, nil
}

func decodeFloat(ct ColumnType, raw interface{}) (core.Value, error) {
	s, ok := asString(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: float column %q: expected string, got %T", ct.Name, raw)
	}
	// Preserve the textual form so that binlog image equals SQL image
	// without binary round-trip drift.
	data, _ := json.Marshal(strings.TrimSpace(s))
	return core.Value{
		Kind:     core.KindFloat,
		Encoding: "string",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func decodeText(ct ColumnType, raw interface{}) (core.Value, error) {
	s, ok := asString(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: text column %q: expected string, got %T", ct.Name, raw)
	}
	data, _ := json.Marshal(s)
	return core.Value{
		Kind:     core.KindText,
		Encoding: "utf8",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func decodeBinary(ct ColumnType, raw interface{}) (core.Value, error) {
	b, ok := asBytes(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: binary column %q: expected []byte, got %T", ct.Name, raw)
	}
	data, _ := json.Marshal(base64.StdEncoding.EncodeToString(b))
	return core.Value{
		Kind:     core.KindBinary,
		Encoding: "base64",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func decodeTemporal(ct ColumnType, raw interface{}, kind core.ValueKind) (core.Value, error) {
	s, ok := asString(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: temporal column %q: expected string, got %T", ct.Name, raw)
	}
	s = strings.TrimSpace(s)
	data, _ := json.Marshal(s)
	return core.Value{
		Kind:     kind,
		Encoding: "string",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func decodeJSON(ct ColumnType, raw interface{}) (core.Value, error) {
	s, ok := asString(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: json column %q: expected string, got %T", ct.Name, raw)
	}
	// MySQL stores JSON as a normalised text. Keep the normalised form so
	// that the image we see matches what the server reports in SQL.
	compacted := compactJSON(s)
	if !json.Valid([]byte(compacted)) {
		return core.Value{}, fmt.Errorf("mysql: json column %q: not valid JSON", ct.Name)
	}
	data, _ := json.Marshal(compacted)
	return core.Value{
		Kind:     core.KindJSON,
		Encoding: "json-canonical",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func compactJSON(s string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return s
	}
	out, err := json.Marshal(v)
	if err != nil {
		return s
	}
	return string(out)
}

func decodeBit(ct ColumnType, raw interface{}) (core.Value, error) {
	// BIT comes back as []byte from go-sql-driver. Use big.Int so that
	// values wider than 64 bits round-trip exactly.
	b, ok := asBytes(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: bit column %q: expected []byte, got %T", ct.Name, raw)
	}
	z := new(big.Int).SetBytes(b)
	data, _ := json.Marshal(z.String())
	return core.Value{
		Kind:     core.KindBit,
		Encoding: "bigint",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func decodeStringish(ct ColumnType, raw interface{}, kind core.ValueKind) (core.Value, error) {
	s, ok := asString(raw)
	if !ok {
		return core.Value{}, fmt.Errorf("mysql: %s column %q: expected string, got %T", kind, ct.Name, raw)
	}
	data, _ := json.Marshal(s)
	return core.Value{
		Kind:     kind,
		Encoding: "string",
		Data:     data,
		Native:   ptrNative(ct.ColumnType),
	}, nil
}

func marshalJSON(v interface{}) (core.RawJSON, error) {
	out, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return core.RawJSON(out), nil
}

func asString(v interface{}) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case []byte:
		return string(x), true
	}
	return "", false
}

func asBytes(v interface{}) ([]byte, bool) {
	switch x := v.(type) {
	case []byte:
		return x, true
	case string:
		return []byte(x), true
	}
	return nil, false
}

// DecodeRow is a convenience for SELECT-style reads where the column
// order matches the column list passed in. raw is the []interface{}
// returned by database/sql; ct is the matching column type metadata.
func DecodeRow(cols []ColumnType, raw []interface{}) (core.Row, error) {
	if len(cols) != len(raw) {
		return core.Row{}, fmt.Errorf("mysql: row has %d values, expected %d columns", len(raw), len(cols))
	}
	out := core.Row{
		Columns: make([]string, 0, len(cols)),
		Values:  make([]core.Value, 0, len(cols)),
	}
	for i, v := range raw {
		cv, err := Decode(cols[i], v)
		if err != nil {
			return core.Row{}, err
		}
		out.Columns = append(out.Columns, cols[i].Name)
		out.Values = append(out.Values, cv)
	}
	return out, nil
}
