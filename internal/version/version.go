// Package version holds build metadata injected with -ldflags.
package version

// Set at build time:
//
//	go build -ldflags "-X github.com/ehilzinger/kwerft/internal/version.Version=0.1.0 -X github.com/ehilzinger/kwerft/internal/version.Commit=abc123"
var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
)
