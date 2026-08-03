package cli

import (
	"bufio"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/backends/mysql"
	"github.com/girimi/unredo/internal/config"
)

func init() { Register(newInitCmd) }

func newInitCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "init",
		Short: "Create a safe MySQL profile and bootstrap instructions",
		RunE:  runInit,
	}
	c.Flags().String("address", "127.0.0.1:3306", "MySQL source address")
	c.Flags().String("target-address", "", "MySQL target address (default: --address)")
	c.Flags().String("reader-user", "unredo_reader", "existing binlog reader account")
	c.Flags().String("reader-password-env", "UNREDO_READER_PASSWORD", "environment variable containing the reader password")
	c.Flags().String("executor-user", "unredo_executor", "existing compensation executor account")
	c.Flags().String("executor-password-env", "UNREDO_EXECUTOR_PASSWORD", "environment variable containing the executor password")
	c.Flags().StringSlice("database", nil, "business schema to grant access to (repeatable)")
	c.Flags().String("account-host", "127.0.0.1", "MySQL account host used in generated GRANT statements")
	c.Flags().Uint32("server-id", 0, "replication client server_id (default: random persistent value)")
	c.Flags().String("grants-output", "", "generated least-privilege SQL path")
	c.Flags().Bool("overwrite-profile", false, "replace an existing profile of the same name")
	c.Flags().Bool("non-interactive", false, "do not prompt; all required values must be supplied")
	c.Flags().Bool("skip-doctor", false, "write setup files without connecting to MySQL")
	c.Flags().Bool("apply-meta", false, "create unredo_meta using explicitly supplied admin credentials")
	c.Flags().String("admin-user", "", "temporary admin user for --apply-meta (never persisted)")
	c.Flags().String("admin-password-env", "", "environment variable containing the temporary admin password")
	return c
}

func runInit(cmd *cobra.Command, _ []string) error {
	profileName, _ := cmd.Flags().GetString("profile")
	configPath, _ := cmd.Flags().GetString("config")
	if configPath == "" {
		configPath = "unredo.yaml"
	}
	address, _ := cmd.Flags().GetString("address")
	targetAddress, _ := cmd.Flags().GetString("target-address")
	readerUser, _ := cmd.Flags().GetString("reader-user")
	readerPasswordEnv, _ := cmd.Flags().GetString("reader-password-env")
	executorUser, _ := cmd.Flags().GetString("executor-user")
	executorPasswordEnv, _ := cmd.Flags().GetString("executor-password-env")
	databases, _ := cmd.Flags().GetStringSlice("database")
	accountHost, _ := cmd.Flags().GetString("account-host")
	serverID, _ := cmd.Flags().GetUint32("server-id")
	grantsPath, _ := cmd.Flags().GetString("grants-output")
	overwrite, _ := cmd.Flags().GetBool("overwrite-profile")
	nonInteractive, _ := cmd.Flags().GetBool("non-interactive")
	skipDoctor, _ := cmd.Flags().GetBool("skip-doctor")
	applyMeta, _ := cmd.Flags().GetBool("apply-meta")
	adminUser, _ := cmd.Flags().GetString("admin-user")
	adminPasswordEnv, _ := cmd.Flags().GetString("admin-password-env")

	if targetAddress == "" {
		targetAddress = address
	}
	var inputReader *bufio.Reader
	if !nonInteractive {
		inputReader = bufio.NewReader(cmd.InOrStdin())
	}
	if !nonInteractive && len(databases) == 0 {
		fmt.Fprint(cmd.OutOrStdout(), "business schemas (comma-separated): ")
		line, err := inputReader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read schemas: %w", err)
		}
		for _, database := range strings.Split(line, ",") {
			if value := strings.TrimSpace(database); value != "" {
				databases = append(databases, value)
			}
		}
	}
	if err := validateInitInputs(profileName, address, targetAddress, readerUser, readerPasswordEnv, executorUser, executorPasswordEnv, accountHost, databases); err != nil {
		return err
	}
	if applyMeta && (adminUser == "" || adminPasswordEnv == "") {
		return fmt.Errorf("--apply-meta requires --admin-user and --admin-password-env")
	}
	if serverID == 0 {
		var err error
		serverID, err = randomServerID()
		if err != nil {
			return err
		}
	}
	if grantsPath == "" {
		grantsPath = filepath.Join(filepath.Dir(configPath), "unredo-"+profileName+"-grants.sql")
	}
	configAbs, err := filepath.Abs(configPath)
	if err != nil {
		return fmt.Errorf("resolve config path: %w", err)
	}
	grantsAbs, err := filepath.Abs(grantsPath)
	if err != nil {
		return fmt.Errorf("resolve grants path: %w", err)
	}
	if strings.EqualFold(configAbs, grantsAbs) {
		return fmt.Errorf("--grants-output must not overwrite the config file")
	}

	cfg, err := loadConfigForInit(configPath)
	if err != nil {
		return err
	}
	if _, exists := cfg.Profiles[profileName]; exists && !overwrite {
		return fmt.Errorf("profile %q already exists; use --overwrite-profile to replace it", profileName)
	}
	if _, err := os.Stat(grantsPath); err == nil && !overwrite {
		return fmt.Errorf("grants file %q already exists; use --overwrite-profile to replace it", grantsPath)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect grants file: %w", err)
	}
	profile := config.Profile{
		Backend: "mysql",
		Source: config.Source{
			Mode: config.SourceReplication, Address: address, User: readerUser,
			PasswordEnv: readerPasswordEnv, ServerID: serverID,
		},
		Target: config.Target{Address: targetAddress, User: executorUser, PasswordEnv: executorPasswordEnv},
		Policy: config.DefaultPolicy(),
	}
	grantSQL := buildGrantSQL(readerUser, executorUser, accountHost, databases)

	if !nonInteractive {
		fmt.Fprintf(cmd.OutOrStdout(), "\nprofile:        %s\nsource:         %s\ntarget:         %s\nserver_id:      %d\nconfig:         %s\ngrants:         %s\napply_meta:     %t\n", profileName, address, targetAddress, serverID, configPath, grantsPath, applyMeta)
		fmt.Fprint(cmd.OutOrStdout(), "Type 'yes' to write these files and continue: ")
		answer, err := inputReader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("read confirmation: %w", err)
		}
		if strings.ToLower(strings.TrimSpace(answer)) != "yes" {
			return fmt.Errorf("initialization cancelled")
		}
	}

	cfg.Profiles[profileName] = profile
	if err := config.Save(configPath, cfg); err != nil {
		return err
	}
	if err := writePrivateFile(grantsPath, []byte(grantSQL)); err != nil {
		return fmt.Errorf("write grants: %w", err)
	}
	if applyMeta {
		ctx, cancel := commandContext(cmd, 30*time.Second)
		defer cancel()
		if err := mysql.ApplyMetaMigration(ctx, targetAddress, adminUser, adminPasswordEnv); err != nil {
			return err
		}
	}

	fmt.Fprintf(cmd.OutOrStdout(), "profile_written: %s (%s)\n", profileName, configPath)
	fmt.Fprintf(cmd.OutOrStdout(), "server_id:       %d\n", serverID)
	fmt.Fprintf(cmd.OutOrStdout(), "grants_written:  %s\n", grantsPath)
	if applyMeta {
		fmt.Fprintln(cmd.OutOrStdout(), "meta_schema:     applied")
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "meta_schema:     pending (run again with --apply-meta or apply migrations/mysql/001_init.sql)")
	}
	if skipDoctor {
		fmt.Fprintln(cmd.OutOrStdout(), "doctor:          skipped; apply the grants, set password environments, then run `unredo doctor`")
		return nil
	}
	fmt.Fprintln(cmd.OutOrStdout(), "doctor:")
	return runDoctor(cmd, nil)
}

func loadConfigForInit(path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err == nil {
		if cfg.Profiles == nil {
			cfg.Profiles = make(map[string]config.Profile)
		}
		return cfg, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return &config.Config{Version: config.Version, Profiles: make(map[string]config.Profile)}, nil
}

func validateInitInputs(profile, sourceAddress, targetAddress, readerUser, readerEnv, executorUser, executorEnv, accountHost string, databases []string) error {
	for label, value := range map[string]string{
		"profile": profile, "source address": sourceAddress, "target address": targetAddress,
		"reader user": readerUser, "reader password env": readerEnv,
		"executor user": executorUser, "executor password env": executorEnv,
		"account host": accountHost,
	} {
		if value == "" {
			return fmt.Errorf("%s is required", label)
		}
	}
	if !safeName(profile) {
		return fmt.Errorf("profile %q contains unsupported characters", profile)
	}
	if !safeAccount(readerUser) || !safeAccount(executorUser) || !safeAccount(accountHost) {
		return fmt.Errorf("reader, executor, and account host may contain only letters, digits, _, ., -, $, %%")
	}
	if !safeEnv(readerEnv) || !safeEnv(executorEnv) {
		return fmt.Errorf("password environment names must use shell-safe uppercase identifiers")
	}
	if len(databases) == 0 {
		return fmt.Errorf("at least one --database is required")
	}
	for _, database := range databases {
		if !safeName(database) {
			return fmt.Errorf("database %q contains unsupported characters", database)
		}
	}
	return nil
}

func buildGrantSQL(readerUser, executorUser, host string, databases []string) string {
	dbs := append([]string(nil), databases...)
	sort.Strings(dbs)
	var out strings.Builder
	fmt.Fprintln(&out, "-- Generated by unredo init. Review as DBA before applying.")
	fmt.Fprintf(&out, "-- Create '%s'@'%s' and '%s'@'%s' with secrets from your vault first.\n", readerUser, host, executorUser, host)
	fmt.Fprintf(&out, "GRANT REPLICATION SLAVE, REPLICATION CLIENT ON *.* TO '%s'@'%s';\n", readerUser, host)
	lastDatabase := ""
	for _, database := range dbs {
		if database == lastDatabase {
			continue
		}
		lastDatabase = database
		fmt.Fprintf(&out, "GRANT SELECT ON `%s`.* TO '%s'@'%s';\n", database, readerUser, host)
		fmt.Fprintf(&out, "GRANT SELECT, INSERT, UPDATE, DELETE ON `%s`.* TO '%s'@'%s';\n", database, executorUser, host)
	}
	fmt.Fprintf(&out, "GRANT SELECT ON `unredo_meta`.* TO '%s'@'%s';\n", readerUser, host)
	fmt.Fprintf(&out, "GRANT SELECT, INSERT, UPDATE, DELETE ON `unredo_meta`.* TO '%s'@'%s';\n", executorUser, host)
	fmt.Fprintln(&out, "FLUSH PRIVILEGES;")
	return out.String()
}

func randomServerID() (uint32, error) {
	var raw [4]byte
	for {
		if _, err := rand.Read(raw[:]); err != nil {
			return 0, fmt.Errorf("generate server_id: %w", err)
		}
		value := binary.BigEndian.Uint32(raw[:])
		if value >= 10_000 {
			return value, nil
		}
	}
}

func writePrivateFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

func safeName(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("_.-$", r) {
			continue
		}
		return false
	}
	return true
}

func safeAccount(value string) bool {
	if safeName(value) {
		return true
	}
	if value == "%" {
		return true
	}
	return false
}

func safeEnv(value string) bool {
	if value == "" || (value[0] < 'A' || value[0] > 'Z') {
		return false
	}
	for _, r := range value {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}
