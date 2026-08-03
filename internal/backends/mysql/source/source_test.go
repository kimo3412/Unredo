package source

import (
	"strings"
	"testing"

	"github.com/go-mysql-org/go-mysql/replication"
)

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
