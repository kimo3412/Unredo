package txnindex

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

type fakeCatalog struct{ files []ports.LogFile }

func (c fakeCatalog) ListLogFiles(context.Context) ([]ports.LogFile, error) { return c.files, nil }

type fakeSource struct {
	transactions map[string][]*core.Transaction
	failFile     string
}

func (s fakeSource) Capabilities(context.Context) (core.BackendCapabilities, error) {
	return core.BackendCapabilities{}, nil
}

func (s fakeSource) Scan(_ context.Context, scope ports.ScanScope) (ports.TransactionIterator, error) {
	var cursor struct {
		File string `json:"file"`
	}
	if err := json.Unmarshal(scope.FromCursor, &cursor); err != nil {
		return nil, err
	}
	return &fakeIterator{transactions: s.transactions[cursor.File], fail: cursor.File == s.failFile}, nil
}

func (s fakeSource) Find(context.Context, core.TransactionRef) (*core.Transaction, error) {
	return nil, ports.ErrTransactionNotFound
}

type fakeIterator struct {
	transactions []*core.Transaction
	position     int
	fail         bool
}

func (i *fakeIterator) Next(context.Context) (*core.Transaction, error) {
	if i.fail {
		return nil, errors.New("fixture read failure")
	}
	if i.position >= len(i.transactions) {
		return nil, io.EOF
	}
	txn := i.transactions[i.position]
	i.position++
	return txn, nil
}

func (i *fakeIterator) Close() error { return nil }

func TestBuildAndQueryValueFreeIndex(t *testing.T) {
	commitA := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	commitB := commitA.Add(time.Hour)
	orders := core.TableRef{Schema: "shop", Name: "orders"}
	users := core.TableRef{Schema: "shop", Name: "users"}
	txnA := &core.Transaction{
		Ref:  core.TransactionRef{Backend: "mysql", InstanceID: "instance", NativeTransactionID: "uuid:1001"},
		GTID: "uuid:1001", CommitTime: commitA, RowCount: 1, Tables: []core.TableRef{orders}, Executable: true,
		Rows: []core.RowChange{{After: core.Row{Columns: []string{"secret"}, Values: []core.Value{{Data: core.RawJSON(`"must-not-be-indexed"`)}}}}},
	}
	txnB := &core.Transaction{
		Ref:  core.TransactionRef{Backend: "mysql", InstanceID: "instance", NativeTransactionID: "uuid:1002"},
		GTID: "uuid:1002", CommitTime: commitB, RowCount: 2, Tables: []core.TableRef{users}, Executable: false,
		Reasons: []string{"no stable unique key"},
	}
	source := fakeSource{transactions: map[string][]*core.Transaction{
		"binlog.000001": {txnA},
		"binlog.000002": {txnA, txnB},
	}}
	catalog := fakeCatalog{files: []ports.LogFile{{Name: "binlog.000001"}, {Name: "binlog.000002"}}}
	path := filepath.Join(t.TempDir(), "transactions.jsonl")
	result, err := Build(context.Background(), source, catalog, BuildOptions{OutputPath: path, Backend: "mysql", InstanceID: "instance"})
	if err != nil {
		t.Fatal(err)
	}
	if result.FilesScanned != 2 || result.Transactions != 2 || result.Duplicates != 1 {
		t.Fatalf("unexpected build result: %+v", result)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "must-not-be-indexed") || strings.Contains(string(raw), "no stable unique key") || strings.Contains(string(raw), `"rows"`) {
		t.Fatalf("index leaked row images: %s", raw)
	}
	var matches []Entry
	header, count, err := Query(context.Background(), path, Filter{Table: "shop.users", Since: commitB, Limit: 10}, func(entry Entry) error {
		matches = append(matches, entry)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if header.InstanceID != "instance" || count != 1 || len(matches) != 1 || matches[0].Ref.NativeTransactionID != "uuid:1002" {
		t.Fatalf("unexpected query result: header=%+v count=%d matches=%+v", header, count, matches)
	}
	var cursor map[string]any
	if err := json.Unmarshal(matches[0].Ref.Cursor, &cursor); err != nil || cursor["file"] != "binlog.000002" {
		t.Fatalf("entry cursor = %s (%v)", matches[0].Ref.Cursor, err)
	}
}

func TestBuildNeverOverwritesExistingIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transactions.jsonl")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Build(context.Background(), fakeSource{}, fakeCatalog{}, BuildOptions{OutputPath: path})
	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected no-overwrite error, got %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "original" {
		t.Fatalf("existing index changed: %q", raw)
	}
}

func TestBuildFailureRemovesPartialIndex(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transactions.jsonl")
	_, err := Build(context.Background(), fakeSource{failFile: "binlog.000001"}, fakeCatalog{
		files: []ports.LogFile{{Name: "binlog.000001"}},
	}, BuildOptions{OutputPath: path})
	if err == nil || !strings.Contains(err.Error(), "fixture read failure") {
		t.Fatalf("expected scan error, got %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial index was published: %v", err)
	}
	temps, err := filepath.Glob(filepath.Join(dir, ".unredo-index-*.tmp"))
	if err != nil || len(temps) != 0 {
		t.Fatalf("temporary files remain: %v (%v)", temps, err)
	}
}

func TestUpdateRescansGrowingTailAndAppendedFiles(t *testing.T) {
	dir := t.TempDir()
	oldPath := filepath.Join(dir, "old.jsonl")
	newPath := filepath.Join(dir, "new.jsonl")
	t1 := time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Minute)
	oldFiles := []ports.LogFile{
		{Name: "binlog.000001", Size: 100, ModifiedAt: t1},
		{Name: "binlog.000002", Size: 100, ModifiedAt: t1},
	}
	txn := func(gtid string, commit time.Time) *core.Transaction {
		return &core.Transaction{
			Ref:  core.TransactionRef{Backend: "mysql", InstanceID: "instance", NativeTransactionID: gtid},
			GTID: gtid, CommitTime: commit, Executable: true,
		}
	}
	initial := fakeSource{transactions: map[string][]*core.Transaction{
		"binlog.000001": {txn("uuid:1", t1)},
		"binlog.000002": {txn("uuid:2", t1)},
	}}
	if _, err := Build(context.Background(), initial, fakeCatalog{files: oldFiles}, BuildOptions{
		OutputPath: oldPath, Backend: "mysql", InstanceID: "instance",
	}); err != nil {
		t.Fatal(err)
	}
	oldRaw, _ := os.ReadFile(oldPath)
	currentFiles := []ports.LogFile{
		oldFiles[0],
		{Name: "binlog.000002", Size: 150, ModifiedAt: t2},
		{Name: "binlog.000003", Size: 80, ModifiedAt: t2},
	}
	updatedSource := fakeSource{transactions: map[string][]*core.Transaction{
		"binlog.000002": {txn("uuid:2", t1), txn("uuid:3", t2)},
		"binlog.000003": {txn("uuid:4", t2)},
	}}
	result, err := Update(context.Background(), updatedSource, fakeCatalog{files: currentFiles}, UpdateOptions{
		ExistingPath: oldPath, OutputPath: newPath, Backend: "mysql", InstanceID: "instance",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Current || result.CopiedTransactions != 1 || result.ScannedTransactions != 3 || result.FilesAdded != 1 || result.FilesRescanned != 2 {
		t.Fatalf("unexpected update result: %+v", result)
	}
	afterOld, _ := os.ReadFile(oldPath)
	if string(afterOld) != string(oldRaw) {
		t.Fatal("update modified the existing index")
	}
	var got []string
	header, count, err := Query(context.Background(), newPath, Filter{}, func(entry Entry) error {
		got = append(got, entry.Ref.NativeTransactionID)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != 4 || strings.Join(got, ",") != "uuid:1,uuid:2,uuid:3,uuid:4" || len(header.Files) != 3 {
		t.Fatalf("unexpected updated index: count=%d got=%v files=%+v", count, got, header.Files)
	}
	unusedOutput := filepath.Join(dir, "unused.jsonl")
	current, err := Update(context.Background(), updatedSource, fakeCatalog{files: currentFiles}, UpdateOptions{
		ExistingPath: newPath, OutputPath: unusedOutput, Backend: "mysql", InstanceID: "instance",
	})
	if err != nil || !current.Current {
		t.Fatalf("unchanged update = %+v, %v", current, err)
	}
	if _, err := os.Stat(unusedOutput); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current update unexpectedly wrote output: %v", err)
	}
}

func TestSafeAppendPlanRejectsDestructiveArchiveChanges(t *testing.T) {
	t1 := time.Date(2026, 8, 5, 1, 0, 0, 0, time.UTC)
	indexed := []ports.LogFile{
		{Name: "binlog.000001", Size: 100, ModifiedAt: t1},
		{Name: "binlog.000002", Size: 100, ModifiedAt: t1},
	}
	tests := []FileChanges{
		{Removed: []ports.LogFile{indexed[0]}},
		{Changed: []ports.LogFile{{Name: "binlog.000001", Size: 120, ModifiedAt: t1.Add(time.Minute)}}},
		{Changed: []ports.LogFile{{Name: "binlog.000002", Size: 90, ModifiedAt: t1.Add(time.Minute)}}},
		{Added: []ports.LogFile{{Name: "binlog.000000", Size: 50, ModifiedAt: t1}}},
	}
	for _, changes := range tests {
		if _, err := safeAppendPlan(indexed, changes); err == nil || !strings.Contains(err.Error(), "full index build") {
			t.Fatalf("expected full rebuild rejection for %+v, got %v", changes, err)
		}
	}
}
