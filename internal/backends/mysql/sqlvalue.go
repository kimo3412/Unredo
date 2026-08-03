package mysql

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/girimi/unredo/internal/core"
)

// driverValue converts the canonical plan representation back to a value
// accepted by go-sql-driver/mysql. JSON string quoting is an on-disk detail
// and must never be written into a user column.
func driverValue(v core.Value) (interface{}, error) {
	if v.Null {
		return nil, nil
	}
	if err := v.Validate(); err != nil {
		return nil, err
	}
	switch v.Kind {
	case core.KindInteger:
		// Preserve arbitrary precision; MySQL performs the native conversion.
		return string(v.Data), nil
	case core.KindDecimal, core.KindFloat, core.KindText, core.KindEnum,
		core.KindSet, core.KindDate, core.KindTime, core.KindDateTime,
		core.KindJSON:
		var s string
		if err := json.Unmarshal(v.Data, &s); err != nil {
			return nil, fmt.Errorf("decode %s value: %w", v.Kind, err)
		}
		return s, nil
	case core.KindBit:
		var s string
		if err := json.Unmarshal(v.Data, &s); err != nil {
			return nil, fmt.Errorf("decode bit value: %w", err)
		}
		n := new(big.Int)
		if _, ok := n.SetString(s, 10); !ok || n.Sign() < 0 {
			return nil, fmt.Errorf("decode bit value: invalid unsigned integer %q", s)
		}
		width := n.BitLen()
		if v.Native != nil {
			native := strings.ToLower(strings.TrimSpace(string(*v.Native)))
			if strings.HasPrefix(native, "bit(") && strings.HasSuffix(native, ")") {
				parsed, err := strconv.Atoi(native[4 : len(native)-1])
				if err != nil || parsed < 1 || parsed > 64 {
					return nil, fmt.Errorf("decode bit value: invalid native type %q", native)
				}
				width = parsed
			}
		}
		if n.BitLen() > width {
			return nil, fmt.Errorf("decode bit value: %q exceeds BIT(%d)", s, width)
		}
		byteLen := (width + 7) / 8
		if byteLen == 0 {
			byteLen = 1
		}
		out := make([]byte, byteLen)
		n.FillBytes(out)
		return out, nil
	case core.KindBinary:
		var encoded string
		if err := json.Unmarshal(v.Data, &encoded); err != nil {
			return nil, fmt.Errorf("decode binary value: %w", err)
		}
		decoded, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode binary base64: %w", err)
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("unsupported MySQL value kind %q", v.Kind)
	}
}

func quoteIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// parameterExpression gives MySQL an explicit native type when a plain
// string parameter would otherwise have different comparison or assignment
// semantics.
func parameterExpression(v core.Value) string {
	switch v.Kind {
	case core.KindJSON:
		return "CAST(? AS JSON)"
	default:
		return "?"
	}
}

// buildPredicate renders exact SQL predicates with correct NULL semantics.
// Duplicate columns in later rows are ignored, so a key can be combined with
// a full expect image without binding the key twice.
func buildPredicate(rows ...core.Row) (string, []interface{}, error) {
	parts := make([]string, 0)
	args := make([]interface{}, 0)
	seen := make(map[string]struct{})
	for _, row := range rows {
		if len(row.Columns) != len(row.Values) {
			return "", nil, fmt.Errorf("row has %d columns but %d values", len(row.Columns), len(row.Values))
		}
		for i, col := range row.Columns {
			if _, ok := seen[col]; ok {
				continue
			}
			seen[col] = struct{}{}
			v := row.Values[i]
			if v.Null {
				parts = append(parts, quoteIdent(col)+" IS NULL")
				continue
			}
			arg, err := driverValue(v)
			if err != nil {
				return "", nil, fmt.Errorf("column %q: %w", col, err)
			}
			columnExpression := quoteIdent(col)
			if v.Kind == core.KindBit {
				columnExpression = "CAST(" + columnExpression + " AS BINARY)"
			}
			predicate := columnExpression + " = " + parameterExpression(v)
			parts = append(parts, predicate)
			args = append(args, arg)
		}
	}
	if len(parts) == 0 {
		return "", nil, fmt.Errorf("empty row predicate")
	}
	return strings.Join(parts, " AND "), args, nil
}
