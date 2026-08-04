package cli

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/ports"
	"github.com/girimi/unredo/internal/txnindex"
)

func init() { Register(newIndexCmd) }

func newIndexCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "index",
		Short: "Build and query value-free transaction indexes for archived logs",
	}
	cmd.AddCommand(newIndexBuildCmd(), newIndexQueryCmd())
	return cmd
}

func newIndexBuildCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Scan every archived binlog in a local-file profile and build an index",
		RunE:  runIndexBuild,
	}
	cmd.Flags().String("output", "", "new index path (.jsonl); existing files are never overwritten")
	return cmd
}

func runIndexBuild(cmd *cobra.Command, _ []string) error {
	backend, _, err := resolveBackend(cmd)
	if err != nil {
		return err
	}
	source, ok := backend.(ports.ChangeSource)
	if !ok {
		return fmt.Errorf("backend %q does not implement ChangeSource", backend.Name())
	}
	catalog, ok := backend.(ports.LogCatalog)
	if !ok {
		return fmt.Errorf("backend %q does not expose archived log files", backend.Name())
	}
	output, _ := cmd.Flags().GetString("output")
	if output == "" {
		return fmt.Errorf("--output is required")
	}
	instanceID := ""
	if identified, ok := backend.(interface{ InstanceID() string }); ok {
		instanceID = identified.InstanceID()
	}
	result, err := txnindex.Build(cmd.Context(), source, catalog, txnindex.BuildOptions{
		OutputPath: output,
		Backend:    backend.Name(),
		InstanceID: instanceID,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "index:        %s\n", result.OutputPath)
	fmt.Fprintf(cmd.OutOrStdout(), "files:        %d\n", result.FilesScanned)
	fmt.Fprintf(cmd.OutOrStdout(), "transactions: %d\n", result.Transactions)
	fmt.Fprintf(cmd.OutOrStdout(), "duplicates:   %d\n", result.Duplicates)
	return nil
}

func newIndexQueryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "query INDEX",
		Short: "Query a transaction index by GTID, time, database, or table",
		Args:  cobra.ExactArgs(1),
		RunE:  runIndexQuery,
	}
	cmd.Flags().String("gtid", "", "exact native transaction ID")
	cmd.Flags().String("database", "", "database/schema name")
	cmd.Flags().String("table", "", "table name or database.table")
	cmd.Flags().String("since", "", "inclusive RFC3339 commit time")
	cmd.Flags().String("until", "", "inclusive RFC3339 commit time")
	cmd.Flags().Int("limit", 100, "maximum matches; 0 means unlimited")
	return cmd
}

func runIndexQuery(cmd *cobra.Command, args []string) error {
	gtid, _ := cmd.Flags().GetString("gtid")
	database, _ := cmd.Flags().GetString("database")
	table, _ := cmd.Flags().GetString("table")
	sinceRaw, _ := cmd.Flags().GetString("since")
	untilRaw, _ := cmd.Flags().GetString("until")
	limit, _ := cmd.Flags().GetInt("limit")
	if limit < 0 {
		return fmt.Errorf("--limit must be non-negative")
	}
	since, err := parseIndexTime("--since", sinceRaw)
	if err != nil {
		return err
	}
	until, err := parseIndexTime("--until", untilRaw)
	if err != nil {
		return err
	}
	if !since.IsZero() && !until.IsZero() && until.Before(since) {
		return fmt.Errorf("--until must not be before --since")
	}
	filter := txnindex.Filter{GTID: gtid, Database: database, Table: table, Since: since, Until: until, Limit: limit}
	format, _ := cmd.Flags().GetString("format")
	switch format {
	case "json":
		encoder := json.NewEncoder(cmd.OutOrStdout())
		_, _, err = txnindex.Query(cmd.Context(), args[0], filter, func(entry txnindex.Entry) error {
			return encoder.Encode(entry)
		})
		return err
	case "table":
		fmt.Fprintf(cmd.OutOrStdout(), "%-40s %-22s %5s %-40s %-10s %s\n", "GTID", "COMMIT_TIME", "ROWS", "TABLES", "REVERSIBLE", "FILE")
		_, _, err = txnindex.Query(cmd.Context(), args[0], filter, func(entry txnindex.Entry) error {
			reversible := "yes"
			if !entry.Executable {
				reversible = "no"
			}
			tables := make([]string, 0, len(entry.Tables))
			for _, table := range entry.Tables {
				tables = append(tables, table.String())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%-40s %-22s %5d %-40s %-10s %s\n",
				entry.Ref.NativeTransactionID, entry.CommitTime.Format(time.RFC3339), entry.RowCount,
				truncate(strings.Join(tables, ","), 40), reversible, entry.SourceFile)
			return nil
		})
		return err
	default:
		return fmt.Errorf("--format must be table or json, got %q", format)
	}
}

func parseIndexTime(flag, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("%s must be RFC3339: %w", flag, err)
	}
	return parsed, nil
}
