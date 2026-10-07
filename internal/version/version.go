// Package version holds build metadata, overridden at link time:
//
//	go build -ldflags "-X github.com/wes/jokku/internal/version.Version=v0.1.0"
package version

var (
	Version = "0.0.0-dev"
	Commit  = "unknown"
)

// Repo is the GitHub repository releases and install scripts come from.
const Repo = "wes/jokku"
