package config

import (
	"path/filepath"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "unredo.yaml")
	want := &Config{Version: Version, Profiles: map[string]Profile{
		"local": {
			Backend: "mysql",
			Source:  Source{Mode: SourceReplication, Address: "127.0.0.1:3306", User: "reader", PasswordEnv: "READER_PASSWORD", ServerID: 12345},
			Target:  Target{Address: "127.0.0.1:3306", User: "executor", PasswordEnv: "EXECUTOR_PASSWORD"},
			Policy:  DefaultPolicy(),
		},
	}}
	if err := Save(path, want); err != nil {
		t.Fatal(err)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := got.Profile("local")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Source.ServerID != 12345 || profile.Source.PasswordEnv != "READER_PASSWORD" || profile.Target.PasswordEnv != "EXECUTOR_PASSWORD" {
		t.Fatalf("round trip changed profile: %+v", profile)
	}
}
