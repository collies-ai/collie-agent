// Package discovery finds what this machine can offer collie: the agent CLIs installed and the git repos under the
// root folders whose remote matches one of collie's codebases.
package discovery

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/collies-ai/collie-agent/internal/machine"
)

// KnownAgents are the agent CLIs collie-agent has (or will have) an adapter for, by the name collie uses.
var KnownAgents = []string{"claude", "codex"}

// LookPath and Version are replaced by tests.
var (
	LookPath = exec.LookPath
	Version  = cliVersion
)

// FindAgents returns the known agent CLIs found in PATH, with the first line of `<cli> --version`.
func FindAgents(ctx context.Context) []machine.Agent {
	var out []machine.Agent
	for _, name := range KnownAgents {
		path, err := LookPath(name)
		if err != nil {
			continue
		}
		out = append(out, machine.Agent{Name: name, Version: Version(ctx, path)})
	}
	return out
}

func cliVersion(ctx context.Context, path string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, path, "--version").Output()
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(b)), "\n")
	if len(line) > 100 {
		line = line[:100]
	}
	return strings.TrimSpace(line)
}
