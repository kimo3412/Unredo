// Command unredo is the MySQL binlog transaction compensation CLI.
//
// See DESIGN.md for the full scope. M0 ships a Cobra skeleton that wires
// the core/backend split and proves the binlog path end-to-end via
// `unredo doctor`, `unredo txn list`, and `unredo txn show`.
package main

import (
	"fmt"
	"log"
	"os"

	// Side-effect: registers the mysql backend with the registry.
	_ "github.com/girimi/unredo/internal/backends/mysql"

	"github.com/girimi/unredo/internal/cli"
)

func main() {
	// The go-mysql-org binlog library writes to the standard logger.
	// Silence it by default; users can still get full chatter via
	// --log-level=debug if we wire a handler later.
	log.SetOutput(os.Stderr)
	log.SetFlags(log.LstdFlags)
	if os.Getenv("UNREDO_BINLOG_VERBOSE") == "" {
		log.SetOutput(devNull{})
	}
	root := cli.NewRoot()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}

type devNull struct{}

func (devNull) Write(p []byte) (int, error) { return len(p), nil }
