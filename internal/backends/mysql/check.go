// Package mysql — check.go implements executor.Reader for MySQL.
//
// The read path is intentionally simple: a single SELECT keyed on the
// plan's key columns, projected to all known columns so the row can
// be compared against the expect image. The plan key columns are
// always a unique key by design (planner invariant), so we expect at
// most one row.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	_ "github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/backends/mysql/schema"
	mysqlvalue "github.com/girimi/unredo/internal/backends/mysql/value"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/executor"
)

// CheckReader is the executor.Reader that targets a MySQL instance.
type CheckReader struct {
	targetDSN  string
	instanceID string
	inspector  *schema.Inspector
}

// NewCheckReader wires a reader to the target DSN. The instance id is
// used to detect SOURCE_MISMATCH at check time.
func NewCheckReader(targetDSN, instanceID string) *CheckReader {
	return &CheckReader{
		targetDSN:  targetDSN,
		instanceID: instanceID,
		inspector:  schema.NewInspector(targetDSN),
	}
}

// NewCheckReaderFromBackend pulls the target DSN and instance id out
// of a Backend, avoiding a redundant ping in the CLI.
func NewCheckReaderFromBackend(b *Backend) *CheckReader {
	return &CheckReader{
		targetDSN:  b.targetDSN,
		instanceID: b.instanceID,
		inspector:  schema.NewInspector(b.targetDSN),
	}
}

// TargetInstanceID implements executor.Reader.
func (r *CheckReader) TargetInstanceID() string { return r.instanceID }

// Fingerprint implements executor.Reader.
func (r *CheckReader) Fingerprint(ctx context.Context, t core.TableRef) (core.SchemaFingerprint, error) {
	return r.inspector.Fingerprint(ctx, t)
}

// ReadByKey implements executor.Reader.
func (r *CheckReader) ReadByKey(ctx context.Context, t core.TableRef, keyColumns []string, key core.Row) (executor.Row, bool, error) {
	if len(keyColumns) == 0 {
		return executor.Row{}, false, fmt.Errorf("mysql: empty key columns for %s", t)
	}
	sch, err := r.inspector.InspectTable(ctx, t)
	if err != nil {
		return executor.Row{}, false, err
	}
	cols := columnsByName(sch)
	if len(cols) == 0 {
		return executor.Row{}, false, fmt.Errorf("mysql: %s has no columns", t)
	}

	// Build SELECT col1, col2, ... FROM schema.table WHERE k1=? AND k2=?
	colNames := make([]string, 0, len(cols))
	colTypes := make([]mysqlvalue.ColumnType, 0, len(cols))
	for _, c := range cols {
		colNames = append(colNames, "`"+c.Name+"`")
		colTypes = append(colTypes, mysqlvalue.ColumnType{
			Database:   t.Schema,
			Table:      t.Name,
			Name:       c.Name,
			ColumnType: string(c.NativeType),
			Nullable:   yesNo(c.Nullable),
			Charset:    c.Charset,
		})
	}
	where := make([]string, 0, len(keyColumns))
	args := make([]interface{}, 0, len(keyColumns))
	for _, kc := range keyColumns {
		v, ok := key.Get(kc)
		if !ok {
			return executor.Row{}, false, fmt.Errorf("mysql: key column %q missing from key image", kc)
		}
		where = append(where, "`"+kc+"` = ?")
		args = append(args, valueToDriverArg(v))
	}
	query := "SELECT " + strings.Join(colNames, ", ") +
		" FROM `" + t.Schema + "`.`" + t.Name + "`" +
		" WHERE " + strings.Join(where, " AND ") +
		" LIMIT 1"

	db, err := sql.Open("mysql", r.targetDSN)
	if err != nil {
		return executor.Row{}, false, fmt.Errorf("mysql: open: %w", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return executor.Row{}, false, fmt.Errorf("mysql: query: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return executor.Row{}, false, err
		}
		return executor.Row{}, false, nil
	}
	// Use sql.RawBytes so the driver hands us the underlying bytes for
	// text/binary columns. We unwrap to either []byte or string before
	// handing each value to the type decoder.
	raw := make([]sql.RawBytes, len(cols))
	scans := make([]interface{}, len(cols))
	for i := range scans {
		scans[i] = &raw[i]
	}
	if err := rows.Scan(scans...); err != nil {
		return executor.Row{}, false, fmt.Errorf("mysql: scan: %w", err)
	}
	values := make([]interface{}, len(cols))
	for i, b := range raw {
		values[i] = []byte(b)
	}
	coreRow, err := mysqlvalue.DecodeRow(colTypes, values)
	if err != nil {
		return executor.Row{}, false, err
	}
	return rowToExecutor(coreRow), true, nil
}

func rowToExecutor(r core.Row) executor.Row {
	return executor.Row{Columns: r.Columns, Values: r.Values}
}

func columnsByName(sch core.TableSchema) []core.ColumnDef {
	// We need ordered output, but the schema already gives us columns
	// in ordinal order from the inspector.
	return sch.Columns
}

// valueToDriverArg converts a core.Value into a Go value the MySQL
// driver will accept as a query argument.
func valueToDriverArg(v core.Value) interface{} {
	if v.Null {
		return nil
	}
	switch v.Kind {
	case core.KindInteger, core.KindDecimal, core.KindFloat:
		// Driver accepts string for DECIMAL; for integer we pass raw
		// JSON number.
		return []byte(v.Data)
	case core.KindText, core.KindEnum, core.KindSet:
		return string(v.Data)
	case core.KindBinary, core.KindBit:
		// Binary is base64; we need the raw bytes. For BIT we keep
		// the decimal form because the driver doesn't accept raw
		// bytes for bit columns reliably.
		if v.Kind == core.KindBinary {
			return unquoteJSONString(string(v.Data))
		}
		return string(v.Data)
	case core.KindDate, core.KindTime, core.KindDateTime:
		return string(v.Data)
	case core.KindJSON:
		return unquoteJSONString(string(v.Data))
	default:
		return string(v.Data)
	}
}

func unquoteJSONString(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		out := s[1 : len(s)-1]
		out = strings.ReplaceAll(out, `\"`, `"`)
		out = strings.ReplaceAll(out, `\\`, `\`)
		return out
	}
	return s
}

// yesNo mirrors value.yesNo without an import cycle. Kept here so the
// value package owns type decoding and the executor owns SQL plumbing.
func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}
