// Package cli wires Cobra commands. Each command file exposes a single
// NewXxx() constructor and registers itself via init().
package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// NewRoot returns the top-level command. All subcommands hang off this.
func NewRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "unredo",
		Short:         "Generate safe compensation plans from MySQL ROW binlog",
		SilenceUsage:  true,
		SilenceErrors: false,
	}
	root.PersistentFlags().String("config", "", "path to unredo.yaml (default: ./unredo.yaml)")
	root.PersistentFlags().String("profile", "default", "profile name from the config file")
	root.PersistentFlags().String("format", "table", "output format: table|json")
	root.PersistentFlags().String("log-level", "info", "log level: error|warn|info|debug")
	root.PersistentFlags().Bool("no-color", false, "disable ANSI colour in table output")
	root.PersistentFlags().String("timeout", "30s", "command-level timeout, e.g. 30s, 2m")

	for _, c := range registered() {
		root.AddCommand(c)
	}
	return root
}

// registry of subcommand constructors. Each subcommand package adds
// itself via Register() in its own init().
var builders []func() *cobra.Command

// Register wires a new subcommand.
func Register(b func() *cobra.Command) { builders = append(builders, b) }

func registered() []*cobra.Command {
	out := make([]*cobra.Command, 0, len(builders))
	for _, b := range builders {
		out = append(out, b())
	}
	return out
}

// Exit prints an error message and returns the desired exit code.
// It exists so command files don't import os directly.
func Exit(code int, format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(code)
}
