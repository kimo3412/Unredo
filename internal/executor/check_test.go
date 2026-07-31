package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/girimi/unredo/internal/core"
	"github.com/girimi/unredo/internal/ports"
)

type fakeReader struct {
	instance  string
	fp        core.SchemaFingerprint
	fpErr     error
	row       Row
	rowFound  bool
	rowErr    error
}

func (f *fakeReader) ReadByKey(_ context.Context, _ core.TableRef, _ []string, _ core.Row) (Row, bool, error) {
	return f.row, f.rowFound, f.rowErr
}
func (f *fakeReader) Fingerprint(_ context.Context, _ core.TableRef) (core.SchemaFingerprint, error) {
	if f.fpErr != nil {
		return "", f.fpErr
	}
	return f.fp, nil
}
func (f *fakeReader) TargetInstanceID() string { return f.instance }

func intVal(s string) core.Value { return core.Value{Kind: core.KindInteger, Data: core.RawJSON(s)} }
func strVal(s string) core.Value { return core.Value{Kind: core.KindText, Data: core.RawJSON("\"" + s + "\"")} }
func decVal(s string) core.Value { return core.Value{Kind: core.KindDecimal, Encoding: "string", Data: core.RawJSON("\"" + s + "\"")} }

func TestCheckReadyOnRevertInsert(t *testing.T) {
	plan := &ports.Plan{
		Ref: core.TransactionRef{Backend: "mysql", InstanceID: "uuid"},
		Operations: []ports.PlanOperation{{
			Sequence: 1,
			Table:    core.TableRef{Schema: "s", Name: "t"},
			Kind:     core.OpDelete, // revert of INSERT
			Key:      core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
			Expect: core.Row{
				Columns: []string{"id", "user_id", "status", "amount"},
				Values:  []core.Value{intVal("8"), intVal("1001"), strVal("paid"), decVal("199.00")},
			},
		}},
		SchemaFingerprints: map[string]string{"s.t": "sha256:abc"},
		Digest:             "sha256:def",
	}
	reader := &fakeReader{
		instance: "uuid",
		fp:       "sha256:abc",
		rowFound: true,
		row: Row{
			Columns: []string{"id", "user_id", "status", "amount"},
			Values:  []core.Value{intVal("8"), intVal("1001"), strVal("paid"), decVal("199.00")},
		},
	}
	r, err := Check(context.Background(), plan, reader)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusReady {
		t.Errorf("expected READY, got %s (%d conflicts)", r.Status, len(r.Conflicts))
		for _, c := range r.Conflicts {
			t.Logf("conflict: %+v", c)
		}
	}
}

func TestCheckConflictOnValueChange(t *testing.T) {
	plan := &ports.Plan{
		Ref: core.TransactionRef{Backend: "mysql", InstanceID: "uuid"},
		Operations: []ports.PlanOperation{{
			Sequence: 1,
			Table:    core.TableRef{Schema: "s", Name: "t"},
			Kind:     core.OpDelete,
			Key:      core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
			Expect: core.Row{
				Columns: []string{"id", "status"},
				Values:  []core.Value{intVal("8"), strVal("paid")},
			},
		}},
		SchemaFingerprints: map[string]string{"s.t": "sha256:abc"},
		Digest:             "sha256:def",
	}
	reader := &fakeReader{
		instance: "uuid",
		fp:       "sha256:abc",
		rowFound: true,
		row: Row{
			Columns: []string{"id", "status"},
			Values:  []core.Value{intVal("8"), strVal("cancelled")},
		},
	}
	r, err := Check(context.Background(), plan, reader)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusConflict {
		t.Errorf("expected CONFLICT, got %s", r.Status)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].Kind != ConflictRowMismatch {
		t.Errorf("expected 1 row_mismatch conflict, got %+v", r.Conflicts)
	}
}

func TestCheckRowMissingForDelete(t *testing.T) {
	plan := &ports.Plan{
		Ref: core.TransactionRef{Backend: "mysql", InstanceID: "uuid"},
		Operations: []ports.PlanOperation{{
			Sequence: 1,
			Table:    core.TableRef{Schema: "s", Name: "t"},
			Kind:     core.OpDelete,
			Key:      core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
			Expect:   core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
		}},
		SchemaFingerprints: map[string]string{"s.t": "sha256:abc"},
	}
	reader := &fakeReader{instance: "uuid", fp: "sha256:abc", rowFound: false}
	r, err := Check(context.Background(), plan, reader)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusConflict {
		t.Errorf("expected CONFLICT, got %s", r.Status)
	}
	if len(r.Conflicts) != 1 || r.Conflicts[0].Kind != ConflictRowMissing {
		t.Errorf("expected row_missing, got %+v", r.Conflicts)
	}
}

func TestCheckStaleSchema(t *testing.T) {
	plan := &ports.Plan{
		Ref: core.TransactionRef{Backend: "mysql", InstanceID: "uuid"},
		Operations: []ports.PlanOperation{{
			Sequence: 1,
			Table:    core.TableRef{Schema: "s", Name: "t"},
			Kind:     core.OpDelete,
			Key:      core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
			Expect:   core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
		}},
		SchemaFingerprints: map[string]string{"s.t": "sha256:old"},
	}
	reader := &fakeReader{instance: "uuid", fp: "sha256:new", rowFound: true,
		row: Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}}}
	r, err := Check(context.Background(), plan, reader)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusStaleSchema {
		t.Errorf("expected STALE_SCHEMA, got %s", r.Status)
	}
}

func TestCheckSourceMismatch(t *testing.T) {
	plan := &ports.Plan{
		Ref: core.TransactionRef{Backend: "mysql", InstanceID: "other-uuid"},
		Operations: []ports.PlanOperation{{
			Sequence: 1,
			Table:    core.TableRef{Schema: "s", Name: "t"},
			Kind:     core.OpDelete,
			Key:      core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
			Expect:   core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
		}},
	}
	reader := &fakeReader{instance: "this-uuid"}
	r, err := Check(context.Background(), plan, reader)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusSourceMismatch {
		t.Errorf("expected SOURCE_MISMATCH, got %s", r.Status)
	}
}

func TestCheckFingerprintError(t *testing.T) {
	plan := &ports.Plan{
		Ref: core.TransactionRef{Backend: "mysql", InstanceID: "uuid"},
		Operations: []ports.PlanOperation{{
			Sequence: 1,
			Table:    core.TableRef{Schema: "s", Name: "t"},
			Kind:     core.OpDelete,
			Key:      core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
			Expect:   core.Row{Columns: []string{"id"}, Values: []core.Value{intVal("8")}},
		}},
		SchemaFingerprints: map[string]string{"s.t": "sha256:abc"},
	}
	reader := &fakeReader{instance: "uuid", fpErr: errors.New("permission denied")}
	r, err := Check(context.Background(), plan, reader)
	if err != nil {
		t.Fatal(err)
	}
	// Fingerprint failure must surface as a conflict so the operator
	// sees why the plan is not safe to apply.
	if r.Status != StatusConflict {
		t.Errorf("expected CONFLICT, got %s", r.Status)
	}
	if len(r.Conflicts) == 0 {
		t.Errorf("expected at least one conflict for the failed fingerprint")
	}
}
