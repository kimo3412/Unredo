// Package config loads profiles from unredo.yaml.
// Passwords are referenced, never stored in plaintext.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// Version is the schema version of the config file.
const Version = 1

// SourceMode is how a backend reads its log stream.
type SourceMode string

const (
	SourceReplication SourceMode = "replication"
	// SourceLocalFile is reserved for the post-MVP local binlog reader.
	SourceLocalFile SourceMode = "local-file"
)

// Source is the connection info for reading the backend log.
type Source struct {
	Mode        SourceMode `yaml:"mode"`
	Address     string     `yaml:"address"`
	User        string     `yaml:"user"`
	PasswordEnv string     `yaml:"password_env"`
	ServerID    uint32     `yaml:"server_id"` // 0 means "auto-resolve from profile or reject"
	// LocalFile-specific fields (post-MVP).
	BinlogPath string `yaml:"binlog_path,omitempty"`
}

// Target is the connection info for executing compensation.
type Target struct {
	Address     string `yaml:"address"`
	User        string `yaml:"user"`
	PasswordEnv string `yaml:"password_env"`
}

// Policy controls safety thresholds and timeout.
type Policy struct {
	RequireGTID            bool          `yaml:"require_gtid"`
	RequireFullRowImage    bool          `yaml:"require_full_row_image"`
	RequirePrimaryKey      bool          `yaml:"require_primary_key"`
	MaxTransactionRows     int           `yaml:"max_transaction_rows"`
	MaxTransactionBytes    int64         `yaml:"max_transaction_bytes"`
	MaxPlanBytes           int64         `yaml:"max_plan_bytes"`
	MaxActionDepth         int           `yaml:"max_action_depth"`
	LockWaitTimeout        time.Duration `yaml:"lock_wait_timeout"`
}

// Profile is a named backend configuration.
type Profile struct {
	Backend string `yaml:"backend"`
	Source  Source `yaml:"source"`
	Target  Target `yaml:"target"`
	Policy  Policy `yaml:"policy"`
}

// Config is the on-disk schema. Version must equal Version.
type Config struct {
	Version  int               `yaml:"version"`
	Profiles map[string]Profile `yaml:"profiles"`
}

// DefaultPolicy returns a conservative policy matching the M0 placeholder
// values in DESIGN.md §11. Final values must come from M0 benchmarks.
func DefaultPolicy() Policy {
	return Policy{
		RequireGTID:         true,
		RequireFullRowImage: true,
		RequirePrimaryKey:   true,
		MaxTransactionRows:  1000,
		MaxTransactionBytes: 64 * 1024 * 1024,
		MaxPlanBytes:        128 * 1024 * 1024,
		MaxActionDepth:      20,
		LockWaitTimeout:     5 * time.Second,
	}
}

// Load reads a config file and returns the parsed result.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %q: %w", path, err)
	}
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("parse config %q: %w", path, err)
	}
	if c.Version != Version {
		return nil, fmt.Errorf("unsupported config version %d, want %d", c.Version, Version)
	}
	return &c, nil
}

// Profile returns the named profile or an error listing available names.
func (c *Config) Profile(name string) (*Profile, error) {
	if len(c.Profiles) == 0 {
		return nil, errors.New("config has no profiles")
	}
	if p, ok := c.Profiles[name]; ok {
		pp := p
		if pp.Policy.MaxTransactionRows == 0 {
			pp.Policy = mergePolicy(pp.Policy, DefaultPolicy())
		}
		return &pp, nil
	}
	names := make([]string, 0, len(c.Profiles))
	for k := range c.Profiles {
		names = append(names, k)
	}
	return nil, fmt.Errorf("profile %q not found; available: %v", name, names)
}

func mergePolicy(a, b Policy) Policy {
	if a.MaxTransactionRows == 0 {
		a.MaxTransactionRows = b.MaxTransactionRows
	}
	if a.MaxTransactionBytes == 0 {
		a.MaxTransactionBytes = b.MaxTransactionBytes
	}
	if a.MaxPlanBytes == 0 {
		a.MaxPlanBytes = b.MaxPlanBytes
	}
	if a.MaxActionDepth == 0 {
		a.MaxActionDepth = b.MaxActionDepth
	}
	if a.LockWaitTimeout == 0 {
		a.LockWaitTimeout = b.LockWaitTimeout
	}
	return a
}

// ResolvePassword returns the password for a credential reference.
// It looks up the named environment variable and errors if missing.
func ResolvePassword(envName string) (string, error) {
	if envName == "" {
		return "", errors.New("password_env is empty")
	}
	v, ok := os.LookupEnv(envName)
	if !ok || v == "" {
		return "", fmt.Errorf("environment variable %q is not set or empty", envName)
	}
	return v, nil
}

// stringReader removed in favor of bytes.NewReader.
