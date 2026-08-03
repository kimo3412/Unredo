// Package schema reads MySQL table definitions and computes stable
// fingerprints used by plan check.
package schema

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	_ "github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

// Inspector queries information_schema to build a ports.TableSchema and
// derives deterministic fingerprints for plan check.
type Inspector struct {
	dsn string
}

// NewInspector returns an inspector that opens connections on demand.
func NewInspector(dsn string) *Inspector { return &Inspector{dsn: dsn} }

// InspectTable fetches the engine, columns, and unique keys for one table.
func (i *Inspector) InspectTable(ctx context.Context, t core.TableRef) (core.TableSchema, error) {
	db, err := sql.Open("mysql", i.dsn)
	if err != nil {
		return core.TableSchema{}, err
	}
	defer db.Close()

	engine, err := readEngine(ctx, db, t)
	if err != nil {
		return core.TableSchema{}, err
	}

	cols, err := readColumns(ctx, db, t)
	if err != nil {
		return core.TableSchema{}, err
	}

	keys, err := readUniqueKeys(ctx, db, t)
	if err != nil {
		return core.TableSchema{}, err
	}

	cs, cl, _ := readTableCharset(ctx, db, t)

	return core.TableSchema{
		Table:      t,
		Engine:     engine,
		Columns:    cols,
		UniqueKeys: keys,
		Charset:    cs,
		Collation:  cl,
	}, nil
}

// Fingerprint returns a stable hash that changes on any drift in the
// engine, columns, unique keys, or table-level charset/collation.
func (i *Inspector) Fingerprint(ctx context.Context, t core.TableRef) (core.SchemaFingerprint, error) {
	schema, err := i.InspectTable(ctx, t)
	if err != nil {
		return "", err
	}
	return FingerprintSchema(schema), nil
}

// FingerprintSchema hashes an already-inspected schema. Callers that must
// inspect many rows from one table can cache the schema without opening a new
// information_schema connection for every row.
func FingerprintSchema(schema core.TableSchema) core.SchemaFingerprint {
	h := sha256.New()
	fmt.Fprintf(h, "table=%s.%s\n", schema.Table.Schema, schema.Table.Name)
	fmt.Fprintf(h, "engine=%s\n", schema.Engine)
	fmt.Fprintf(h, "charset=%s\n", schema.Charset)
	fmt.Fprintf(h, "collation=%s\n", schema.Collation)

	cols := append([]ports.ColumnDef(nil), schema.Columns...)
	sort.Slice(cols, func(a, b int) bool { return cols[a].Ordinal < cols[b].Ordinal })
	for _, c := range cols {
		fmt.Fprintf(h, "col[%d]=%s|%s|nullable=%t|charset=%s|collation=%s|generated=%t\n",
			c.Ordinal, c.Name, string(c.NativeType), c.Nullable, c.Charset, c.Collation, c.Generated)
	}

	keys := append([]ports.UniqueKey(nil), schema.UniqueKeys...)
	sort.Slice(keys, func(a, b int) bool {
		if keys[a].IsPrimary != keys[b].IsPrimary {
			return keys[a].IsPrimary
		}
		return keys[a].Name < keys[b].Name
	})
	for _, k := range keys {
		fmt.Fprintf(h, "key=%s|primary=%t|cols=%s\n", k.Name, k.IsPrimary, strings.Join(k.Columns, ","))
	}
	return core.SchemaFingerprint("sha256:" + hex.EncodeToString(h.Sum(nil)))
}

func readEngine(ctx context.Context, db *sql.DB, t core.TableRef) (string, error) {
	var engine, name string
	err := db.QueryRowContext(ctx,
		`SELECT ENGINE, TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?`,
		t.Schema, t.Name).Scan(&engine, &name)
	if err == sql.ErrNoRows {
		return "", fmt.Errorf("mysql: table %s.%s not found", t.Schema, t.Name)
	}
	if err != nil {
		return "", fmt.Errorf("mysql: read engine: %w", err)
	}
	if !strings.EqualFold(engine, "InnoDB") {
		return engine, fmt.Errorf("mysql: table %s.%s engine is %q, want InnoDB", t.Schema, t.Name, engine)
	}
	return engine, nil
}

func readColumns(ctx context.Context, db *sql.DB, t core.TableRef) ([]ports.ColumnDef, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ORDINAL_POSITION, COLUMN_NAME, COLUMN_TYPE, IS_NULLABLE, CHARACTER_SET_NAME, COLLATION_NAME, EXTRA
		FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY ORDINAL_POSITION`, t.Schema, t.Name)
	if err != nil {
		return nil, fmt.Errorf("mysql: read columns: %w", err)
	}
	defer rows.Close()
	var out []ports.ColumnDef
	for rows.Next() {
		var (
			ordinal    int
			name       string
			columnType string
			nullable   string
			charset    sql.NullString
			collation  sql.NullString
			extra      sql.NullString
		)
		if err := rows.Scan(&ordinal, &name, &columnType, &nullable, &charset, &collation, &extra); err != nil {
			return nil, err
		}
		generated := strings.Contains(strings.ToUpper(extra.String), "VIRTUAL") ||
			strings.Contains(strings.ToUpper(extra.String), "STORED")
		out = append(out, ports.ColumnDef{
			Name:       name,
			Ordinal:    ordinal,
			NativeType: core.NativeType(columnType),
			Nullable:   strings.EqualFold(nullable, "YES"),
			Charset:    charset.String,
			Collation:  collation.String,
			Generated:  generated,
		})
	}
	return out, rows.Err()
}

func readUniqueKeys(ctx context.Context, db *sql.DB, t core.TableRef) ([]ports.UniqueKey, error) {
	// statistics: NON_UNIQUE = 0 marks both PRIMARY and secondary uniques.
	rows, err := db.QueryContext(ctx, `
		SELECT INDEX_NAME, NON_UNIQUE, COLUMN_NAME, SEQ_IN_INDEX
		FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?
		ORDER BY INDEX_NAME, SEQ_IN_INDEX`, t.Schema, t.Name)
	if err != nil {
		return nil, fmt.Errorf("mysql: read indexes: %w", err)
	}
	defer rows.Close()
	byName := map[string]*ports.UniqueKey{}
	order := []string{}
	for rows.Next() {
		var (
			name      string
			nonUnique int
			col       string
			seq       int
		)
		if err := rows.Scan(&name, &nonUnique, &col, &seq); err != nil {
			return nil, err
		}
		if nonUnique != 0 {
			continue
		}
		k, ok := byName[name]
		if !ok {
			k = &ports.UniqueKey{
				Name:      name,
				IsPrimary: strings.EqualFold(name, "PRIMARY"),
			}
			byName[name] = k
			order = append(order, name)
		}
		k.Columns = append(k.Columns, col)
	}
	out := make([]ports.UniqueKey, 0, len(order))
	for _, n := range order {
		out = append(out, *byName[n])
	}
	return out, rows.Err()
}

func readTableCharset(ctx context.Context, db *sql.DB, t core.TableRef) (string, string, error) {
	var cs, cl sql.NullString
	err := db.QueryRowContext(ctx,
		`SELECT CCSA.CHARACTER_SET_NAME, CCSA.COLLATION_NAME
		 FROM information_schema.TABLES T
		 LEFT JOIN information_schema.COLLATION_CHARACTER_SET_APPLICABILITY CCSA
		   ON CCSA.COLLATION_NAME = T.TABLE_COLLATION
		 WHERE T.TABLE_SCHEMA = ? AND T.TABLE_NAME = ?`,
		t.Schema, t.Name).Scan(&cs, &cl)
	if err != nil {
		return "", "", nil // charset is best-effort; columns carry the truth.
	}
	return cs.String, cl.String, nil
}
