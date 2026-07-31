package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/girimi/unredo/internal/ports"
)

func (b *Backend) FindAction(ctx context.Context, actionID string) (*ports.Action, error) {
	id, err := ulidBytes(actionID)
	if err != nil {
		return nil, fmt.Errorf("mysql: invalid action id %q: %w", actionID, err)
	}
	return b.queryAction(ctx, `WHERE action_id = ?`, id)
}

func (b *Backend) LatestAction(ctx context.Context, rootPlanDigest string) (*ports.Action, error) {
	digest, err := digestBytes(rootPlanDigest)
	if err != nil {
		return nil, err
	}
	return b.queryAction(ctx, `WHERE root_plan_digest = ? ORDER BY chain_depth DESC LIMIT 1`, digest)
}

func (b *Backend) queryAction(ctx context.Context, suffix string, arg interface{}) (*ports.Action, error) {
	db, err := sql.Open("mysql", b.targetDSN)
	if err != nil {
		return nil, fmt.Errorf("mysql: action store open: %w", err)
	}
	defer db.Close()
	q := `SELECT action_id, plan_id, parent_action_id, root_plan_digest,
		action_type, target_state, chain_depth, source_native_transaction_id,
		plan_digest, execution_class, reason, tool_version, operator_name,
		CAST(UNIX_TIMESTAMP(created_at) * 1000000 AS UNSIGNED)
		FROM unredo_meta.action_markers ` + suffix
	var (
		actionID, planID, rootDigest, planDigest []byte
		parentID                                 []byte
		reason                                   sql.NullString
		a                                        ports.Action
		createdMicros                            uint64
	)
	err = db.QueryRowContext(ctx, q, arg).Scan(
		&actionID, &planID, &parentID, &rootDigest,
		&a.ActionType, &a.TargetState, &a.ChainDepth, &a.SourceNativeTransactionID,
		&planDigest, &a.ExecutionClass, &reason, &a.ToolVersion, &a.OperatorName, &createdMicros,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ports.ErrActionNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mysql: query action: %w", err)
	}
	a.ActionID, err = ulidString(actionID)
	if err != nil {
		return nil, fmt.Errorf("mysql: decode action id: %w", err)
	}
	a.PlanID, err = ulidString(planID)
	if err != nil {
		return nil, fmt.Errorf("mysql: decode plan id: %w", err)
	}
	if len(parentID) > 0 {
		a.ParentActionID, err = ulidString(parentID)
		if err != nil {
			return nil, fmt.Errorf("mysql: decode parent action id: %w", err)
		}
	}
	a.RootPlanDigest = "sha256:" + encodeHex(rootDigest)
	a.PlanDigest = "sha256:" + encodeHex(planDigest)
	a.Reason = reason.String
	a.CreatedAt = timeFromUnixMicros(createdMicros)
	return &a, nil
}

func digestBytes(digest string) ([]byte, error) {
	const prefix = "sha256:"
	if len(digest) != len(prefix)+64 || digest[:len(prefix)] != prefix {
		return nil, fmt.Errorf("mysql: invalid sha256 digest %q", digest)
	}
	return hexDecode(digest[len(prefix):])
}

func timeFromUnixMicros(micros uint64) time.Time {
	return time.UnixMicro(int64(micros)).UTC()
}
