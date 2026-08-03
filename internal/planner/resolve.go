package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

type ResolutionDecision string

const (
	DecisionSkip      ResolutionDecision = "skip"
	DecisionOverwrite ResolutionDecision = "overwrite"
	DecisionAbort     ResolutionDecision = "abort"
)

type Resolution struct {
	OperationSequence int                `json:"operation_sequence"`
	Decision          ResolutionDecision `json:"decision"`
	ConflictDigest    string             `json:"conflict_digest"`
}

type ResolveOptions struct {
	Operator    string
	Reason      string
	ToolVersion string
	Decisions   []Resolution
}

// BuildResolved creates a new immutable plan whose preconditions are bound to
// the row images observed by Check. A later change therefore conflicts again.
func BuildResolved(parent *Plan, check *ports.CheckResult, opts ResolveOptions) (*Plan, error) {
	if parent == nil || check == nil {
		return nil, errors.New("planner: parent plan and check result are required")
	}
	if check.PlanDigest != parent.Digest {
		return nil, fmt.Errorf("planner: check result does not belong to parent plan")
	}
	if check.Status != "CONFLICT" || len(check.Conflicts) == 0 {
		return nil, fmt.Errorf("planner: resolve requires a CONFLICT check result")
	}
	if opts.Operator == "" || opts.Reason == "" {
		return nil, fmt.Errorf("planner: resolution operator and reason are required")
	}

	grouped := groupConflicts(check.Conflicts)
	decisions := make(map[int]Resolution, len(opts.Decisions))
	for _, decision := range opts.Decisions {
		if decision.OperationSequence <= 0 {
			return nil, fmt.Errorf("planner: invalid resolution operation_sequence %d", decision.OperationSequence)
		}
		if _, exists := decisions[decision.OperationSequence]; exists {
			return nil, fmt.Errorf("planner: duplicate resolution for operation %d", decision.OperationSequence)
		}
		if _, exists := grouped[decision.OperationSequence]; !exists {
			return nil, fmt.Errorf("planner: operation %d has no conflict", decision.OperationSequence)
		}
		switch decision.Decision {
		case DecisionSkip, DecisionOverwrite, DecisionAbort:
		default:
			return nil, fmt.Errorf("planner: operation %d has invalid decision %q", decision.OperationSequence, decision.Decision)
		}
		want := ConflictDigest(parent.Digest, decision.OperationSequence, grouped[decision.OperationSequence])
		if decision.ConflictDigest != want {
			return nil, fmt.Errorf("planner: operation %d conflict digest mismatch", decision.OperationSequence)
		}
		decisions[decision.OperationSequence] = decision
	}
	for sequence := range grouped {
		if _, exists := decisions[sequence]; !exists {
			return nil, fmt.Errorf("planner: conflict on operation %d has no resolution", sequence)
		}
	}

	operations := make([]ports.PlanOperation, 0, len(parent.Operations))
	records := make([]Resolution, 0, len(decisions))
	for _, op := range parent.Operations {
		decision, conflicted := decisions[op.Sequence]
		if !conflicted {
			operations = append(operations, cloneOperation(op))
			continue
		}
		if decision.Decision == DecisionAbort {
			return nil, fmt.Errorf("planner: operation %d was aborted", op.Sequence)
		}
		records = append(records, decision)
		if decision.Decision == DecisionSkip {
			continue
		}
		resolved, keep, err := overwriteOperation(op, grouped[op.Sequence])
		if err != nil {
			return nil, err
		}
		if keep {
			operations = append(operations, resolved)
		}
	}
	sort.Slice(records, func(i, j int) bool { return records[i].OperationSequence < records[j].OperationSequence })
	for i := range operations {
		operations[i].Sequence = i + 1
	}

	rootDigest := parent.RootPlanDigest
	if rootDigest == "" {
		rootDigest = parent.Digest
	}
	resolved := &Plan{
		FormatVersion:      FormatVersion,
		PlanID:             ulid.Make().String(),
		Mode:               parent.Mode,
		ExecutionClass:     ClassUnsafeResolved,
		CreatedAt:          time.Now().UTC(),
		ToolVersion:        opts.ToolVersion,
		Source:             parent.Source,
		SchemaFingerprints: cloneStringMap(parent.SchemaFingerprints),
		Operations:         operations,
		BackendExtensions:  cloneRawMap(parent.BackendExtensions),
		RootPlanDigest:     rootDigest,
		ParentActionID:     parent.ParentActionID,
		ChainDepth:         parent.ChainDepth,
		ParentPlanDigest:   parent.Digest,
		ResolutionReason:   opts.Reason,
		ResolutionOperator: opts.Operator,
		Resolutions:        records,
	}
	resolved.Digest = computeDigest(resolved)
	return resolved, nil
}

// ConflictDigest binds a decision to the exact conflict evidence observed by
// plan check. Messages are excluded because they are presentation text.
func ConflictDigest(parentDigest string, sequence int, conflicts []ports.Conflict) string {
	type evidence struct {
		ParentDigest string           `json:"parent_plan_digest"`
		Sequence     int              `json:"operation_sequence"`
		Conflicts    []ports.Conflict `json:"conflicts"`
	}
	clean := make([]ports.Conflict, len(conflicts))
	for i, conflict := range conflicts {
		clean[i] = conflict
		clean[i].Message = ""
	}
	raw, _ := canonicalJSON(evidence{ParentDigest: parentDigest, Sequence: sequence, Conflicts: clean})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func groupConflicts(conflicts []ports.Conflict) map[int][]ports.Conflict {
	out := make(map[int][]ports.Conflict)
	for _, conflict := range conflicts {
		out[conflict.OperationSequence] = append(out[conflict.OperationSequence], conflict)
	}
	return out
}

func overwriteOperation(op ports.PlanOperation, conflicts []ports.Conflict) (ports.PlanOperation, bool, error) {
	out := cloneOperation(op)
	if len(conflicts) == 0 {
		return out, false, fmt.Errorf("planner: operation %d has no conflict evidence", op.Sequence)
	}
	kind := conflicts[0].Kind
	for _, conflict := range conflicts {
		if conflict.Kind != kind {
			return out, false, fmt.Errorf("planner: operation %d has mixed conflict kinds", op.Sequence)
		}
	}
	switch kind {
	case "row_mismatch":
		if op.Kind != core.OpUpdate && op.Kind != core.OpDelete {
			return out, false, fmt.Errorf("planner: cannot overwrite row mismatch for %s operation %d", op.Kind, op.Sequence)
		}
		out.Expect = cloneRow(conflicts[0].Current)
		if len(out.Expect.Columns) == 0 {
			return out, false, fmt.Errorf("planner: operation %d conflict has no current row image", op.Sequence)
		}
		if op.Kind == core.OpUpdate && rowContains(out.Expect, op.Write) {
			return out, false, nil
		}
		return out, true, nil
	case "row_exists":
		if op.Kind != core.OpInsert || len(conflicts[0].Current.Columns) == 0 {
			return out, false, fmt.Errorf("planner: cannot overwrite row_exists for operation %d", op.Sequence)
		}
		if rowContains(conflicts[0].Current, op.Write) {
			return out, false, nil
		}
		out.Kind = core.OpUpdate
		out.Expect = cloneRow(conflicts[0].Current)
		return out, true, nil
	case "row_missing":
		switch op.Kind {
		case core.OpDelete:
			return out, false, nil
		case core.OpUpdate:
			out.Kind = core.OpInsert
			out.Expect = core.Row{}
			out.Key = projectColumns(out.Write, op.Key.Columns)
			if len(out.Key.Columns) != len(op.Key.Columns) {
				return out, false, fmt.Errorf("planner: operation %d write image is missing key columns", op.Sequence)
			}
			return out, true, nil
		}
	}
	return out, false, fmt.Errorf("planner: conflict kind %q on operation %d cannot be overwritten", kind, op.Sequence)
}

func cloneOperation(op ports.PlanOperation) ports.PlanOperation {
	op.Key = cloneRow(op.Key)
	op.Expect = cloneRow(op.Expect)
	op.Write = cloneRow(op.Write)
	return op
}

func cloneRow(row core.Row) core.Row {
	out := core.Row{Columns: append([]string(nil), row.Columns...), Values: make([]core.Value, len(row.Values))}
	for i, value := range row.Values {
		out.Values[i] = value
		out.Values[i].Data = append(core.RawJSON(nil), value.Data...)
		if value.Native != nil {
			native := *value.Native
			out.Values[i].Native = &native
		}
	}
	return out
}

func rowContains(current, desired core.Row) bool {
	for i, column := range desired.Columns {
		if i >= len(desired.Values) {
			return false
		}
		got, ok := current.Get(column)
		if !ok || !got.Equal(desired.Values[i]) {
			return false
		}
	}
	return true
}
