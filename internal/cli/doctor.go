package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/girimi/unredo/internal/backends/mysql"
	"github.com/girimi/unredo/internal/backends/mysql/doctor"
	"github.com/girimi/unredo/internal/config"
	"github.com/girimi/unredo/internal/registry"
)

func init() { Register(newDoctorCmd) }

func newDoctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Check MySQL prerequisites, permissions, and binlog/GTID configuration",
		RunE:  runDoctor,
	}
}

func runDoctor(cmd *cobra.Command, _ []string) error {
	profileName, _ := cmd.Flags().GetString("profile")
	cfgPath, _ := cmd.Flags().GetString("config")
	if cfgPath == "" {
		cfgPath = "unredo.yaml"
	}
	timeout, _ := cmd.Flags().GetString("timeout")
	dur, err := time.ParseDuration(timeout)
	if err != nil {
		return fmt.Errorf("invalid --timeout: %w", err)
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	p, err := cfg.Profile(profileName)
	if err != nil {
		return err
	}
	be, err := registry.Resolve(p)
	if err != nil {
		return err
	}
	mbe, ok := be.(*mysql.Backend)
	if !ok {
		return fmt.Errorf("doctor: backend %q does not support doctor", be.Name())
	}

	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	report, err := mbe.RunDoctor(ctx, &doctor.Deps{Context: ctx, Timeout: dur})
	if err != nil {
		return err
	}
	fatal := false
	for _, c := range report.Checks {
		marker := "OK"
		switch c.Severity {
		case doctor.SeverityWarn:
			marker = "WARN"
		case doctor.SeverityError:
			marker = "ERROR"
			fatal = true
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%-22s %-6s %s\n", c.Name, marker, c.Message)
	}
	if fatal {
		return fmt.Errorf("doctor found blocking issues")
	}
	return nil
}
