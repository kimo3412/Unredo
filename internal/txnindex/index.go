// Package txnindex builds and queries value-free transaction summary indexes.
package txnindex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

const FormatVersion = 1

const (
	recordHeader      = "unredo_transaction_index"
	recordTransaction = "transaction"
)

type Header struct {
	Type          string          `json:"type"`
	FormatVersion int             `json:"format_version"`
	CreatedAt     time.Time       `json:"created_at"`
	Backend       string          `json:"backend"`
	InstanceID    string          `json:"instance_id"`
	Files         []ports.LogFile `json:"files"`
}

type Entry struct {
	Type       string              `json:"type"`
	Ref        core.TransactionRef `json:"ref"`
	CommitTime time.Time           `json:"commit_time"`
	RowCount   int                 `json:"row_count"`
	Tables     []core.TableRef     `json:"tables,omitempty"`
	Executable bool                `json:"executable"`
	SourceFile string              `json:"source_file"`
}

type BuildOptions struct {
	OutputPath string
	Backend    string
	InstanceID string
}

type BuildResult struct {
	OutputPath   string
	FilesScanned int
	Transactions int
	Duplicates   int
}

// Build scans every file exposed by catalog and atomically publishes a JSONL
// index. Row images are deliberately excluded from Entry.
func Build(ctx context.Context, source ports.ChangeSource, catalog ports.LogCatalog, options BuildOptions) (result BuildResult, err error) {
	if source == nil || catalog == nil {
		return result, errors.New("transaction index: source and catalog are required")
	}
	if strings.TrimSpace(options.OutputPath) == "" {
		return result, errors.New("transaction index: output path is required")
	}
	if _, statErr := os.Lstat(options.OutputPath); statErr == nil {
		return result, fmt.Errorf("transaction index: output %q already exists", options.OutputPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf("transaction index: inspect output: %w", statErr)
	}
	files, err := catalog.ListLogFiles(ctx)
	if err != nil {
		return result, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	dir := filepath.Dir(options.OutputPath)
	if dir == "" {
		dir = "."
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return result, fmt.Errorf("transaction index: create output directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".unredo-index-*.tmp")
	if err != nil {
		return result, fmt.Errorf("transaction index: create temporary file: %w", err)
	}
	tempPath := temp.Name()
	published := false
	defer func() {
		_ = temp.Close()
		if !published {
			_ = os.Remove(tempPath)
		}
	}()
	if err := os.Chmod(tempPath, 0o600); err != nil {
		return result, fmt.Errorf("transaction index: restrict temporary file: %w", err)
	}
	encoder := json.NewEncoder(temp)
	encoder.SetEscapeHTML(false)
	header := Header{
		Type: recordHeader, FormatVersion: FormatVersion, CreatedAt: time.Now().UTC(),
		Backend: options.Backend, InstanceID: options.InstanceID, Files: files,
	}
	if err := encoder.Encode(header); err != nil {
		return result, fmt.Errorf("transaction index: write header: %w", err)
	}
	seen := make(map[string]struct{})
	for _, file := range files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		cursor, _ := json.Marshal(map[string]any{"file": file.Name, "start_pos": uint32(4)})
		iterator, err := source.Scan(ctx, ports.ScanScope{FromCursor: cursor})
		if err != nil {
			return result, fmt.Errorf("transaction index: scan %s: %w", file.Name, err)
		}
		for {
			txn, nextErr := iterator.Next(ctx)
			if errors.Is(nextErr, io.EOF) {
				break
			}
			if nextErr != nil {
				_ = iterator.Close()
				return result, fmt.Errorf("transaction index: scan %s: %w", file.Name, nextErr)
			}
			key := txn.GTID
			if key == "" {
				key = txn.Ref.NativeTransactionID
			}
			if _, exists := seen[key]; exists && key != "" {
				result.Duplicates++
				continue
			}
			if key != "" {
				seen[key] = struct{}{}
			}
			ref := txn.Ref
			ref.Cursor = append(json.RawMessage(nil), cursor...)
			entry := Entry{
				Type: recordTransaction, Ref: ref, CommitTime: txn.CommitTime,
				RowCount: txn.RowCount, Tables: append([]core.TableRef(nil), txn.Tables...),
				Executable: txn.Executable, SourceFile: file.Name,
			}
			if err := encoder.Encode(entry); err != nil {
				_ = iterator.Close()
				return result, fmt.Errorf("transaction index: write %s: %w", key, err)
			}
			result.Transactions++
		}
		if err := iterator.Close(); err != nil {
			return result, fmt.Errorf("transaction index: close %s: %w", file.Name, err)
		}
		result.FilesScanned++
	}
	if err := temp.Sync(); err != nil {
		return result, fmt.Errorf("transaction index: sync: %w", err)
	}
	if err := temp.Close(); err != nil {
		return result, fmt.Errorf("transaction index: close: %w", err)
	}
	if _, statErr := os.Lstat(options.OutputPath); statErr == nil {
		return result, fmt.Errorf("transaction index: output %q appeared while building", options.OutputPath)
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return result, fmt.Errorf("transaction index: recheck output: %w", statErr)
	}
	if err := os.Rename(tempPath, options.OutputPath); err != nil {
		return result, fmt.Errorf("transaction index: publish: %w", err)
	}
	published = true
	result.OutputPath = options.OutputPath
	return result, nil
}

type Filter struct {
	GTID     string
	Database string
	Table    string
	Since    time.Time
	Until    time.Time
	Limit    int
}

// Query streams matching entries without loading the complete index.
func Query(ctx context.Context, path string, filter Filter, yield func(Entry) error) (Header, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return Header{}, 0, fmt.Errorf("transaction index: open %q: %w", path, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var header Header
	if err := decoder.Decode(&header); err != nil {
		return Header{}, 0, fmt.Errorf("transaction index: read header: %w", err)
	}
	if header.Type != recordHeader || header.FormatVersion != FormatVersion {
		return Header{}, 0, fmt.Errorf("transaction index: unsupported header type=%q format_version=%d", header.Type, header.FormatVersion)
	}
	matched := 0
	for {
		if err := ctx.Err(); err != nil {
			return header, matched, err
		}
		var entry Entry
		err := decoder.Decode(&entry)
		if errors.Is(err, io.EOF) {
			return header, matched, nil
		}
		if err != nil {
			return header, matched, fmt.Errorf("transaction index: decode entry: %w", err)
		}
		if entry.Type != recordTransaction {
			return header, matched, fmt.Errorf("transaction index: unexpected record type %q", entry.Type)
		}
		if !matches(entry, filter) {
			continue
		}
		if err := yield(entry); err != nil {
			return header, matched, err
		}
		matched++
		if filter.Limit > 0 && matched >= filter.Limit {
			return header, matched, nil
		}
	}
}

func matches(entry Entry, filter Filter) bool {
	if filter.GTID != "" && entry.Ref.NativeTransactionID != filter.GTID {
		return false
	}
	if !filter.Since.IsZero() && entry.CommitTime.Before(filter.Since) {
		return false
	}
	if !filter.Until.IsZero() && entry.CommitTime.After(filter.Until) {
		return false
	}
	if filter.Database == "" && filter.Table == "" {
		return true
	}
	for _, table := range entry.Tables {
		if filter.Database != "" && table.Schema != filter.Database {
			continue
		}
		if filter.Table != "" && table.Name != filter.Table && table.String() != filter.Table {
			continue
		}
		return true
	}
	return false
}
