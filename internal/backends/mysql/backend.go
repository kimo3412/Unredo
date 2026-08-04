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
	instanceID       string
	targetInstanceID string
	sourceDSN        string
	targetDSN        string
	serverID         uint32
	sourceMode       config.SourceMode
	localBinlogDir   string
	policy           config.Policy
	inspector        ports.SchemaInspector
	source           *mysqlsource.Source
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
	if p.Source.Mode != config.SourceReplication && p.Source.Mode != config.SourceLocalFile {
		return nil, fmt.Errorf("mysql: unsupported source.mode %q", p.Source.Mode)
	}
	if p.Source.Mode == config.SourceLocalFile && p.Source.BinlogPath == "" {
		return nil, fmt.Errorf("mysql: source.binlog_path is required for local-file mode")
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
	targetInstanceID, err := fetchServerUUID(tgtDSN)
	if err != nil {
		return nil, fmt.Errorf("mysql: detect target server uuid: %w", err)
	}
	insp := schema.NewInspector(srcDSN)
	src := mysqlsource.New(srcDSN, instanceID, p.Source.ServerID,
		p.Policy.MaxTransactionRows, p.Policy.MaxTransactionBytes)
	if p.Source.Mode == config.SourceLocalFile {
		src = mysqlsource.NewLocal(srcDSN, instanceID, p.Source.BinlogPath, p.Source.ServerID,
			p.Policy.MaxTransactionRows, p.Policy.MaxTransactionBytes)
	}
	return &Backend{
		instanceID:       instanceID,
		targetInstanceID: targetInstanceID,
		sourceDSN:        srcDSN,
		targetDSN:        tgtDSN,
		serverID:         p.Source.ServerID,
		sourceMode:       p.Source.Mode,
		localBinlogDir:   p.Source.BinlogPath,
		policy:           p.Policy,
		inspector:        insp,
		source:           src,
	}, nil
}

// Name implements ports.Backend.
func (b *Backend) Name() string { return "mysql" }

// InstanceID returns the MySQL @@server_uuid, used to validate plan refs.
func (b *Backend) InstanceID() string { return b.instanceID }

// TargetInstanceID is used by commit-unknown verification to prove that a
// missing marker was queried on the exact instance named by the plan.
func (b *Backend) TargetInstanceID() string { return b.targetInstanceID }

// Capabilities implements ports.ChangeSource.
// MySQL 8 with ROW/FULL/GTID is the assumed M0 configuration. The flags
// reflect what the planner can rely on; schema-at-event-time is reported
// as true only when we read schema with the source conn, which we do.
func (b *Backend) Capabilities(ctx context.Context) (core.BackendCapabilities, error) {
	return b.source.Capabilities(ctx)
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
func (b *Backend) Check(ctx context.Context, plan ports.Plan) (*ports.CheckResult, error) {
	reader := NewCheckReader(b.targetDSN, b.targetInstanceID)
	result, err := executor.Check(ctx, &plan, reader)
	_ = reader.Close()
	if err != nil {
		return nil, err
	}
	out := &ports.CheckResult{
		Status: string(result.Status), PlanDigest: result.PlanDigest,
		TargetInstance: result.TargetInstance, OperationsTotal: result.OperationsTotal,
		SchemaChecks: make([]ports.SchemaCheck, 0, len(result.SchemaChecks)),
	}
	for _, s := range result.SchemaChecks {
		out.SchemaChecks = append(out.SchemaChecks, ports.SchemaCheck{Table: s.Table, PlanDigest: s.PlanDigest, ActualDigest: s.ActualDigest, Match: s.Match})
	}
	conflicts := make([]ports.Conflict, 0, len(result.Conflicts))
	for _, c := range result.Conflicts {
		conflicts = append(conflicts, ports.Conflict{
			OperationSequence: c.OperationSequence,
			Table:             c.Table,
			Kind:              string(c.Kind),
			Column:            c.Column,
			Expected:          c.Expected,
			Actual:            c.Actual,
			Current:           c.Current,
			Message:           c.Message,
		})
	}
	out.Conflicts = conflicts
	return out, nil
}

// Apply executes a plan in a single InnoDB transaction together with
// the action marker. See ports.ApplyRequest for what the caller
// supplies; everything else comes from the plan.
func (b *Backend) Apply(ctx context.Context, plan ports.Plan, req ports.ApplyRequest) (ports.ExecutionResult, error) {
	if plan.Ref.Backend != "mysql" {
		return ports.ExecutionResult{}, fmt.Errorf("mysql: plan backend is %q: %w", plan.Ref.Backend, ports.ErrUnsupportedCapability)
	}
	if plan.ExecutionClass != "safe" && plan.ExecutionClass != "unsafe_resolved" {
		return ports.ExecutionResult{}, fmt.Errorf("mysql: execution class %q is not implemented: %w", plan.ExecutionClass, ports.ErrUnsupportedCapability)
	}
	if err := b.validateActionChain(ctx, plan); err != nil {
		return ports.ExecutionResult{}, err
	}
	// Apply must never rely on a check performed by an earlier CLI command.
	// Re-check the real target instance, schema and current row images now.
	reader := NewCheckReader(b.targetDSN, b.targetInstanceID)
	check, err := executor.Check(ctx, &plan, reader)
	_ = reader.Close()
	if err != nil {
		return ports.ExecutionResult{}, fmt.Errorf("mysql: pre-apply check: %w", err)
	}
	switch check.Status {
	case executor.StatusReady:
	case executor.StatusSourceMismatch:
		return ports.ExecutionResult{}, ports.ErrInstanceMismatch
	case executor.StatusStaleSchema:
		return ports.ExecutionResult{}, ports.ErrSchemaMismatch
	default:
		return ports.ExecutionResult{}, executor.ErrApplyConflict
	}
	writer := NewApplyWriterFromBackend(b)
	opts, err := b.buildApplyOptions(plan, req)
	if err != nil {
		return ports.ExecutionResult{}, err
	}
	startCursor, cursorErr := b.source.CurrentCursor(ctx)
	result, err := writer.Apply(ctx, &plan, opts)
	if err != nil {
		return result, err
	}
	if cursorErr != nil {
		result.GTIDCorrelationWarning = cursorErr.Error()
		return result, nil
	}
	actionID, err := ulidBytes(req.ActionID)
	if err != nil {
		result.GTIDCorrelationWarning = "invalid committed action id: " + err.Error()
		return result, nil
	}
	correlationCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	gtid, err := b.source.FindActionGTID(correlationCtx, startCursor, actionID)
	if err != nil {
		result.GTIDCorrelationWarning = err.Error()
		return result, nil
	}
	result.CompensatingGTID = gtid
	return result, nil
}

func (b *Backend) validateActionChain(ctx context.Context, plan ports.Plan) error {
	if plan.ParentActionID == "" {
		if plan.ChainDepth != 0 {
			return fmt.Errorf("mysql: root plan has inconsistent action-chain metadata")
		}
		if plan.ExecutionClass == "safe" && (plan.RootPlanDigest != "" || plan.ParentPlanDigest != "") {
			return fmt.Errorf("mysql: safe root plan has unexpected parent metadata")
		}
		if plan.ExecutionClass == "unsafe_resolved" && (plan.RootPlanDigest == "" || plan.ParentPlanDigest == "") {
			return fmt.Errorf("mysql: resolved root plan is missing parent/root digest")
		}
		return nil
	}
	if plan.RootPlanDigest == "" || plan.ChainDepth == 0 {
		return fmt.Errorf("mysql: child plan is missing root digest or chain depth")
	}
	if b.policy.MaxActionDepth > 0 && int(plan.ChainDepth) > b.policy.MaxActionDepth {
		return fmt.Errorf("mysql: action chain depth %d exceeds limit %d", plan.ChainDepth, b.policy.MaxActionDepth)
	}
	parent, err := b.FindAction(ctx, plan.ParentActionID)
	if err != nil {
		return fmt.Errorf("mysql: parent action: %w", err)
	}
	if parent.RootPlanDigest != plan.RootPlanDigest || parent.ChainDepth+1 != plan.ChainDepth {
		return fmt.Errorf("mysql: parent action does not match root/depth")
	}
	if parent.SourceNativeTransactionID != plan.Ref.NativeTransactionID {
		return fmt.Errorf("mysql: parent action does not match source transaction")
	}
	latest, err := b.LatestAction(ctx, plan.RootPlanDigest)
	if err != nil {
		return fmt.Errorf("mysql: latest action: %w", err)
	}
	if latest.ActionID != parent.ActionID {
		return fmt.Errorf("mysql: parent action is not the latest successful action")
	}
	if plan.Mode == "reapply" && parent.TargetState != "ORIGINAL_REVERTED" {
		return fmt.Errorf("mysql: reapply requires latest state ORIGINAL_REVERTED")
	}
	if plan.Mode == "revert" && parent.TargetState != "ORIGINAL_APPLIED" {
		return fmt.Errorf("mysql: chained revert requires latest state ORIGINAL_APPLIED")
	}
	return nil
}

// buildApplyOptions derives the marker row from a plan and the
// caller-supplied action id. plan_id is taken from the plan file
// (a ULID); the action type and target state come from plan.Mode.
func (b *Backend) buildApplyOptions(plan ports.Plan, req ports.ApplyRequest) (executor.ApplyOptions, error) {
	planID, err := ulidBytes(plan.PlanID)
	if err != nil {
		return executor.ApplyOptions{}, fmt.Errorf("mysql: plan_id %q: %w", plan.PlanID, err)
	}
	actionIDBytes, err := ulidBytes(req.ActionID)
	if err != nil {
		return executor.ApplyOptions{}, fmt.Errorf("mysql: action_id %q: %w", req.ActionID, err)
	}
	var actionType, targetState string
	switch plan.Mode {
	case "revert":
		actionType = "REVERT"
		targetState = "ORIGINAL_REVERTED"
	case "reapply":
		actionType = "REAPPLY"
		targetState = "ORIGINAL_APPLIED"
	default:
		return executor.ApplyOptions{}, fmt.Errorf("mysql: plan mode %q unsupported", plan.Mode)
	}
	executionClass := "SAFE"
	if plan.ExecutionClass == "unsafe_resolved" {
		executionClass = "UNSAFE_RESOLVED"
	}
	chainDepth := uint32(0)
	rootPlanDigest := plan.RootPlanDigest
	if rootPlanDigest == "" {
		rootPlanDigest = plan.Digest
	}
	var parentActionID []byte
	if plan.ParentActionID != "" {
		parentActionID, err = ulidBytes(plan.ParentActionID)
		if err != nil {
			return executor.ApplyOptions{}, fmt.Errorf("mysql: parent_action_id %q: %w", plan.ParentActionID, err)
		}
	}
	chainDepth = plan.ChainDepth
	return executor.ApplyOptions{
		PlanID:                    planID,
		ActionID:                  actionIDBytes,
		ActionType:                actionType,
		TargetState:               targetState,
		ChainDepth:                chainDepth,
		ParentActionID:            parentActionID,
		RootPlanDigest:            rootPlanDigest,
		OperatorName:              req.OperatorName,
		Reason:                    req.Reason,
		ExecutionClass:            executionClass,
		SourceNativeTransactionID: plan.Ref.NativeTransactionID,
		Confirm:                   req.Confirm,
	}, nil
}

// RunDoctor exposes the doctor checks for the CLI.
func (b *Backend) RunDoctor(_ context.Context, d *doctor.Deps) (*doctor.Report, error) {
	return doctor.Run(d, b.sourceDSN, b.targetDSN, b.instanceID, b.serverID, b.sourceMode, b.localBinlogDir, b.policy)
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
