//go:build integration
// +build integration

package integration_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oklog/ulid/v2"

	mysqlbackend "github.com/girimi/unredo/internal/backends/mysql"
	"github.com/girimi/unredo/internal/config"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/planner"
	"github.com/girimi/unredo/internal/ports"
)

// TestLargeTransactionMetrics is intentionally opt-in because it writes large
// real transactions. Run it with:
//
//	UNREDO_RUN_LARGE_BENCHMARK=1 UNREDO_BENCH_ROWS=1000,10000 \
//	  go test -tags=integration -run TestLargeTransactionMetrics -v ./tests/integration
func TestLargeTransactionMetrics(t *testing.T) {
	if os.Getenv("UNREDO_RUN_LARGE_BENCHMARK") != "1" {
		t.Skip("set UNREDO_RUN_LARGE_BENCHMARK=1 to run destructive local benchmark fixtures")
	}
	mysqlBin := findMySQLBin(t)
	rootConn := openRoot(t, mysqlBin)
	defer rootConn.Close()
	ensureFullRowMetadata(t, rootConn)
	execConn := openExecutor(t, mysqlBin)
	defer execConn.Close()

	if _, err := rootConn.Exec(`CREATE TABLE IF NOT EXISTS unredo_shop.unredo_bench_rows (
		id BIGINT NOT NULL AUTO_INCREMENT,
		payload VARBINARY(8192) NULL,
		note TEXT NULL,
		PRIMARY KEY (id)
	) ENGINE=InnoDB`); err != nil {
		t.Fatal(err)
	}
	if _, err := rootConn.Exec("TRUNCATE TABLE unredo_shop.unredo_bench_rows"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = rootConn.Exec("DROP TABLE IF EXISTS unredo_shop.unredo_bench_rows") })
	payloadBytes := benchmarkPayloadBytes(t)

	for _, rows := range benchmarkRowCounts(t) {
		t.Run(strconv.Itoa(rows)+"-rows", func(t *testing.T) {
			if _, err := rootConn.Exec("TRUNCATE TABLE unredo_shop.unredo_bench_rows"); err != nil {
				t.Fatal(err)
			}
			file, position, err := readCurrentBinlogPosition(rootConn)
			if err != nil {
				t.Fatal(err)
			}
			seedStarted := time.Now()
			if err := seedBenchmarkRows(execConn, rows, payloadBytes); err != nil {
				t.Fatal(err)
			}
			seedDuration := time.Since(seedStarted)
			gtid, err := latestGTID(rootConn)
			if err != nil {
				t.Fatal(err)
			}

			profile := config.Profile{
				Backend: "mysql",
				Source: config.Source{
					Mode: config.SourceReplication, Address: "127.0.0.1:3306",
					User: readerUser, PasswordEnv: "UNREDO_READER_PASSWORD",
					ServerID: uint32(700000 + rows%100000),
				},
				Target: config.Target{Address: "127.0.0.1:3306", User: executorUser, PasswordEnv: "UNREDO_EXECUTOR_PASSWORD"},
				Policy: config.Policy{
					RequireGTID: true, RequireFullRowImage: true, RequirePrimaryKey: true,
					MaxTransactionRows: rows + 1, MaxTransactionBytes: 1 << 30,
					MaxPlanBytes: 2 << 30, MaxActionDepth: 20, LockWaitTimeout: 5 * time.Second,
				},
			}
			backend, err := mysqlbackend.NewBackend(&profile)
			if err != nil {
				t.Fatal(err)
			}
			cursor, _ := json.Marshal(map[string]interface{}{"file": file, "start_pos": position})
			ref := core.TransactionRef{
				Backend: "mysql", InstanceID: backend.(interface{ InstanceID() string }).InstanceID(),
				NativeTransactionID: gtid, Cursor: cursor,
			}

			runtime.GC()
			var before runtime.MemStats
			runtime.ReadMemStats(&before)
			stopSampling, peakHeap := samplePeakHeap()
			decodeStarted := time.Now()
			txn, err := backend.Find(context.Background(), ref)
			decodeDuration := time.Since(decodeStarted)
			if err != nil {
				stopSampling()
				t.Fatal(err)
			}
			if !txn.Executable || txn.RowCount != rows || len(txn.Rows) != rows {
				stopSampling()
				t.Fatalf("decoded transaction is incomplete: executable=%t row_count=%d retained=%d reasons=%v", txn.Executable, txn.RowCount, len(txn.Rows), txn.Reasons)
			}

			planStarted := time.Now()
			plan, err := planner.Build(txn, planner.ModeRevert, planner.Deps{
				SchemaFor: func(table core.TableRef) (core.TableSchema, error) {
					return backend.InspectTable(context.Background(), table)
				},
				FingerprintFor: func(table core.TableRef) (core.SchemaFingerprint, error) {
					return backend.Fingerprint(context.Background(), table)
				},
				ToolVersion: "benchmark",
			})
			if err != nil {
				stopSampling()
				t.Fatal(err)
			}
			planPath := filepath.Join(t.TempDir(), "plan.json")
			if err := planner.WriteFile(plan, planPath); err != nil {
				stopSampling()
				t.Fatal(err)
			}
			planDuration := time.Since(planStarted)
			info, err := os.Stat(planPath)
			if err != nil {
				t.Fatal(err)
			}
			if len(plan.Operations) != rows {
				t.Fatalf("plan has %d operations, want %d", len(plan.Operations), rows)
			}
			checkStarted := time.Now()
			check, err := backend.Check(context.Background(), *plan.ToPorts())
			checkDuration := time.Since(checkStarted)
			if err != nil || check.Status != "READY" {
				stopSampling()
				t.Fatalf("benchmark plan is not ready: status=%v err=%v", check, err)
			}
			actionID := ulid.Make()
			applyStarted := time.Now()
			applyResult, err := backend.Apply(context.Background(), *plan.ToPorts(), ports.ApplyRequest{
				ActionID: actionID.String(), OperatorName: "benchmark", Confirm: planner.ShortDigest(plan.Digest),
			})
			applyDuration := time.Since(applyStarted)
			if err != nil {
				stopSampling()
				t.Fatal(err)
			}
			if applyResult.AffectedRows != rows || applyResult.CompensatingGTID == "" {
				stopSampling()
				t.Fatalf("benchmark apply result is incomplete: %+v", applyResult)
			}
			t.Cleanup(func() {
				_, _ = rootConn.Exec("DELETE FROM unredo_meta.action_markers WHERE action_id = ?", actionID[:])
			})
			peak := stopSampling()
			if sampled := peakHeap.Load(); sampled > peak {
				peak = sampled
			}
			var after runtime.MemStats
			runtime.ReadMemStats(&after)
			metrics := map[string]interface{}{
				"rows": rows, "payload_bytes_per_row": payloadBytes,
				"seed_ms": seedDuration.Milliseconds(), "decode_ms": decodeDuration.Milliseconds(),
				"plan_ms": planDuration.Milliseconds(), "check_ms": checkDuration.Milliseconds(),
				"apply_ms": applyDuration.Milliseconds(), "plan_bytes": info.Size(),
				"total_alloc_bytes": after.TotalAlloc - before.TotalAlloc,
				"peak_heap_bytes":   peak,
			}
			raw, _ := json.Marshal(metrics)
			t.Log(string(raw))
		})
	}
}

func benchmarkPayloadBytes(t *testing.T) int {
	t.Helper()
	value := os.Getenv("UNREDO_BENCH_PAYLOAD_BYTES")
	if value == "" {
		return 512
	}
	bytes, err := strconv.Atoi(value)
	if err != nil || bytes < 0 || bytes > 8192 {
		t.Fatalf("invalid UNREDO_BENCH_PAYLOAD_BYTES %q", value)
	}
	return bytes
}

func benchmarkRowCounts(t *testing.T) []int {
	t.Helper()
	value := os.Getenv("UNREDO_BENCH_ROWS")
	if value == "" {
		value = "1000,10000"
	}
	var out []int
	for _, part := range strings.Split(value, ",") {
		rows, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || rows <= 0 || rows > 1_000_000 {
			t.Fatalf("invalid UNREDO_BENCH_ROWS value %q", part)
		}
		out = append(out, rows)
	}
	return out
}

func seedBenchmarkRows(db *sql.DB, rows, payloadBytes int) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	statement, err := tx.Prepare("INSERT INTO unredo_shop.unredo_bench_rows (payload, note) VALUES (?, ?)")
	if err != nil {
		return err
	}
	defer statement.Close()
	payload := make([]byte, payloadBytes)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	for i := 0; i < rows; i++ {
		if _, err := statement.Exec(payload, fmt.Sprintf("benchmark-row-%d", i)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func readCurrentBinlogPosition(db *sql.DB) (string, uint32, error) {
	var file string
	var position uint32
	var doDB, ignoreDB, gtidSet sql.NullString
	scan := func(statement string) error {
		return db.QueryRow(statement).Scan(&file, &position, &doDB, &ignoreDB, &gtidSet)
	}
	if err := scan("SHOW BINARY LOG STATUS"); err != nil {
		if legacyErr := scan("SHOW MASTER STATUS"); legacyErr != nil {
			return "", 0, fmt.Errorf("binary log status: %v; legacy fallback: %w", err, legacyErr)
		}
	}
	return file, position, nil
}

func samplePeakHeap() (func() uint64, *atomic.Uint64) {
	var peak atomic.Uint64
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			var stats runtime.MemStats
			runtime.ReadMemStats(&stats)
			for current := peak.Load(); stats.HeapAlloc > current && !peak.CompareAndSwap(current, stats.HeapAlloc); current = peak.Load() {
			}
			select {
			case <-stop:
				return
			case <-ticker.C:
			}
		}
	}()
	var stopped atomic.Bool
	return func() uint64 {
		if stopped.CompareAndSwap(false, true) {
			close(stop)
			<-done
		}
		return peak.Load()
	}, &peak
}
