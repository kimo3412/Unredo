package source

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"

	"github.com/girimi/unredo/internal/core"
)

func TestResolveLocalBinlogPathConfinesFilesToArchiveDirectory(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "binlog.000001")
	if err := os.WriteFile(want, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := resolveLocalBinlogPath(dir, "binlog.000001")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("resolved path = %q, want %q", got, want)
	}
	for _, name := range []string{"../outside", filepath.Join(dir, "binlog.000001")} {
		if _, err := resolveLocalBinlogPath(dir, name); err == nil {
			t.Fatalf("expected unsafe path %q to be rejected", name)
		}
	}
}

func TestResolveLocalBinlogPathRequiresRegularFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := resolveLocalBinlogPath(dir, "missing"); err == nil {
		t.Fatal("expected missing local binlog to be rejected")
	}
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveLocalBinlogPath(dir, "nested"); err == nil {
		t.Fatal("expected directory to be rejected as a binlog")
	}
}

func TestResolveLocalBinlogPathRejectsSymlinkEscape(t *testing.T) {
	dir := t.TempDir()
	outsideDir := t.TempDir()
	outside := filepath.Join(outsideDir, "binlog.000001")
	if err := os.WriteFile(outside, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "linked-binlog")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are unavailable: %v", err)
	}
	if _, err := resolveLocalBinlogPath(dir, "linked-binlog"); err == nil {
		t.Fatal("expected symlink escaping the archive directory to be rejected")
	}
}

func TestDDLQueryEventCompletesWithoutXID(t *testing.T) {
	iterator := &binlogIterator{instanceID: "server-uuid"}
	gtid := &replication.BinlogEvent{
		Header: &replication.EventHeader{Timestamp: 100, EventType: replication.GTID_EVENT},
		Event:  &replication.GTIDEvent{SID: []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}, GNO: 7},
	}
	if err := iterator.handleEvent(gtid); err != nil {
		t.Fatal(err)
	}
	ddl := &replication.BinlogEvent{
		Header: &replication.EventHeader{Timestamp: 101, EventType: replication.QUERY_EVENT},
		Event:  &replication.QueryEvent{Query: []byte("CREATE DATABASE IF NOT EXISTS unredo_meta")},
	}
	if err := iterator.handleEvent(ddl); err != nil {
		t.Fatal(err)
	}
	if iterator.current == nil || iterator.current.CommitTime.IsZero() {
		t.Fatal("DDL transaction was not completed at its QueryEvent")
	}
	if iterator.current.Executable || len(iterator.current.Reasons) != 1 || !strings.Contains(iterator.current.Reasons[0], "CREATE DATABASE") {
		t.Fatalf("unexpected DDL transaction: %+v", iterator.current)
	}
}

func TestOversizeTransactionKeepsSummaryWithoutRowImages(t *testing.T) {
	iterator := &binlogIterator{
		current:  &core.Transaction{},
		maxRows:  2,
		maxBytes: 1 << 20,
	}
	table := core.TableRef{Schema: "shop", Name: "orders"}
	for i := 0; i < 4; i++ {
		iterator.appendChange(core.RowChange{Sequence: i + 1, Table: table, Operation: core.OpInsert})
	}
	if err := iterator.commitTransaction(&replication.BinlogEvent{
		Header: &replication.EventHeader{Timestamp: 100, EventType: replication.XID_EVENT},
		Event:  &replication.XIDEvent{},
	}); err != nil {
		t.Fatal(err)
	}
	if iterator.current.RowCount != 4 || len(iterator.current.Rows) != 0 || len(iterator.current.Tables) != 1 {
		t.Fatalf("oversize summary lost: %+v", iterator.current)
	}
	if iterator.current.Executable || len(iterator.current.Reasons) != 1 || !strings.Contains(iterator.current.Reasons[0], "actual_rows=4") {
		t.Fatalf("unexpected oversize classification: %+v", iterator.current)
	}
}

func TestActionMarkerIsCapturedWithoutBecomingAPlannableRow(t *testing.T) {
	iterator := &binlogIterator{instanceID: "server-uuid"}
	marker := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}
	tableMap := &replication.BinlogEvent{
		Header: &replication.EventHeader{EventType: replication.TABLE_MAP_EVENT},
		Event: &replication.TableMapEvent{
			TableID:    42,
			Schema:     []byte("unredo_meta"),
			Table:      []byte("action_markers"),
			ColumnName: [][]byte{[]byte("action_id"), []byte("plan_id")},
		},
	}
	if err := iterator.handleEvent(tableMap); err != nil {
		t.Fatal(err)
	}
	rows := &replication.BinlogEvent{
		Header: &replication.EventHeader{EventType: replication.WRITE_ROWS_EVENTv2},
		Event: &replication.RowsEvent{
			TableID: 42,
			Rows:    [][]interface{}{{marker, []byte("plan")}},
		},
	}
	if err := iterator.handleEvent(rows); err != nil {
		t.Fatal(err)
	}
	if len(iterator.pending) != 0 || len(iterator.markerActionIDs) != 1 {
		t.Fatalf("marker handling leaked into rows: pending=%d markers=%d", len(iterator.pending), len(iterator.markerActionIDs))
	}
	iterator.lastMarkerActionIDs = iterator.markerActionIDs
	if !iterator.lastTransactionHasAction(marker) {
		t.Fatal("captured marker cannot be matched")
	}
}
