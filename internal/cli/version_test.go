package cli

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/girimi/unredo/internal/buildinfo"
)

func TestVersionCommandJSON(t *testing.T) {
	oldVersion, oldCommit, oldDate := buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate
	buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate = "0.2.0", "abc123", "2026-08-03T12:00:00Z"
	t.Cleanup(func() {
		buildinfo.Version, buildinfo.Commit, buildinfo.BuildDate = oldVersion, oldCommit, oldDate
	})

	root := NewRoot()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetArgs([]string{"--format", "json", "version"})
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	var got versionOutput
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != "0.2.0" || got.Commit != "abc123" || got.BuildDate != "2026-08-03T12:00:00Z" {
		t.Fatalf("version output = %#v", got)
	}
}
