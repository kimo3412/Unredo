// Package source reads MySQL ROW binlog and converts events into
// core.Transaction objects. It depends on go-mysql-org/go-mysql for
// protocol and event decoding, and translates every value through the
// value package so the planner sees core.Value rather than driver types.
package source

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/replication"
	gomysqldriver "github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/backends/mysql/schema"
	mysqlvalue "github.com/girimi/unredo/internal/backends/mysql/value"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

// Source implements ports.ChangeSource for MySQL ROW binlog.
type Source struct {
	dsn        string
	instanceID string
	serverID   uint32
	inspector  *schema.Inspector
	maxRows    int
	maxBytes   int64
}

// New builds a Source. serverID must be a non-zero, profile-unique value.
func New(dsn, instanceID string, serverID uint32, maxRows int, maxBytes int64) *Source {
	return &Source{
		dsn:        dsn,
		instanceID: instanceID,
		serverID:   serverID,
		inspector:  schema.NewInspector(dsn),
		maxRows:    maxRows,
		maxBytes:   maxBytes,
	}
}

// Capabilities reports what the connected server actually provides.
func (s *Source) Capabilities(ctx context.Context) (core.BackendCapabilities, error) {
	format, rowImage, gtid, rowMetadata, err := s.settings(ctx)
	if err != nil {
		return core.BackendCapabilities{}, err
	}
	rowFull := strings.EqualFold(format, "ROW") && strings.EqualFold(rowImage, "FULL")
	gtidOn := strings.EqualFold(gtid, "ON")
	metadataFull := strings.EqualFold(rowMetadata, "FULL")
	return core.BackendCapabilities{
		FullBeforeImage:       rowFull,
		FullAfterImage:        rowFull,
		StableTransactionID:   gtidOn,
		TransactionBoundaries: strings.EqualFold(format, "ROW"),
		AtomicActionMarker:    true,
		SchemaAtEventTime:     metadataFull,
		SupportsReapply:       rowFull,
	}, nil
}

func (s *Source) settings(ctx context.Context) (format, rowImage, gtid, rowMetadata string, err error) {
	db, err := sql.Open("mysql", s.dsn)
	if err != nil {
		return "", "", "", "", err
	}
	defer db.Close()
	err = db.QueryRowContext(ctx,
		"SELECT @@global.binlog_format, @@global.binlog_row_image, @@global.gtid_mode, @@global.binlog_row_metadata").Scan(&format, &rowImage, &gtid, &rowMetadata)
	return
}

// Scan opens a replication stream and returns an iterator.
func (s *Source) Scan(ctx context.Context, scope ports.ScanScope) (ports.TransactionIterator, error) {
	if s.serverID == 0 {
		return nil, fmt.Errorf("mysql: source.server_id is 0; set a non-zero value in the profile")
	}
	caps, err := s.Capabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("mysql: inspect binlog capabilities: %w", err)
	}
	if !caps.FullBeforeImage || !caps.FullAfterImage || !caps.StableTransactionID || !caps.SchemaAtEventTime {
		return nil, fmt.Errorf("mysql: ROW/FULL/GTID with binlog_row_metadata=FULL required: %w", ports.ErrUnsupportedCapability)
	}
	host, port, user, pass, err := parseDSN(s.dsn)
	if err != nil {
		return nil, err
	}
	cfg := replication.BinlogSyncerConfig{
		ServerID:       s.serverID,
		Flavor:         "mysql",
		Host:           host,
		Port:           port,
		User:           user,
		Password:       pass,
		RawModeEnabled: false, // we want the library to decode; we translate types
	}
	pos, err := s.resolveScope(scope)
	if err != nil {
		return nil, err
	}
	syncer := replication.NewBinlogSyncer(cfg)
	streamer, err := syncer.StartSync(pos)
	if err != nil {
		syncer.Close()
		return nil, fmt.Errorf("mysql: start binlog sync at %s: %w", pos, err)
	}
	return &binlogIterator{
		ctx:        ctx,
		syncer:     syncer,
		streamer:   streamer,
		instanceID: s.instanceID,
		inspector:  s.inspector,
		limit:      scope.Limit,
		maxRows:    s.maxRows,
		maxBytes:   s.maxBytes,
		colCache:   map[string][]columnInfo{},
	}, nil
}

// Find locates one transaction by GTID.
func (s *Source) Find(ctx context.Context, ref core.TransactionRef) (*core.Transaction, error) {
	if ref.Backend != "mysql" {
		return nil, fmt.Errorf("mysql: backend mismatch %q", ref.Backend)
	}
	if ref.InstanceID != "" && ref.InstanceID != s.instanceID {
		return nil, ports.ErrInstanceMismatch
	}
	want := stripGTIDPrefix(ref.NativeTransactionID)
	if want == "" {
		return nil, fmt.Errorf("mysql: empty native transaction id")
	}
	iter, err := s.Scan(ctx, ports.ScanScope{FromCursor: ref.Cursor})
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}
		txn, err := iter.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, ports.ErrTransactionNotFound
			}
			return nil, err
		}
		if stripGTIDPrefix(txn.GTID) == want {
			return txn, nil
		}
	}
}

func (s *Source) resolveScope(scope ports.ScanScope) (gomysql.Position, error) {
	if len(scope.FromCursor) == 0 {
		return gomysql.Position{Name: "", Pos: 4}, nil
	}
	c, err := decodeCursor(scope.FromCursor)
	if err != nil {
		return gomysql.Position{}, err
	}
	if c.File == "" {
		return gomysql.Position{Name: "", Pos: 4}, nil
	}
	return gomysql.Position{Name: c.File, Pos: c.StartPos}, nil
}

type cursorData struct {
	File     string `json:"file"`
	StartPos uint32 `json:"start_pos"`
	EndPos   uint32 `json:"end_pos,omitempty"`
	GTIDSet  string `json:"gtid_set,omitempty"`
}

func decodeCursor(raw json.RawMessage) (*cursorData, error) {
	if len(raw) == 0 {
		return &cursorData{}, nil
	}
	var c cursorData
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("mysql: decode cursor: %w", err)
	}
	return &c, nil
}

func stripGTIDPrefix(s string) string {
	if i := strings.Index(s, ":"); i > 0 {
		return s[i+1:]
	}
	return s
}

// parseDSN extracts (host, port, user, pass) from a go-sql-driver DSN.
func parseDSN(dsn string) (string, uint16, string, string, error) {
	cfg, err := gomysqldriver.ParseDSN(dsn)
	if err != nil {
		return "", 0, "", "", err
	}
	host := cfg.Addr
	port := uint16(3306)
	if hp := strings.SplitN(cfg.Addr, ":", 2); len(hp) == 2 {
		host = hp[0]
		var p int
		_, _ = fmt.Sscanf(hp[1], "%d", &p)
		if p > 0 {
			port = uint16(p)
		}
	}
	return host, port, cfg.User, cfg.Passwd, nil
}

// columnInfo carries the MySQL column type label and the column name.
type columnInfo struct {
	Name       string
	ColumnType string // e.g. "decimal(12,2)", "bigint unsigned"
	Nullable   bool
}

// binlogIterator reads events and assembles them into transactions.
type binlogIterator struct {
	ctx        context.Context
	syncer     *replication.BinlogSyncer
	streamer   *replication.BinlogStreamer
	instanceID string
	inspector  *schema.Inspector

	mu        sync.Mutex
	pending   []core.RowChange
	current   *core.Transaction
	started   time.Time
	tableID   uint64
	tableMap  *replication.TableMapEvent
	colCache  map[string][]columnInfo
	emitted   int
	limit     int
	exhausted bool
	maxRows   int
	maxBytes  int64
	rowCount  int
	rowBytes  int64
	tooLarge  bool
}

func (b *binlogIterator) Close() error {
	b.syncer.Close()
	return nil
}

func (b *binlogIterator) Next(ctx context.Context) (*core.Transaction, error) {
	for {
		if b.exhausted {
			return nil, io.EOF
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-b.ctx.Done():
			return nil, b.ctx.Err()
		default:
		}

		if b.current != nil && !b.current.CommitTime.IsZero() {
			out := b.current
			b.current = nil
			b.pending = nil
			b.rowCount = 0
			b.rowBytes = 0
			b.tooLarge = false
			if b.limit > 0 {
				b.emitted++
				if b.emitted >= b.limit {
					b.exhausted = true
				}
			}
			return out, nil
		}

		ev, err := b.streamer.GetEvent(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, err
		}
		if err := b.handleEvent(ev); err != nil {
			return nil, err
		}
	}
}

func (b *binlogIterator) handleEvent(ev *replication.BinlogEvent) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch ev.Header.EventType {
	case replication.ROTATE_EVENT:
		return nil
	case replication.FORMAT_DESCRIPTION_EVENT:
		b.started = time.Unix(int64(ev.Header.Timestamp), 0).UTC()
		return nil
	case replication.PREVIOUS_GTIDS_EVENT:
		return nil
	case replication.GTID_EVENT:
		return b.beginTransaction(ev)
	case replication.QUERY_EVENT:
		return b.handleQuery(ev)
	case replication.TABLE_MAP_EVENT:
		return b.handleTableMap(ev)
	case replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2,
		replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2,
		replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		return b.handleRowEvent(ev)
	case replication.XID_EVENT:
		return b.commitTransaction(ev)
	}
	return nil
}

func (b *binlogIterator) beginTransaction(ev *replication.BinlogEvent) error {
	gtid, err := decodeGTID(ev)
	if err != nil {
		return err
	}
	if b.current == nil {
		b.current = &core.Transaction{
			Ref: core.TransactionRef{
				Backend:    "mysql",
				InstanceID: b.instanceID,
			},
			StartTime: b.eventTime(ev),
		}
	}
	b.current.Ref.NativeTransactionID = gtid
	b.current.GTID = gtid
	return nil
}

func (b *binlogIterator) handleTableMap(ev *replication.BinlogEvent) error {
	m, ok := ev.Event.(*replication.TableMapEvent)
	if !ok {
		return nil
	}
	if b.current == nil {
		b.current = &core.Transaction{Ref: core.TransactionRef{
			Backend:    "mysql",
			InstanceID: b.instanceID,
		}}
	}
	b.tableID = m.TableID
	b.tableMap = m
	return nil
}

func (b *binlogIterator) handleRowEvent(ev *replication.BinlogEvent) error {
	raw, ok := ev.Event.(*replication.RowsEvent)
	if !ok {
		return nil
	}
	if b.tableMap == nil || raw.TableID != b.tableMap.TableID {
		return nil
	}
	tableRef := core.TableRef{
		Schema: strings.TrimRight(string(b.tableMap.Schema), " "),
		Name:   strings.TrimRight(string(b.tableMap.Table), " "),
	}
	// Skip the marker and other system tables. They show up in the
	// binlog whenever we INSERT into action_markers, but the unredo
	// plan never operates on them.
	if isSystemTable(tableRef) {
		return nil
	}
	cols, err := b.columnsFor(tableRef)
	if err != nil {
		return err
	}
	if len(b.tableMap.ColumnName) != len(cols) {
		b.current.Executable = false
		b.current.Reasons = append(b.current.Reasons,
			fmt.Sprintf("%s event metadata has %d column names, current schema has %d; historical schema cannot be proven", tableRef, len(b.tableMap.ColumnName), len(cols)))
		return nil
	}
	for i, name := range b.tableMap.ColumnName {
		if string(name) != cols[i].Name {
			b.current.Executable = false
			b.current.Reasons = append(b.current.Reasons,
				fmt.Sprintf("%s event column %d is %q, current schema is %q; historical schema drift", tableRef, i+1, name, cols[i].Name))
			return nil
		}
	}
	op := opForEventType(ev.Header.EventType)
	for i := 0; i < len(raw.Rows); {
		sequence := b.rowCount + 1
		switch op {
		case core.OpInsert:
			row, err := buildRow(tableRef, cols, raw.Rows[i])
			if err != nil {
				return err
			}
			b.appendChange(core.RowChange{
				Sequence:  sequence,
				Table:     tableRef,
				Operation: op,
				Key:       row,
				After:     row,
			})
			i++
		case core.OpDelete:
			row, err := buildRow(tableRef, cols, raw.Rows[i])
			if err != nil {
				return err
			}
			b.appendChange(core.RowChange{
				Sequence:  sequence,
				Table:     tableRef,
				Operation: op,
				Key:       row,
				Before:    row,
			})
			i++
		case core.OpUpdate:
			pre, err := buildRow(tableRef, cols, raw.Rows[i])
			if err != nil {
				return err
			}
			post, err := buildRow(tableRef, cols, raw.Rows[i+1])
			if err != nil {
				return err
			}
			b.appendChange(core.RowChange{
				Sequence:  sequence,
				Table:     tableRef,
				Operation: op,
				Key:       post,
				Before:    pre,
				After:     post,
			})
			i += 2
		}
	}
	return nil
}

func (b *binlogIterator) appendChange(change core.RowChange) {
	b.rowCount++
	if b.tooLarge {
		return
	}
	b.pending = append(b.pending, change)
	raw, _ := json.Marshal(change)
	b.rowBytes += int64(len(raw))
	if (b.maxRows > 0 && b.rowCount > b.maxRows) || (b.maxBytes > 0 && b.rowBytes > b.maxBytes) {
		b.tooLarge = true
		b.pending = nil
		b.current.Executable = false
		b.current.Reasons = append(b.current.Reasons, fmt.Sprintf("transaction exceeds configured limit (rows=%d bytes=%d)", b.rowCount, b.rowBytes))
	}
}

func (b *binlogIterator) handleQuery(ev *replication.BinlogEvent) error {
	q, ok := ev.Event.(*replication.QueryEvent)
	if !ok {
		return nil
	}
	if b.current == nil {
		b.current = &core.Transaction{Ref: core.TransactionRef{
			Backend:    "mysql",
			InstanceID: b.instanceID,
		}, StartTime: b.eventTime(ev)}
	}
	stmt := string(q.Query)
	kind := classifyStatement(stmt)
	if kind == stmtNonRow {
		b.current.Executable = false
		// Keep at most one reason per first keyword; full statement lines
		// can be long and many duplicates bloat the output.
		reason := "non-row statement: " + firstLine(stmt)
		if !hasReasonPrefix(b.current.Reasons, "non-row statement:") {
			b.current.Reasons = append(b.current.Reasons, reason)
		}
		// DDL and DCL QueryEvents are autocommit transactions and do not
		// receive an XID_EVENT. Mark them complete here so their state cannot
		// leak into the next GTID/row transaction.
		b.current.CommitTime = b.eventTime(ev)
		b.current.Rows = b.pending
	}
	return nil
}

func hasReasonPrefix(reasons []string, prefix string) bool {
	for _, r := range reasons {
		if len(r) >= len(prefix) && r[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

func (b *binlogIterator) commitTransaction(ev *replication.BinlogEvent) error {
	if b.current == nil {
		return nil
	}
	b.current.CommitTime = b.eventTime(ev)
	b.current.Rows = b.pending
	if b.current.Executable == false && len(b.current.Reasons) == 0 {
		b.current.Executable = true
	}
	return nil
}

func (b *binlogIterator) eventTime(ev *replication.BinlogEvent) time.Time {
	return time.Unix(int64(ev.Header.Timestamp), 0).UTC()
}

// columnsFor looks up the columns for a table and pairs them with the
// native MySQL type label. The label is what the value package uses
// to pick a kind.
func (b *binlogIterator) columnsFor(t core.TableRef) ([]columnInfo, error) {
	if cached, ok := b.colCache[t.String()]; ok {
		return cached, nil
	}
	sch, err := b.inspector.InspectTable(b.ctx, t)
	if err != nil {
		return nil, fmt.Errorf("mysql: inspect %s: %w", t, err)
	}
	out := make([]columnInfo, 0, len(sch.Columns))
	for _, c := range sch.Columns {
		out = append(out, columnInfo{
			Name:       c.Name,
			ColumnType: string(c.NativeType),
			Nullable:   c.Nullable,
		})
	}
	b.colCache[t.String()] = out
	return out, nil
}

// buildRow converts one decoded row from the binlog library into a
// core.Row by mapping each value through the value package.
func buildRow(t core.TableRef, cols []columnInfo, row []interface{}) (core.Row, error) {
	if len(row) != len(cols) {
		return core.Row{}, fmt.Errorf("mysql: %s row has %d values, expected %d columns", t, len(row), len(cols))
	}
	out := core.Row{
		Columns: make([]string, 0, len(cols)),
		Values:  make([]core.Value, 0, len(cols)),
	}
	for i, v := range row {
		ct := mysqlvalue.ColumnType{
			Database:   t.Schema,
			Table:      t.Name,
			Name:       cols[i].Name,
			ColumnType: cols[i].ColumnType,
			Nullable:   yesNo(cols[i].Nullable),
		}
		cv, err := mysqlvalue.Decode(ct, v)
		if err != nil {
			return core.Row{}, err
		}
		out.Columns = append(out.Columns, cols[i].Name)
		out.Values = append(out.Values, cv)
	}
	return out, nil
}

func opForEventType(t replication.EventType) core.OperationKind {
	switch t {
	case replication.WRITE_ROWS_EVENTv1, replication.WRITE_ROWS_EVENTv2:
		return core.OpInsert
	case replication.UPDATE_ROWS_EVENTv1, replication.UPDATE_ROWS_EVENTv2:
		return core.OpUpdate
	case replication.DELETE_ROWS_EVENTv1, replication.DELETE_ROWS_EVENTv2:
		return core.OpDelete
	}
	return ""
}

// decodeGTID extracts the textual GTID from a GTID_EVENT.
func decodeGTID(ev *replication.BinlogEvent) (string, error) {
	g, ok := ev.Event.(*replication.GTIDEvent)
	if !ok {
		return "", fmt.Errorf("mysql: event is not a GTID event: %T", ev.Event)
	}
	if len(g.SID) == 0 {
		return "", fmt.Errorf("mysql: GTID event has empty SID")
	}
	// Format: 8-4-4-4-12 hex digits joined by '-', then ':' then GNO.
	sid := formatSID(g.SID)
	return fmt.Sprintf("%s:%d", sid, g.GNO), nil
}

func formatSID(b []byte) string {
	if len(b) != 16 {
		return fmt.Sprintf("%x", b)
	}
	hex := fmt.Sprintf("%x", b)
	return hex[0:8] + "-" + hex[8:12] + "-" + hex[12:16] + "-" + hex[16:20] + "-" + hex[20:32]
}

type stmtKind int

const (
	stmtRow stmtKind = iota
	stmtNonRow
)

func classifyStatement(stmt string) stmtKind {
	up := strings.ToUpper(strings.TrimSpace(stmt))
	// DDL keywords.
	ddl := []string{"CREATE TABLE", "ALTER TABLE", "DROP TABLE", "TRUNCATE", "RENAME", "CREATE INDEX", "DROP INDEX", "CREATE VIEW", "DROP VIEW"}
	for _, kw := range ddl {
		if strings.HasPrefix(up, kw) {
			return stmtNonRow
		}
	}
	// DCL and account/permission management: out of scope for M0.
	dcl := []string{"CREATE USER", "ALTER USER", "DROP USER", "GRANT", "REVOKE", "SET PASSWORD", "RENAME USER", "CREATE ROLE", "DROP ROLE"}
	for _, kw := range dcl {
		if strings.HasPrefix(up, kw) {
			return stmtNonRow
		}
	}
	// Schema-level CREATE/DROP without the "TABLE"/"USER" suffix (rare
	// here) is still flagged as non-row.
	if strings.HasPrefix(up, "CREATE ") || strings.HasPrefix(up, "DROP ") || strings.HasPrefix(up, "ALTER ") {
		return stmtNonRow
	}
	return stmtRow
}

// isSystemTable reports whether the binlog iterator should ignore
// row events on this table. We never want to plan against the
// unredo marker table, the mysql system schema, or information_schema.
func isSystemTable(t core.TableRef) bool {
	switch t.Schema {
	case "mysql", "information_schema", "performance_schema", "sys",
		"unredo_meta":
		return true
	}
	return false
}

func firstLine(s string) string {
	if i := strings.Index(s, "\n"); i > 0 {
		return s[:i]
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "YES"
	}
	return "NO"
}
