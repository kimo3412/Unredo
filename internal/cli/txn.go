package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/config"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
	"github.com/girimi/unredo/internal/redact"
	"github.com/girimi/unredo/internal/registry"
)

func init() { Register(newTxnCmd) }

func newTxnCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "txn",
		Short: "Inspect transactions from the binlog (list, show)",
	}
	c.AddCommand(newTxnListCmd(), newTxnShowCmd())
	return c
}

func newTxnListCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "list",
		Short: "Stream committed transactions from the binlog within a cursor range",
		RunE:  runTxnList,
	}
	c.Flags().String("binlog", "", "starting binlog file (server-side logical name, e.g. mysql-bin.000123)")
	c.Flags().Uint32("from-pos", 4, "position to start reading from")
	c.Flags().String("database", "", "filter to one database")
	c.Flags().String("table", "", "filter to one table")
	c.Flags().Int("limit", 20, "max transactions to print; 0 means unlimited")
	c.Flags().Duration("max-time", 5*time.Second, "abort after this much wall-clock time")
	return c
}

func newTxnShowCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "show",
		Short: "Show row changes for one transaction, by GTID",
		RunE:  runTxnShow,
	}
	c.Flags().String("binlog", "", "starting binlog file")
	c.Flags().Uint32("from-pos", 4, "starting position")
	c.Flags().String("txn", "", "transaction id (uuid:gnum)")
	c.Flags().Bool("show-values", false, "print full values (sensitive)")
	return c
}

func runTxnList(cmd *cobra.Command, _ []string) error {
	be, p, err := resolveBackend(cmd)
	if err != nil {
		return err
	}
	src, ok := be.(ports.ChangeSource)
	if !ok {
		return fmt.Errorf("backend %q does not implement ChangeSource", be.Name())
	}

	binlog, _ := cmd.Flags().GetString("binlog")
	fromPos, _ := cmd.Flags().GetUint32("from-pos")
	database, _ := cmd.Flags().GetString("database")
	table, _ := cmd.Flags().GetString("table")
	limit, _ := cmd.Flags().GetInt("limit")
	maxTime, _ := cmd.Flags().GetDuration("max-time")

	cursor := map[string]interface{}{}
	if binlog != "" {
		cursor["file"] = binlog
		cursor["start_pos"] = fromPos
	}
	cursorJSON, _ := json.Marshal(cursor)

	ctx, cancel := context.WithTimeout(cmd.Context(), maxTime)
	defer cancel()

	iter, err := src.Scan(ctx, ports.ScanScope{
		FromCursor: cursorJSON,
		Database:   database,
		Table:      table,
		Limit:      limit,
	})
	if err != nil {
		return err
	}
	defer iter.Close()

	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		return runTxnListJSON(ctx, cmd, iter)
	}
	return runTxnListTable(ctx, cmd, iter, p)
}

func runTxnListJSON(ctx context.Context, cmd *cobra.Command, iter ports.TransactionIterator) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	for {
		txn, err := iter.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if err := enc.Encode(txn); err != nil {
			return err
		}
	}
}

func runTxnListTable(ctx context.Context, cmd *cobra.Command, iter ports.TransactionIterator, _ *config.Profile) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "%-40s %-22s %5s %-40s %-10s\n", "GTID", "COMMIT_TIME", "ROWS", "TABLES", "REVERSIBLE")
	for {
		txn, err := iter.Next(ctx)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, context.DeadlineExceeded) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		tables := distinctTables(txn)
		rev := "yes"
		if !txn.Executable {
			rev = "no"
			if len(txn.Reasons) > 0 {
				rev = "no:" + firstWord(txn.Reasons[0])
			}
		}
		gtid := txn.GTID
		if gtid == "" {
			gtid = txn.Ref.NativeTransactionID
		}
		fmt.Fprintf(out, "%-40s %-22s %5d %-40s %-10s\n",
			truncate(gtid, 40),
			txn.CommitTime.Format(time.RFC3339),
			transactionRowCount(txn),
			truncate(tables, 40),
			rev,
		)
	}
}

func runTxnShow(cmd *cobra.Command, _ []string) error {
	be, _, err := resolveBackend(cmd)
	if err != nil {
		return err
	}
	src, ok := be.(ports.ChangeSource)
	if !ok {
		return fmt.Errorf("backend %q does not implement ChangeSource", be.Name())
	}
	txnID, _ := cmd.Flags().GetString("txn")
	if txnID == "" {
		return fmt.Errorf("--txn is required")
	}
	binlog, _ := cmd.Flags().GetString("binlog")
	fromPos, _ := cmd.Flags().GetUint32("from-pos")
	cursor, _ := json.Marshal(map[string]interface{}{
		"file":      binlog,
		"start_pos": fromPos,
	})

	// Discover the backend's instance id by issuing a cheap capabilities
	// call, then passing it into the ref so Find can validate.
	caps, err := src.Capabilities(cmd.Context())
	if err != nil {
		return err
	}
	_ = caps
	beImpl, ok := be.(interface{ InstanceID() string })
	if !ok {
		// Fall back to placeholder; Find will surface the mismatch.
	}
	ref := core.TransactionRef{
		Backend:             be.Name(),
		InstanceID:          firstNonEmpty(beImpl.InstanceID(), ""),
		NativeTransactionID: txnID,
		Cursor:              cursor,
	}

	ctx := cmd.Context()
	txn, err := src.Find(ctx, ref)
	if err != nil {
		return err
	}
	showValues, _ := cmd.Flags().GetBool("show-values")
	format, _ := cmd.Flags().GetString("format")
	if format == "json" {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(txn)
	}
	return printTxnHuman(cmd, txn, showValues)
}

func printTxnHuman(cmd *cobra.Command, txn *core.Transaction, showValues bool) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "GTID:           %s\n", txn.GTID)
	fmt.Fprintf(out, "Instance:       %s\n", txn.Ref.InstanceID)
	fmt.Fprintf(out, "Start:          %s\n", txn.StartTime.Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(out, "Commit:         %s\n", txn.CommitTime.Format("2006-01-02T15:04:05Z"))
	fmt.Fprintf(out, "Rows:           %d\n", transactionRowCount(txn))
	fmt.Fprintf(out, "Tables:         %s\n", distinctTables(txn))
	fmt.Fprintf(out, "Reversible:     %s\n", yesNo(txn.Executable))
	if len(txn.Reasons) > 0 {
		fmt.Fprintf(out, "Reasons:\n")
		for _, r := range txn.Reasons {
			fmt.Fprintf(out, "  - %s\n", r)
		}
	}
	for i, row := range txn.Rows {
		header := fmt.Sprintf("[%d] %s %s", i+1, row.Operation, row.Table)
		if showValues {
			fmt.Fprintf(out, "\n%s\n", header)
			fmt.Fprintf(out, "  key:   %s\n", redact.RowSummary(row.Key))
			if len(row.Before.Columns) > 0 {
				fmt.Fprintf(out, "  before:%s\n", redact.RowSummary(row.Before))
			}
			if len(row.After.Columns) > 0 {
				fmt.Fprintf(out, "  after: %s\n", redact.RowSummary(row.After))
			}
		} else {
			fmt.Fprintf(out, "\n%s (use --show-values to print values)\n", header)
		}
	}
	return nil
}

func distinctTables(t *core.Transaction) string {
	if len(t.Tables) > 0 {
		parts := make([]string, 0, len(t.Tables))
		for _, table := range t.Tables {
			parts = append(parts, table.String())
		}
		return strings.Join(parts, ",")
	}
	seen := map[core.TableRef]bool{}
	parts := []string{}
	for _, r := range t.Rows {
		if seen[r.Table] {
			continue
		}
		seen[r.Table] = true
		parts = append(parts, r.Table.String())
	}
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}

func transactionRowCount(txn *core.Transaction) int {
	if txn.RowCount > 0 {
		return txn.RowCount
	}
	return len(txn.Rows)
}

func firstWord(s string) string {
	for i, r := range s {
		if r == ' ' || r == ':' {
			return s[:i]
		}
	}
	return s
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 1 {
		return s[:n]
	}
	return s[:n-1] + "…"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// resolveBackend reads the global flags and returns a ready backend.
func resolveBackend(cmd *cobra.Command) (ports.Backend, *config.Profile, error) {
	profileName, _ := cmd.Flags().GetString("profile")
	cfgPath, _ := cmd.Flags().GetString("config")
	if cfgPath == "" {
		cfgPath = "unredo.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	p, err := cfg.Profile(profileName)
	if err != nil {
		return nil, nil, err
	}
	be, err := registry.Resolve(p)
	if err != nil {
		return nil, nil, err
	}
	return be, p, nil
}
