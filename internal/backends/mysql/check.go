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
	instanceID string
	inspector  *schema.Inspector
	db         *sql.DB
	dbErr      error
	schemas    map[core.TableRef]core.TableSchema
}

// NewCheckReader wires a reader to the target DSN. The instance id is
// used to detect SOURCE_MISMATCH at check time.
func NewCheckReader(targetDSN, instanceID string) *CheckReader {
	db, dbErr := sql.Open("mysql", targetDSN)
	if dbErr == nil {
		db.SetMaxOpenConns(4)
		db.SetMaxIdleConns(4)
	}
	return &CheckReader{
		instanceID: instanceID,
		inspector:  schema.NewInspector(targetDSN),
		db:         db,
		dbErr:      dbErr,
		schemas:    make(map[core.TableRef]core.TableSchema),
	}
}

// NewCheckReaderFromBackend pulls the target DSN and instance id out
// of a Backend, avoiding a redundant ping in the CLI.
func NewCheckReaderFromBackend(b *Backend) *CheckReader {
	return NewCheckReader(b.targetDSN, b.targetInstanceID)
}

func (r *CheckReader) Close() error {
	if r.db == nil {
		return nil
	}
	return r.db.Close()
}

// TargetInstanceID implements executor.Reader.
func (r *CheckReader) TargetInstanceID() string { return r.instanceID }

// Fingerprint implements executor.Reader.
func (r *CheckReader) Fingerprint(ctx context.Context, t core.TableRef) (core.SchemaFingerprint, error) {
	sch, err := r.schemaFor(ctx, t)
	if err != nil {
		return "", err
	}
	return schema.FingerprintSchema(sch), nil
}

// ReadByKey implements executor.Reader.
func (r *CheckReader) ReadByKey(ctx context.Context, t core.TableRef, keyColumns []string, key core.Row) (executor.Row, bool, error) {
	if r.dbErr != nil {
		return executor.Row{}, false, fmt.Errorf("mysql: open: %w", r.dbErr)
	}
	if len(keyColumns) == 0 {
		return executor.Row{}, false, fmt.Errorf("mysql: empty key columns for %s", t)
	}
	sch, err := r.schemaFor(ctx, t)
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
		colNames = append(colNames, quoteIdent(c.Name))
		colTypes = append(colTypes, mysqlvalue.ColumnType{
			Database:   t.Schema,
			Table:      t.Name,
			Name:       c.Name,
			ColumnType: string(c.NativeType),
			Nullable:   yesNo(c.Nullable),
			Charset:    c.Charset,
		})
	}
	keyRow := core.Row{Columns: make([]string, 0, len(keyColumns)), Values: make([]core.Value, 0, len(keyColumns))}
	for _, kc := range keyColumns {
		v, ok := key.Get(kc)
		if !ok {
			return executor.Row{}, false, fmt.Errorf("mysql: key column %q missing from key image", kc)
		}
		keyRow.Columns = append(keyRow.Columns, kc)
		keyRow.Values = append(keyRow.Values, v)
	}
	where, args, err := buildPredicate(keyRow)
	if err != nil {
		return executor.Row{}, false, fmt.Errorf("mysql: key predicate: %w", err)
	}
	query := "SELECT " + strings.Join(colNames, ", ") +
		" FROM " + quoteIdent(t.Schema) + "." + quoteIdent(t.Name) +
		" WHERE " + where +
		" LIMIT 1"

	rows, err := r.db.QueryContext(ctx, query, args...)
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

func (r *CheckReader) schemaFor(ctx context.Context, table core.TableRef) (core.TableSchema, error) {
	if cached, ok := r.schemas[table]; ok {
		return cached, nil
	}
	sch, err := r.inspector.InspectTable(ctx, table)
	if err != nil {
		return core.TableSchema{}, err
	}
	r.schemas[table] = sch
	return sch, nil
}

func rowToExecutor(r core.Row) executor.Row {
	return executor.Row{Columns: r.Columns, Values: r.Values}
}

func columnsByName(sch core.TableSchema) []core.ColumnDef {
	// We need ordered output, but the schema already gives us columns
	// in ordinal order from the inspector.
	return sch.Columns
}

// yesNo mirrors value.yesNo without an import cycle. Kept here so the
// value package owns type decoding and the executor owns SQL plumbing.
func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}
