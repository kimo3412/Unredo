// Package doctor implements the environment checks documented in
// DESIGN.md §6.2. The CLI exposes them as `unredo doctor`.
package doctor

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/config"
)

// Severity of a single check.
type Severity string

const (
	SeverityOK    Severity = "OK"
	SeverityWarn  Severity = "WARN"
	SeverityError Severity = "ERROR"
)

// Check is one named environment check.
type Check struct {
	Name     string   `json:"name"`
	Severity Severity `json:"severity"`
	Message  string   `json:"message"`
}

// Report is the doctor output, also used as `unredo doctor --format=json`.
type Report struct {
	ServerUUID  string    `json:"server_uuid"`
	GeneratedAt time.Time `json:"generated_at"`
	Checks      []Check   `json:"checks"`
}

// Deps lets the CLI pass a context and a max duration.
type Deps struct {
	Context context.Context
	Timeout time.Duration
}

// Run executes every check. The result is suitable for both human output
// and machine parsing. It does not abort early on failures; instead it
// collects everything so the operator sees the full picture.
func Run(deps *Deps, sourceDSN, targetDSN, serverUUID string, serverID uint32, sourceMode config.SourceMode, localBinlogDir string, policy config.Policy) (*Report, error) {
	if deps == nil {
		deps = &Deps{Context: context.Background(), Timeout: 30 * time.Second}
	}
	ctx, cancel := context.WithTimeout(deps.Context, deps.Timeout)
	defer cancel()

	r := &Report{
		ServerUUID:  serverUUID,
		GeneratedAt: time.Now().UTC(),
	}

	if err := openAndCheck(ctx, sourceDSN, "source", r, func(ctx context.Context, db *sql.DB) error {
		return checkSource(ctx, db, serverID, sourceMode, localBinlogDir, policy, r)
	}); err != nil {
		r.Checks = append(r.Checks, Check{
			Name:     "source.connect",
			Severity: SeverityError,
			Message:  err.Error(),
		})
	}

	if targetDSN != sourceDSN {
		if err := openAndCheck(ctx, targetDSN, "target", r, func(ctx context.Context, db *sql.DB) error {
			return checkTarget(ctx, db, policy, r)
		}); err != nil {
			r.Checks = append(r.Checks, Check{
				Name:     "target.connect",
				Severity: SeverityError,
				Message:  err.Error(),
			})
		}
	} else {
		r.Checks = append(r.Checks, Check{
			Name:     "target.connect",
			Severity: SeverityOK,
			Message:  "shares source connection",
		})
	}

	return r, nil
}

func openAndCheck(ctx context.Context, dsn, label string, r *Report, fn func(context.Context, *sql.DB) error) error {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return fmt.Errorf("%s: open: %w", label, err)
	}
	defer db.Close()
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fmt.Errorf("%s: ping: %w", label, err)
	}
	r.Checks = append(r.Checks, Check{
		Name:     label + ".connect",
		Severity: SeverityOK,
		Message:  "ok",
	})
	return fn(ctx, db)
}

func checkSource(ctx context.Context, db *sql.DB, serverID uint32, sourceMode config.SourceMode, localBinlogDir string, policy config.Policy, r *Report) error {
	checkVersion(ctx, db, r)
	if sourceMode == config.SourceLocalFile {
		checkLocalBinlogDirectory(localBinlogDir, r)
		return nil
	}
	checkVariable(ctx, db, r, "log_bin", "ON", true)
	checkVariable(ctx, db, r, "binlog_format", "ROW", true)
	checkVariable(ctx, db, r, "binlog_row_image", "FULL", true)
	checkVariable(ctx, db, r, "binlog_row_metadata", "FULL", true)
	checkVariable(ctx, db, r, "gtid_mode", "ON", policy.RequireGTID)
	checkVariable(ctx, db, r, "enforce_gtid_consistency", "ON", policy.RequireGTID)
	checkReplPrivs(ctx, db, r)
	checkServerID(ctx, db, serverID, r)
	return nil
}

func checkLocalBinlogDirectory(path string, r *Report) {
	info, err := os.Stat(path)
	if err != nil {
		r.Checks = append(r.Checks, Check{Name: "mysql.local_binlog_path", Severity: SeverityError, Message: err.Error()})
		return
	}
	if !info.IsDir() {
		r.Checks = append(r.Checks, Check{Name: "mysql.local_binlog_path", Severity: SeverityError, Message: "must be a directory"})
		return
	}
	r.Checks = append(r.Checks, Check{Name: "mysql.local_binlog_path", Severity: SeverityOK, Message: path})
}

func checkServerID(ctx context.Context, db *sql.DB, serverID uint32, r *Report) {
	if serverID == 0 {
		r.Checks = append(r.Checks, Check{Name: "mysql.replication_server_id", Severity: SeverityError, Message: "must be a non-zero uint32"})
		return
	}
	var sourceID uint32
	if err := db.QueryRowContext(ctx, "SELECT @@global.server_id").Scan(&sourceID); err == nil && sourceID == serverID {
		r.Checks = append(r.Checks, Check{Name: "mysql.replication_server_id", Severity: SeverityError, Message: fmt.Sprintf("%d conflicts with source @@server_id", serverID)})
		return
	}
	rows, err := db.QueryContext(ctx, "SHOW REPLICAS")
	if err != nil {
		r.Checks = append(r.Checks, Check{Name: "mysql.replication_server_id", Severity: SeverityWarn, Message: fmt.Sprintf("%d is valid, but visible replica IDs could not be checked: %v", serverID, err)})
		return
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil || len(cols) == 0 {
		r.Checks = append(r.Checks, Check{Name: "mysql.replication_server_id", Severity: SeverityWarn, Message: fmt.Sprintf("%d is valid; replica list unavailable", serverID)})
		return
	}
	for rows.Next() {
		raw := make([]sql.RawBytes, len(cols))
		dest := make([]interface{}, len(cols))
		for i := range raw {
			dest[i] = &raw[i]
		}
		if err := rows.Scan(dest...); err != nil {
			continue
		}
		visible, _ := strconv.ParseUint(string(raw[0]), 10, 32)
		if uint32(visible) == serverID {
			r.Checks = append(r.Checks, Check{Name: "mysql.replication_server_id", Severity: SeverityError, Message: fmt.Sprintf("%d conflicts with a visible replica", serverID)})
			return
		}
	}
	r.Checks = append(r.Checks, Check{Name: "mysql.replication_server_id", Severity: SeverityOK, Message: fmt.Sprintf("%d; no conflict among visible replicas (best effort)", serverID)})
}

func checkTarget(ctx context.Context, db *sql.DB, _ config.Policy, r *Report) error {
	var count int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
		WHERE table_schema = 'unredo_meta' AND table_name = 'action_markers'`).Scan(&count)
	if err != nil {
		r.Checks = append(r.Checks, Check{Name: "target.meta_schema", Severity: SeverityError, Message: "could not inspect unredo_meta: " + err.Error()})
		return nil
	}
	if count != 1 {
		r.Checks = append(r.Checks, Check{Name: "target.meta_schema", Severity: SeverityError, Message: "unredo_meta.action_markers is missing; run unredo init --apply-meta"})
		return nil
	}
	r.Checks = append(r.Checks, Check{Name: "target.meta_schema", Severity: SeverityOK, Message: "unredo_meta.action_markers exists"})
	return nil
}

func checkVersion(ctx context.Context, db *sql.DB, r *Report) {
	var v string
	if err := db.QueryRowContext(ctx, "SELECT VERSION()").Scan(&v); err != nil {
		r.Checks = append(r.Checks, Check{
			Name:     "mysql.version",
			Severity: SeverityError,
			Message:  "could not read version: " + err.Error(),
		})
		return
	}
	// We support MySQL 8.x; warn on 8.0/8.4 and error otherwise.
	sev := SeverityOK
	msg := "ok"
	if len(v) < 2 || v[:2] != "8." {
		sev = SeverityError
		msg = "unsupported major version, need 8.x: " + v
	}
	r.Checks = append(r.Checks, Check{Name: "mysql.version", Severity: sev, Message: msg})
}

func checkVariable(ctx context.Context, db *sql.DB, r *Report, name, want string, required bool) {
	row := db.QueryRowContext(ctx, "SELECT @@"+name)
	var got sql.NullString
	if err := row.Scan(&got); err != nil {
		severity := SeverityError
		if !required {
			severity = SeverityWarn
		}
		r.Checks = append(r.Checks, Check{
			Name: "mysql." + name, Severity: severity,
			Message: "could not read: " + err.Error(),
		})
		return
	}
	val := got.String
	ok := false
	if name == "log_bin" {
		ok = val == "1" || val == "ON"
	} else {
		ok = val == want
	}
	if ok {
		r.Checks = append(r.Checks, Check{
			Name: "mysql." + name, Severity: SeverityOK,
			Message: val,
		})
		return
	}
	severity := SeverityError
	if !required {
		severity = SeverityWarn
	}
	r.Checks = append(r.Checks, Check{
		Name: "mysql." + name, Severity: severity,
		Message: fmt.Sprintf("got %q, want %q", val, want),
	})
}

func checkReplPrivs(ctx context.Context, db *sql.DB, r *Report) {
	rows, err := db.QueryContext(ctx, "SHOW GRANTS FOR CURRENT_USER()")
	if err != nil {
		r.Checks = append(r.Checks, Check{
			Name: "mysql.repl_privs", Severity: SeverityWarn,
			Message: "could not read grants: " + err.Error(),
		})
		return
	}
	defer rows.Close()
	hasRepl := false
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			continue
		}
		if contains(g, "REPLICATION SLAVE") || contains(g, "REPLICATION CLIENT") {
			hasRepl = true
			break
		}
	}
	if hasRepl {
		r.Checks = append(r.Checks, Check{
			Name: "mysql.repl_privs", Severity: SeverityOK,
			Message: "REPLICATION SLAVE / CLIENT present",
		})
	} else {
		r.Checks = append(r.Checks, Check{
			Name: "mysql.repl_privs", Severity: SeverityWarn,
			Message: "reader is missing REPLICATION SLAVE / CLIENT",
		})
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
