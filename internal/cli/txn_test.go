package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/core"
)

type blockingTransactionIterator struct {
	observedDeadline bool
}

func (i *blockingTransactionIterator) Next(ctx context.Context) (*core.Transaction, error) {
	_, i.observedDeadline = ctx.Deadline()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (i *blockingTransactionIterator) Close() error { return nil }

type oneTransactionIterator struct {
	txn     *core.Transaction
	emitted bool
}

func (i *oneTransactionIterator) Next(context.Context) (*core.Transaction, error) {
	if i.emitted {
		return nil, io.EOF
	}
	i.emitted = true
	return i.txn, nil
}

func (i *oneTransactionIterator) Close() error { return nil }

func TestTxnListTableStopsCleanlyAtMaxTime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	cmd := &cobra.Command{}
	cmd.SetOut(&bytes.Buffer{})
	iter := &blockingTransactionIterator{}

	started := time.Now()
	if err := runTxnListTable(ctx, cmd, iter, nil); err != nil {
		t.Fatal(err)
	}
	if !iter.observedDeadline {
		t.Fatal("iterator did not receive the max-time context")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("max-time took too long: %s", elapsed)
	}
}

func TestTxnListTablePreservesFullGTID(t *testing.T) {
	const gtid = "2385308c-36eb-11f1-9e91-30c5991fb28e:1006"
	var out bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&out)
	iter := &oneTransactionIterator{txn: &core.Transaction{
		GTID: gtid, CommitTime: time.Unix(1, 0).UTC(), Executable: true,
	}}
	if err := runTxnListTable(context.Background(), cmd, iter, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), gtid) {
		t.Fatalf("transaction table truncated GTID:\n%s", out.String())
	}
}
