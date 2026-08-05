//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-sql-driver/mysql"

	mysqlsource "github.com/girimi/unredo/internal/backends/mysql/source"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/txnindex"
)

func TestLocalBinlogArchiveFindsCommittedTransaction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	marker := fmt.Sprintf("local-%d", time.Now().UnixNano()%100000000)
	markerUser := 970000 + int(time.Now().UnixNano()%20000)
	result, err := execConn.Exec(
		"INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (?, ?, ?)",
		markerUser, marker, "17.25",
	)
	if err != nil {
		t.Fatalf("insert local-file fixture: %v", err)
	}
	rowID, _ := result.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE id = ?", rowID) })

	gtid, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	binlogName, err := readCurrentBinlogFile(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	archiveDir := t.TempDir()
	archiveBinlog(t, rootConn, binlogName, filepath.Join(archiveDir, binlogName))

	var instanceID string
	if err := rootConn.QueryRow("SELECT @@server_uuid").Scan(&instanceID); err != nil {
		t.Fatalf("read server uuid: %v", err)
	}
	cfg := mysql.NewConfig()
	cfg.User = readerUser
	cfg.Passwd = readerPass
	cfg.Net = "tcp"
	cfg.Addr = "127.0.0.1:3306"
	cfg.Loc = time.UTC
	source := mysqlsource.NewLocal(cfg.FormatDSN(), instanceID, archiveDir, 100001, 1000, 64<<20)
	cursor, _ := json.Marshal(map[string]any{"file": binlogName, "start_pos": 4})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	txn, err := source.Find(ctx, core.TransactionRef{
		Backend:             "mysql",
		InstanceID:          instanceID,
		NativeTransactionID: gtid,
		Cursor:              cursor,
	})
	if err != nil {
		t.Fatalf("find %s in archived binlog: %v", gtid, err)
	}
	if txn.GTID != gtid || len(txn.Rows) != 1 || !txn.Executable {
		t.Fatalf("unexpected archived transaction: %+v", txn)
	}
	status, ok := txn.Rows[0].After.Get("status")
	if !ok || strings.Trim(string(status.Data), `"`) != marker {
		t.Fatalf("archived status = %s, want %q", status.Data, marker)
	}
	var savedCursor map[string]any
	if err := json.Unmarshal(txn.Ref.Cursor, &savedCursor); err != nil || savedCursor["file"] != binlogName {
		t.Fatalf("transaction did not retain archive cursor: %s (%v)", txn.Ref.Cursor, err)
	}

	indexPath := filepath.Join(t.TempDir(), "transactions.jsonl")
	build, err := txnindex.Build(ctx, source, source, txnindex.BuildOptions{
		OutputPath: indexPath, Backend: "mysql", InstanceID: instanceID,
	})
	if err != nil {
		t.Fatalf("build archive index: %v", err)
	}
	if build.FilesScanned != 1 || build.Transactions == 0 {
		t.Fatalf("unexpected index build result: %+v", build)
	}
	var indexed []txnindex.Entry
	_, count, err := txnindex.Query(ctx, indexPath, txnindex.Filter{GTID: gtid}, func(entry txnindex.Entry) error {
		indexed = append(indexed, entry)
		return nil
	})
	if err != nil {
		t.Fatalf("query archive index: %v", err)
	}
	if count != 1 || len(indexed) != 1 || indexed[0].SourceFile != binlogName || indexed[0].Ref.NativeTransactionID != gtid {
		t.Fatalf("unexpected indexed transaction: count=%d entries=%+v", count, indexed)
	}

	marker2 := fmt.Sprintf("incr-%d", time.Now().UnixNano()%100000000)
	result2, err := execConn.Exec(
		"INSERT INTO unredo_shop.orders (user_id, status, amount) VALUES (?, ?, ?)",
		markerUser+1, marker2, "18.25",
	)
	if err != nil {
		t.Fatalf("insert incremental fixture: %v", err)
	}
	rowID2, _ := result2.LastInsertId()
	t.Cleanup(func() { _, _ = execConn.Exec("DELETE FROM unredo_shop.orders WHERE id = ?", rowID2) })
	gtid2, err := latestGTID(rootConn)
	if err != nil {
		t.Fatal(err)
	}
	archiveBinlog(t, rootConn, binlogName, filepath.Join(archiveDir, binlogName))
	updatedPath := filepath.Join(t.TempDir(), "transactions-updated.jsonl")
	updated, err := txnindex.Update(ctx, source, source, txnindex.UpdateOptions{
		ExistingPath: indexPath, OutputPath: updatedPath, Backend: "mysql", InstanceID: instanceID,
	})
	if err != nil {
		t.Fatalf("increment archive index: %v", err)
	}
	if updated.Current || updated.FilesRescanned != 1 || updated.ScannedTransactions <= build.Transactions {
		t.Fatalf("unexpected incremental result: old=%+v updated=%+v", build, updated)
	}
	_, count, err = txnindex.Query(ctx, updatedPath, txnindex.Filter{GTID: gtid2}, func(entry txnindex.Entry) error {
		indexed = append(indexed, entry)
		return nil
	})
	if err != nil || count != 1 || indexed[len(indexed)-1].Ref.NativeTransactionID != gtid2 {
		t.Fatalf("incremented index missing %s: count=%d entries=%+v err=%v", gtid2, count, indexed, err)
	}
}

func archiveBinlog(t *testing.T, db *sql.DB, name, destination string) {
	t.Helper()
	var dataDir string
	if err := db.QueryRow("SELECT @@datadir").Scan(&dataDir); err != nil {
		t.Fatalf("read mysql datadir: %v", err)
	}
	sourcePath := filepath.Join(dataDir, name)
	if err := copyRegularFile(sourcePath, destination); err == nil {
		return
	}
	container := os.Getenv("UNREDO_MYSQL_CONTAINER")
	if container == "" {
		container = "unredo-mysql"
	}
	containerPath := strings.TrimRight(filepath.ToSlash(dataDir), "/") + "/" + name
	cmd := exec.Command("docker", "cp", container+":"+containerPath, destination)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("archive binlog from %q or docker container %q: %v\n%s", sourcePath, container, err, out)
	}
}

func copyRegularFile(source, destination string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(destination)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}
