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

// FileChanges compares the archive snapshot stored in an index with the
// current catalog. Changed means size or modification time differs.
type FileChanges struct {
	Added     []ports.LogFile `json:"added,omitempty"`
	Changed   []ports.LogFile `json:"changed,omitempty"`
	Removed   []ports.LogFile `json:"removed,omitempty"`
	Unchanged []ports.LogFile `json:"unchanged,omitempty"`
}

func (c FileChanges) Current() bool {
	return len(c.Added) == 0 && len(c.Changed) == 0 && len(c.Removed) == 0
}

// CompareFiles returns a deterministic filename-sorted snapshot diff.
func CompareFiles(indexed, current []ports.LogFile) FileChanges {
	oldByName := make(map[string]ports.LogFile, len(indexed))
	newByName := make(map[string]ports.LogFile, len(current))
	for _, file := range indexed {
		oldByName[file.Name] = file
	}
	for _, file := range current {
		newByName[file.Name] = file
	}
	var changes FileChanges
	for name, file := range newByName {
		old, exists := oldByName[name]
		switch {
		case !exists:
			changes.Added = append(changes.Added, file)
		case old.Size != file.Size || !old.ModifiedAt.Equal(file.ModifiedAt):
			changes.Changed = append(changes.Changed, file)
		default:
			changes.Unchanged = append(changes.Unchanged, file)
		}
	}
	for name, file := range oldByName {
		if _, exists := newByName[name]; !exists {
			changes.Removed = append(changes.Removed, file)
		}
	}
	sortFiles := func(files []ports.LogFile) {
		sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	}
	sortFiles(changes.Added)
	sortFiles(changes.Changed)
	sortFiles(changes.Removed)
	sortFiles(changes.Unchanged)
	return changes
}

// ReadHeader reads and validates only the index snapshot header.
func ReadHeader(path string) (Header, error) {
	file, err := os.Open(path)
	if err != nil {
		return Header{}, fmt.Errorf("transaction index: open %q: %w", path, err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var header Header
	if err := decoder.Decode(&header); err != nil {
		return Header{}, fmt.Errorf("transaction index: read header: %w", err)
	}
	if header.Type != recordHeader || header.FormatVersion != FormatVersion {
		return Header{}, fmt.Errorf("transaction index: unsupported header type=%q format_version=%d", header.Type, header.FormatVersion)
	}
	return header, nil
}

type UpdateOptions struct {
	ExistingPath string
	OutputPath   string
	Backend      string
	InstanceID   string
}

type UpdateResult struct {
	OutputPath          string
	Current             bool
	CopiedTransactions  int
	ScannedTransactions int
	Duplicates          int
	FilesAdded          int
	FilesRescanned      int
}

// Update creates a new index from an old snapshot without rescanning immutable
// files. It accepts only append-shaped archive changes: the previous last file
// may grow and lexically later files may be added. Any destructive or reordered
// change requires a full Build.
func Update(ctx context.Context, source ports.ChangeSource, catalog ports.LogCatalog, options UpdateOptions) (result UpdateResult, err error) {
	if source == nil || catalog == nil {
		return result, errors.New("transaction index: source and catalog are required")
	}
	if strings.TrimSpace(options.ExistingPath) == "" || strings.TrimSpace(options.OutputPath) == "" {
		return result, errors.New("transaction index: existing and output paths are required")
	}
	if err := requireAbsent(options.OutputPath); err != nil {
		return result, err
	}
	header, err := ReadHeader(options.ExistingPath)
	if err != nil {
		return result, err
	}
	if err := validateFileSnapshot(header.Files); err != nil {
		return result, err
	}
	if options.Backend != "" && header.Backend != options.Backend {
		return result, fmt.Errorf("transaction index: backend mismatch index=%q profile=%q", header.Backend, options.Backend)
	}
	if options.InstanceID != "" && header.InstanceID != options.InstanceID {
		return result, fmt.Errorf("transaction index: instance mismatch index=%q profile=%q", header.InstanceID, options.InstanceID)
	}
	currentFiles, err := catalog.ListLogFiles(ctx)
	if err != nil {
		return result, err
	}
	sort.Slice(currentFiles, func(i, j int) bool { return currentFiles[i].Name < currentFiles[j].Name })
	changes := CompareFiles(header.Files, currentFiles)
	if changes.Current() {
		result.Current = true
		return result, nil
	}
	rescan, err := safeAppendPlan(header.Files, changes)
	if err != nil {
		return result, err
	}
	result.FilesAdded = len(changes.Added)
	result.FilesRescanned = len(rescan)

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
	newHeader := header
	newHeader.CreatedAt = time.Now().UTC()
	newHeader.Files = currentFiles
	if err := encoder.Encode(newHeader); err != nil {
		return result, fmt.Errorf("transaction index: write updated header: %w", err)
	}

	seen := make(map[string]struct{})
	indexedFiles := make(map[string]struct{}, len(header.Files))
	for _, file := range header.Files {
		indexedFiles[file.Name] = struct{}{}
	}
	old, err := os.Open(options.ExistingPath)
	if err != nil {
		return result, fmt.Errorf("transaction index: reopen existing index: %w", err)
	}
	decoder := json.NewDecoder(old)
	var discarded Header
	if err := decoder.Decode(&discarded); err != nil {
		_ = old.Close()
		return result, fmt.Errorf("transaction index: reread header: %w", err)
	}
	for {
		if err := ctx.Err(); err != nil {
			_ = old.Close()
			return result, err
		}
		var entry Entry
		decodeErr := decoder.Decode(&entry)
		if errors.Is(decodeErr, io.EOF) {
			break
		}
		if decodeErr != nil {
			_ = old.Close()
			return result, fmt.Errorf("transaction index: decode existing entry: %w", decodeErr)
		}
		if entry.Type != recordTransaction {
			_ = old.Close()
			return result, fmt.Errorf("transaction index: unexpected record type %q", entry.Type)
		}
		if _, known := indexedFiles[entry.SourceFile]; !known {
			_ = old.Close()
			return result, fmt.Errorf("transaction index: entry references unknown source file %q", entry.SourceFile)
		}
		if entry.Ref.Backend != header.Backend || entry.Ref.InstanceID != header.InstanceID {
			_ = old.Close()
			return result, fmt.Errorf("transaction index: entry identity does not match index header")
		}
		if _, replace := rescan[entry.SourceFile]; replace {
			continue
		}
		key := entry.Ref.NativeTransactionID
		if key != "" {
			if _, duplicate := seen[key]; duplicate {
				result.Duplicates++
				continue
			}
			seen[key] = struct{}{}
		}
		if err := encoder.Encode(entry); err != nil {
			_ = old.Close()
			return result, fmt.Errorf("transaction index: copy %s: %w", key, err)
		}
		result.CopiedTransactions++
	}
	if err := old.Close(); err != nil {
		return result, fmt.Errorf("transaction index: close existing index: %w", err)
	}
	for _, file := range currentFiles {
		if _, shouldScan := rescan[file.Name]; !shouldScan {
			continue
		}
		count, duplicates, err := appendFileEntries(ctx, encoder, source, file, seen)
		if err != nil {
			return result, err
		}
		result.ScannedTransactions += count
		result.Duplicates += duplicates
	}
	if err := temp.Sync(); err != nil {
		return result, fmt.Errorf("transaction index: sync update: %w", err)
	}
	if err := temp.Close(); err != nil {
		return result, fmt.Errorf("transaction index: close update: %w", err)
	}
	if err := requireAbsent(options.OutputPath); err != nil {
		return result, err
	}
	if err := os.Rename(tempPath, options.OutputPath); err != nil {
		return result, fmt.Errorf("transaction index: publish update: %w", err)
	}
	published = true
	result.OutputPath = options.OutputPath
	return result, nil
}

func safeAppendPlan(indexed []ports.LogFile, changes FileChanges) (map[string]struct{}, error) {
	if len(indexed) == 0 {
		return nil, errors.New("transaction index: empty file snapshot; run index build")
	}
	if len(changes.Removed) > 0 {
		return nil, fmt.Errorf("transaction index: archived files were removed; run a full index build")
	}
	last := indexed[len(indexed)-1]
	rescan := make(map[string]struct{}, len(changes.Added)+len(changes.Changed))
	for _, file := range changes.Changed {
		if file.Name != last.Name || file.Size <= last.Size {
			return nil, fmt.Errorf("transaction index: archived file %q was rewritten; run a full index build", file.Name)
		}
		rescan[file.Name] = struct{}{}
	}
	for _, file := range changes.Added {
		if file.Name <= last.Name {
			return nil, fmt.Errorf("transaction index: out-of-order archive file %q appeared; run a full index build", file.Name)
		}
		rescan[file.Name] = struct{}{}
	}
	return rescan, nil
}

func validateFileSnapshot(files []ports.LogFile) error {
	if len(files) == 0 {
		return errors.New("transaction index: empty file snapshot; run index build")
	}
	for i, file := range files {
		if strings.TrimSpace(file.Name) == "" {
			return errors.New("transaction index: file snapshot contains an empty name; run index build")
		}
		if i > 0 && files[i-1].Name >= file.Name {
			return errors.New("transaction index: file snapshot is not strictly ordered; run index build")
		}
	}
	return nil
}

func appendFileEntries(ctx context.Context, encoder *json.Encoder, source ports.ChangeSource, file ports.LogFile, seen map[string]struct{}) (count, duplicates int, err error) {
	cursor, _ := json.Marshal(map[string]any{"file": file.Name, "start_pos": uint32(4)})
	iterator, err := source.Scan(ctx, ports.ScanScope{FromCursor: cursor})
	if err != nil {
		return 0, 0, fmt.Errorf("transaction index: scan %s: %w", file.Name, err)
	}
	defer iterator.Close()
	for {
		txn, nextErr := iterator.Next(ctx)
		if errors.Is(nextErr, io.EOF) {
			return count, duplicates, nil
		}
		if nextErr != nil {
			return count, duplicates, fmt.Errorf("transaction index: scan %s: %w", file.Name, nextErr)
		}
		key := txn.GTID
		if key == "" {
			key = txn.Ref.NativeTransactionID
		}
		if key != "" {
			if _, duplicate := seen[key]; duplicate {
				duplicates++
				continue
			}
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
			return count, duplicates, fmt.Errorf("transaction index: write %s: %w", key, err)
		}
		count++
	}
}

func requireAbsent(path string) error {
	if _, err := os.Lstat(path); err == nil {
		return fmt.Errorf("transaction index: output %q already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("transaction index: inspect output: %w", err)
	}
	return nil
}
