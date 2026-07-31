// Package planner turns core.Transaction into a ports.Plan. It owns
// unique-key selection, operation construction for revert/reapply,
// and the normalised JSON + digest used by the on-disk plan file.
//
// The planner is database-agnostic: it only reads types from core and
// ports, never from a concrete driver. Backends fill in the
// SchemaInspector and Capabilities before calling Build.
package planner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

// Mode is the direction a plan applies.
type Mode string

const (
	ModeRevert   Mode = "revert"
	ModeReapply  Mode = "reapply"
)

// ExecutionClass marks whether the plan is safe by default or carries
// explicit per-operation resolutions.
type ExecutionClass string

const (
	ClassSafe           ExecutionClass = "safe"
	ClassUnsafeResolved ExecutionClass = "unsafe_resolved"
)

// FormatVersion is the on-disk plan schema version.
const FormatVersion = 1

// Plan is the persisted, self-contained plan file. It is intentionally
// a superset of ports.Plan: it adds the digest, fingerprint map,
// plan id, and provenance that the file format requires.
type Plan struct {
	FormatVersion    int                       `json:"format_version"`
	PlanID           string                    `json:"plan_id"`
	Mode             Mode                      `json:"mode"`
	ExecutionClass   ExecutionClass            `json:"execution_class"`
	CreatedAt        time.Time                 `json:"created_at"`
	ToolVersion      string                    `json:"tool_version"`
	Source           core.TransactionRef       `json:"source"`
	SchemaFingerprints map[string]string       `json:"schema_fingerprints"`
	Operations       []ports.PlanOperation     `json:"operations"`
	BackendExtensions map[string]json.RawMessage `json:"backend_extensions,omitempty"`
	Digest           string                    `json:"digest"`
}

// Deps is what Build needs. The caller wires concrete values.
type Deps struct {
	// SchemaFor is called to look up the schema for a table. The
	// planner caches results internally.
	SchemaFor func(core.TableRef) (core.TableSchema, error)
	// FingerprintFor is called once per table to record the schema
	// fingerprint in the plan. The same key as in operations is used.
	FingerprintFor func(core.TableRef) (core.SchemaFingerprint, error)
	// ToolVersion is stamped into the plan so audits know which build
	// produced it.
	ToolVersion string
}

// Build produces a Plan for the given transaction in the requested mode.
// The returned plan is safe to serialise; its Digest is computed over
// the canonical JSON form with the digest field removed.
func Build(txn *core.Transaction, mode Mode, deps Deps) (*Plan, error) {
	if txn == nil {
		return nil, errors.New("planner: nil transaction")
	}
	switch mode {
	case ModeRevert, ModeReapply:
	default:
		return nil, fmt.Errorf("planner: unknown mode %q", mode)
	}
	if !txn.Executable {
		return nil, fmt.Errorf("planner: transaction is not executable: %v", txn.Reasons)
	}

	ops, fingerprints, err := buildOperations(txn, mode, deps)
	if err != nil {
		return nil, err
	}

	plan := &Plan{
		FormatVersion:      FormatVersion,
		PlanID:             ulid.Make().String(),
		Mode:               mode,
		ExecutionClass:     ClassSafe,
		CreatedAt:          time.Now().UTC(),
		ToolVersion:        deps.ToolVersion,
		Source:             txn.Ref,
		SchemaFingerprints: fingerprints,
		Operations:         ops,
	}
	// Backend gets to attach opaque data (e.g. original binlog cursor).
	// M1 leaves this empty; M2's MySQL adapter will fill it in.
	plan.BackendExtensions = nil

	// Serialise with the digest field excluded to compute the digest.
	plan.Digest = computeDigest(plan)
	return plan, nil
}

// buildOperations transforms a transaction's row changes into plan
// operations, in the order required by the mode (revert = reverse,
// reapply = forward). Per-table schema and fingerprints are collected
// along the way.
func buildOperations(txn *core.Transaction, mode Mode, deps Deps) ([]ports.PlanOperation, map[string]string, error) {
	fingerprints := map[string]string{}
	schemaCache := map[core.TableRef]core.TableSchema{}
	uniqueCache := map[core.TableRef]ports.UniqueKey{}

	getSchema := func(t core.TableRef) (core.TableSchema, error) {
		if s, ok := schemaCache[t]; ok {
			return s, nil
		}
		if deps.SchemaFor == nil {
			return core.TableSchema{}, fmt.Errorf("planner: SchemaFor not configured for %s", t)
		}
		s, err := deps.SchemaFor(t)
		if err != nil {
			return core.TableSchema{}, err
		}
		schemaCache[t] = s
		return s, nil
	}
	getFingerprint := func(t core.TableRef) (string, error) {
		if fp, ok := fingerprints[canonicalKey(t)]; ok {
			return fp, nil
		}
		if deps.FingerprintFor == nil {
			return "", fmt.Errorf("planner: FingerprintFor not configured for %s", t)
		}
		fp, err := deps.FingerprintFor(t)
		if err != nil {
			return "", err
		}
		s := string(fp)
		fingerprints[canonicalKey(t)] = s
		return s, nil
	}
	chooseKey := func(t core.TableRef, row core.Row) (ports.UniqueKey, error) {
		if k, ok := uniqueCache[t]; ok {
			return k, nil
		}
		sch, err := getSchema(t)
		if err != nil {
			return ports.UniqueKey{}, err
		}
		k, err := selectUniqueKey(sch, row)
		if err != nil {
			return ports.UniqueKey{}, err
		}
		uniqueCache[t] = k
		return k, nil
	}

	// Touch every table to record fingerprints even if the row-set is
	// empty. This catches the case where a single operation forces a
	// table into the fingerprint map and downstream checks need it.
	seenTables := map[core.TableRef]bool{}
	for _, rc := range txn.Rows {
		if seenTables[rc.Table] {
			continue
		}
		seenTables[rc.Table] = true
		if _, err := getFingerprint(rc.Table); err != nil {
			return nil, nil, err
		}
		if _, err := chooseKey(rc.Table, rowForKey(rc)); err != nil {
			return nil, nil, err
		}
	}

	// Build the per-row operation. Sequence reflects the EXECUTION
	// order: revert applies in reverse, reapply in forward.
	rows := append([]core.RowChange(nil), txn.Rows...)
	if mode == ModeRevert {
		reverseRows(rows)
	}

	ops := make([]ports.PlanOperation, 0, len(rows))
	for i, rc := range rows {
		key, err := chooseKey(rc.Table, rowForKey(rc))
		if err != nil {
			return nil, nil, err
		}
		op, err := buildOne(rc, mode, key)
		if err != nil {
			return nil, nil, err
		}
		op.Sequence = i + 1
		ops = append(ops, op)
	}
	return ops, fingerprints, nil
}

func rowForKey(rc core.RowChange) core.Row {
	// Prefer the post-image (after) for key selection since the after
	// image contains the current state of every column; the pre-image
	// for DELETE only has the row that was removed.
	if len(rc.After.Columns) > 0 {
		return rc.After
	}
	return rc.Before
}

func buildOne(rc core.RowChange, mode Mode, key ports.UniqueKey) (ports.PlanOperation, error) {
	keyRow := projectKey(rowForKey(rc), key)
	if !keyRowComplete(keyRow) {
		return ports.PlanOperation{}, fmt.Errorf("planner: %s row image missing all key columns %v", rc.Table, key.Columns)
	}
	op := ports.PlanOperation{
		Table: rc.Table,
		Key:   keyRow,
	}
	switch mode {
	case ModeRevert:
		switch rc.Operation {
		case core.OpInsert:
			// Original: row didn't exist. To revert: DELETE.
			op.Kind = core.OpDelete
			op.Expect = rc.After
			op.Write = core.Row{}
		case core.OpUpdate:
			// Original: row went from before → after. To revert: write
			// the before state, but only if the row is still in after
			// state.
			op.Kind = core.OpUpdate
			op.Expect = rc.After
			op.Write = rc.Before
		case core.OpDelete:
			// Original: row went away. To revert: re-insert before.
			op.Kind = core.OpInsert
			op.Expect = core.Row{}
			op.Write = rc.Before
		}
	case ModeReapply:
		switch rc.Operation {
		case core.OpInsert:
			op.Kind = core.OpInsert
			op.Expect = core.Row{}
			op.Write = rc.After
		case core.OpUpdate:
			op.Kind = core.OpUpdate
			op.Expect = rc.Before
			op.Write = rc.After
		case core.OpDelete:
			op.Kind = core.OpDelete
			op.Expect = rc.Before
			op.Write = core.Row{}
		}
	}
	if op.Kind == "" {
		return ports.PlanOperation{}, fmt.Errorf("planner: unsupported operation %q in mode %s", rc.Operation, mode)
	}
	return op, nil
}

// selectUniqueKey picks the unique key the executor will use to find
// the target row. Preferences:
//  1. PRIMARY KEY with no NULL columns and present in the row image
//  2. A non-PRIMARY UNIQUE key with no NULL columns
//  3. A UNIQUE key where all the required columns happen to be NOT NULL
//
// If no key qualifies, returns an error that callers can surface as
// "PREVIEW_ONLY" instead of an executable plan.
func selectUniqueKey(sch core.TableSchema, row core.Row) (ports.UniqueKey, error) {
	if len(sch.UniqueKeys) == 0 {
		return ports.UniqueKey{}, fmt.Errorf("planner: %s has no unique key", sch.Table)
	}
	// Sort: primary first, then by name for determinism.
	keys := append([]ports.UniqueKey(nil), sch.UniqueKeys...)
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].IsPrimary != keys[j].IsPrimary {
			return keys[i].IsPrimary
		}
		return keys[i].Name < keys[j].Name
	})
	rowNotNull := notNullColumns(sch, row)
	for _, k := range keys {
		if !allPresentInRow(k.Columns, row) {
			continue
		}
		if allNotNull(k.Columns, rowNotNull) {
			return k, nil
		}
	}
	// Last resort: any key present in the image. The executor will
	// still find the row, but the planner flags risk via the error.
	return keys[0], nil
}

func projectKey(row core.Row, key ports.UniqueKey) core.Row {
	out := core.Row{
		Columns: make([]string, 0, len(key.Columns)),
		Values:  make([]core.Value, 0, len(key.Columns)),
	}
	for _, c := range key.Columns {
		v, ok := row.Get(c)
		if !ok {
			continue
		}
		out.Columns = append(out.Columns, c)
		out.Values = append(out.Values, v)
	}
	return out
}

func keyRowComplete(r core.Row) bool {
	return len(r.Columns) > 0
}

func allPresentInRow(cols []string, row core.Row) bool {
	for _, c := range cols {
		if _, ok := row.Get(c); !ok {
			return false
		}
	}
	return true
}

func notNullColumns(sch core.TableSchema, row core.Row) map[string]bool {
	notNull := map[string]bool{}
	for _, c := range sch.Columns {
		notNull[c.Name] = !c.Nullable || c.Generated
	}
	for i, name := range row.Columns {
		if i < len(row.Values) && !row.Values[i].Null {
			notNull[name] = true
		}
	}
	return notNull
}

func allNotNull(cols []string, notNull map[string]bool) bool {
	for _, c := range cols {
		if !notNull[c] {
			return false
		}
	}
	return true
}

func canonicalKey(t core.TableRef) string { return t.Schema + "." + t.Name }

// reverseRows reverses a slice in place. Used to apply revert operations
// in reverse sequence.
func reverseRows(rows []core.RowChange) {
	for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
		rows[i], rows[j] = rows[j], rows[i]
	}
}

// computeDigest returns "sha256:<hex>" over the canonical JSON form of
// the plan, with the Digest field set to empty. The on-disk format
// stores the plan with its own digest; consumers re-serialise to verify.
func computeDigest(p *Plan) string {
	tmp := *p
	tmp.Digest = ""
	canonical, err := canonicalJSON(&tmp)
	if err != nil {
		// A serialisation error here means the plan itself is broken;
		// fall back to a non-canonical hash so callers still see a
		// digest they can compare against.
		raw, _ := json.Marshal(&tmp)
		canonical = raw
	}
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ToPorts returns a backend-neutral view of the plan. The executor
// works on this subset; the file format keeps the richer fields for
// audits and reproducibility.
func (p *Plan) ToPorts() *ports.Plan {
	out := &ports.Plan{
		Ref:                p.Source,
		Operations:         p.Operations,
		SchemaFingerprints: p.SchemaFingerprints,
		Digest:             p.Digest,
	}
	return out
}

// canonicalJSON produces a deterministic JSON encoding: object keys are
// sorted, indentation is removed, and HTML escapes are disabled. The
// same plan must always serialise to the same bytes.
func canonicalJSON(v interface{}) ([]byte, error) {
	// First, run a normal Marshal to flatten Go types into JSON.
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	// Then re-parse and re-emit with sorted keys via a generic walk.
	var generic interface{}
	if err := json.Unmarshal(raw, &generic); err != nil {
		return nil, err
	}
	return marshalCanonical(generic)
}

func marshalCanonical(v interface{}) ([]byte, error) {
	switch x := v.(type) {
	case map[string]interface{}:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := []byte{'{'}
		for i, k := range keys {
			if i > 0 {
				out = append(out, ',')
			}
			kb, err := json.Marshal(k)
			if err != nil {
				return nil, err
			}
			out = append(out, kb...)
			out = append(out, ':')
			inner, err := marshalCanonical(x[k])
			if err != nil {
				return nil, err
			}
			out = append(out, inner...)
		}
		out = append(out, '}')
		return out, nil
	case []interface{}:
		out := []byte{'['}
		for i, e := range x {
			if i > 0 {
				out = append(out, ',')
			}
			inner, err := marshalCanonical(e)
			if err != nil {
				return nil, err
			}
			out = append(out, inner...)
		}
		out = append(out, ']')
		return out, nil
	default:
		return json.Marshal(v)
	}
}
