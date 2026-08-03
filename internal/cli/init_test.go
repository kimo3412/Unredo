package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/girimi/unredo/internal/config"
)

func TestInitNonInteractiveWritesProfileAndGrantTemplate(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "unredo.yaml")
	grantsPath := filepath.Join(dir, "grants.sql")
	root := NewRoot()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{
		"--config", configPath,
		"--profile", "staging",
		"init",
		"--non-interactive",
		"--skip-doctor",
		"--address", "mysql.internal:3306",
		"--reader-user", "audit_reader",
		"--reader-password-env", "STAGING_READER_PASSWORD",
		"--executor-user", "undo_executor",
		"--executor-password-env", "STAGING_EXECUTOR_PASSWORD",
		"--database", "shop",
		"--database", "billing",
		"--server-id", "345678",
		"--grants-output", grantsPath,
	})
	if err := root.Execute(); err != nil {
		t.Fatalf("init: %v\n%s", err, output.String())
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := cfg.Profile("staging")
	if err != nil {
		t.Fatal(err)
	}
	if profile.Source.ServerID != 345678 || profile.Source.Address != "mysql.internal:3306" || profile.Target.Address != "mysql.internal:3306" {
		t.Fatalf("unexpected profile: %+v", profile)
	}
	if profile.Source.PasswordEnv != "STAGING_READER_PASSWORD" || profile.Target.PasswordEnv != "STAGING_EXECUTOR_PASSWORD" {
		t.Fatal("password environment references were not persisted")
	}
	raw, err := os.ReadFile(grantsPath)
	if err != nil {
		t.Fatal(err)
	}
	sql := string(raw)
	for _, want := range []string{
		"GRANT REPLICATION SLAVE, REPLICATION CLIENT",
		"GRANT SELECT ON `billing`.*",
		"GRANT SELECT, INSERT, UPDATE, DELETE ON `shop`.*",
		"GRANT SELECT ON `unredo_meta`.*",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("grant template missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(strings.ToUpper(sql), "IDENTIFIED BY") {
		t.Fatal("grant template must not generate a default account password")
	}
}

func TestInitRefusesExistingProfileWithoutOverwrite(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "unredo.yaml")
	grantsPath := filepath.Join(dir, "grants.sql")
	args := []string{
		"--config", configPath, "--profile", "same", "init",
		"--non-interactive", "--skip-doctor", "--database", "shop",
		"--server-id", "123456", "--grants-output", grantsPath,
	}
	first := NewRoot()
	first.SetArgs(args)
	if err := first.Execute(); err != nil {
		t.Fatal(err)
	}
	second := NewRoot()
	second.SetArgs(args)
	if err := second.Execute(); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected existing profile error, got %v", err)
	}
}

func TestRandomServerIDIsNonZero(t *testing.T) {
	for i := 0; i < 16; i++ {
		id, err := randomServerID()
		if err != nil {
			t.Fatal(err)
		}
		if id < 10_000 {
			t.Fatalf("generated unsafe server id %d", id)
		}
	}
}
