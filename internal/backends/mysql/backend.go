// Package mysql is the MySQL 8 backend for Unredo.
//
// It exposes a single ports.Backend assembled from the source, schema,
// doctor, value, and (post-M2) executor sub-packages. The init function
// registers the factory with the registry.
package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/girimi/unredo/internal/backends/mysql/doctor"
	"github.com/girimi/unredo/internal/backends/mysql/schema"
	mysqlsource "github.com/girimi/unredo/internal/backends/mysql/source"
	"github.com/girimi/unredo/internal/config"
	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/executor"
	"github.com/girimi/unredo/internal/ports"
	"github.com/girimi/unredo/internal/registry"
)

func init() {
	registry.Register("mysql", NewBackend)
}

// Backend is the MySQL adapter. It implements ports.Backend by composing
// the source, schema, and (later) executor sub-packages.
type Backend struct {
	instanceID string
	sourceDSN  string
	targetDSN  string
	policy     config.Policy
	inspector  ports.SchemaInspector
	source     *mysqlsource.Source
}

// NewBackend builds a Backend from a profile. The source connection is
// validated with a ping but no binlog connection is opened until needed.
func NewBackend(p *config.Profile) (ports.Backend, error) {
	if p == nil {
		return nil, fmt.Errorf("mysql: nil profile")
	}
	if p.Backend != "mysql" {
		return nil, fmt.Errorf("mysql: profile backend is %q, not mysql", p.Backend)
	}
	if p.Source.Mode != config.SourceReplication {
		return nil, fmt.Errorf("mysql: source.mode %q not yet supported in M0", p.Source.Mode)
	}
	srcDSN, err := buildDSN(p.Source.Address, p.Source.User, p.Source.PasswordEnv, p.Source.ServerID)
	if err != nil {
		return nil, err
	}
	tgtDSN, err := buildDSN(p.Target.Address, p.Target.User, p.Target.PasswordEnv, 0)
	if err != nil {
		return nil, err
	}
	instanceID, err := fetchServerUUID(srcDSN)
	if err != nil {
		return nil, fmt.Errorf("mysql: detect server uuid: %w", err)
	}
	insp := schema.NewInspector(srcDSN)
	return &Backend{
		instanceID: instanceID,
		sourceDSN:  srcDSN,
		targetDSN:  tgtDSN,
		policy:     p.Policy,
		inspector:  insp,
		source:     mysqlsource.New(srcDSN, instanceID, p.Source.ServerID),
	}, nil
}

// Name implements ports.Backend.
func (b *Backend) Name() string { return "mysql" }

// InstanceID returns the MySQL @@server_uuid, used to validate plan refs.
func (b *Backend) InstanceID() string { return b.instanceID }

// Capabilities implements ports.ChangeSource.
// MySQL 8 with ROW/FULL/GTID is the assumed M0 configuration. The flags
// reflect what the planner can rely on; schema-at-event-time is reported
// as true only when we read schema with the source conn, which we do.
func (b *Backend) Capabilities(_ context.Context) (core.BackendCapabilities, error) {
	return core.BackendCapabilities{
		FullBeforeImage:       true,
		FullAfterImage:        true,
		StableTransactionID:   true, // GTID
		TransactionBoundaries: true, // XID + GTID
		AtomicActionMarker:    true, // unredo_meta.action_markers (M2; reported optimistically in M0)
		SchemaAtEventTime:     true, // we read information_schema alongside the stream
		SupportsReapply:       true,
	}, nil
}

// InspectTable delegates to the schema inspector.
func (b *Backend) InspectTable(ctx context.Context, t core.TableRef) (core.TableSchema, error) {
	return b.inspector.InspectTable(ctx, t)
}

// Fingerprint delegates to the schema inspector.
func (b *Backend) Fingerprint(ctx context.Context, t core.TableRef) (core.SchemaFingerprint, error) {
	return b.inspector.Fingerprint(ctx, t)
}

// Scan opens a change-source iterator.
func (b *Backend) Scan(ctx context.Context, scope ports.ScanScope) (ports.TransactionIterator, error) {
	return b.source.Scan(ctx, scope)
}

// Find loads one transaction by ref.
func (b *Backend) Find(ctx context.Context, ref core.TransactionRef) (*core.Transaction, error) {
	return b.source.Find(ctx, ref)
}

// Check verifies that the current state of the target database still
// matches the plan's expect images. The executor itself lives in
// internal/executor; this method adapts the result into ports.Conflict.
func (b *Backend) Check(ctx context.Context, plan ports.Plan) ([]ports.Conflict, error) {
	reader := NewCheckReader(b.targetDSN, b.instanceID)
	result, err := executor.Check(ctx, &plan, reader)
	if err != nil {
		return nil, err
	}
	conflicts := make([]ports.Conflict, 0, len(result.Conflicts))
	for _, c := range result.Conflicts {
		conflicts = append(conflicts, ports.Conflict{
			OperationSequence: c.OperationSequence,
			Table:             c.Table,
			Kind:              string(c.Kind),
			Message:           c.Message,
		})
	}
	return conflicts, nil
}

func (b *Backend) Apply(_ context.Context, _ ports.Plan, _ string) (ports.ExecutionResult, error) {
	return ports.ExecutionResult{}, fmt.Errorf("mysql: plan apply is part of M2 follow-up; only check is wired so far")
}

// RunDoctor exposes the doctor checks for the CLI.
func (b *Backend) RunDoctor(_ context.Context, d *doctor.Deps) (*doctor.Report, error) {
	return doctor.Run(d, b.sourceDSN, b.targetDSN, b.instanceID, b.policy)
}

func buildDSN(addr, user, passwordEnv string, serverID uint32) (string, error) {
	pw, err := config.ResolvePassword(passwordEnv)
	if err != nil {
		return "", fmt.Errorf("mysql: %w", err)
	}
	cfg := mysql.NewConfig()
	cfg.User = user
	cfg.Passwd = pw
	cfg.Net = "tcp"
	cfg.Addr = addr
	cfg.ParseTime = false
	cfg.Loc = time.UTC
	cfg.InterpolateParams = false
	// We deliberately do NOT set session time_zone here. The binlog
	// stores TIMESTAMP text in the writer's session timezone, so the
	// read path must use the same default to see byte-equal values.
	// M2 trust model: both writer and reader share the same instance
	// default time_zone. Cross-timezone plans are out of scope for M2.
	_ = serverID
	return cfg.FormatDSN(), nil
}

func fetchServerUUID(dsn string) (string, error) {
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var uuid string
	if err := db.QueryRowContext(ctx, "SELECT @@server_uuid").Scan(&uuid); err != nil {
		return "", err
	}
	if uuid == "" {
		return "", fmt.Errorf("empty server uuid")
	}
	return uuid, nil
}
