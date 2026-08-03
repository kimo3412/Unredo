// Package buildinfo exposes release metadata injected with go build -ldflags.
package buildinfo

var (
	Version   = "0.1.0-dev"
	Commit    = "unknown"
	BuildDate = "unknown"
)
