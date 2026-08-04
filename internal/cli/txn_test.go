package cli

import (
	"bytes"
	"context"
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
