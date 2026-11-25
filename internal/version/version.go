// Package version holds the build version, set by the release with -ldflags.
package version

// Version is overwritten at build time: -ldflags "-X github.com/collies-ai/collie-agent/internal/version.Version=v1.2.3".
var Version = "dev"
