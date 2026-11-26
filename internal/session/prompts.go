// Package session runs accepted dispatches: one git worktree and one agent process per session, under the agent's
// own CLI, with collie's MCP scoped to the session's token. It reports the session's lifecycle to collie, renews
// its token while the agent works and removes the worktree afterwards.
package session

import (
	_ "embed"
	"strings"
)

// The fixed prompts, copied word for word from collie's Runner::WORKER_PROMPT, QA_PROMPT and REVIEWER_PROMPT
// (jfromaniello/collie, app/models/runner.rb). The prompt lives on the machine: collie only ever sends a job
// reference. Keep them in sync with collie when they change.
var (
	//go:embed prompts/worker.txt
	workerPrompt string
	//go:embed prompts/qa.txt
	qaPrompt string
	//go:embed prompts/reviewer.txt
	reviewerPrompt string
	// resumePrompt is what a session resumed after a restart is told: continue, once collie confirms the work is
	// still its own.
	//go:embed prompts/resume.txt
	resumePrompt string
	// reviewSchema is the shape of a review agent's final answer, for agents that take one (codex --output-schema).
	//go:embed prompts/review.schema.json
	reviewSchema []byte
)

// PromptFor returns the fixed prompt for a purpose (code and research share the worker prompt, which handles
// "collie work_goal" and "collie research"), followed by the job reference in the <routine-fire-payload> block the
// prompt waits for.
func PromptFor(purpose, payload string) string {
	var p string
	switch purpose {
	case "qa-browser":
		p = qaPrompt
	case "reviewer":
		p = reviewerPrompt
	default:
		p = workerPrompt
	}
	return strings.TrimRight(p, "\n") + "\n\n<routine-fire-payload>\n" + payload + "\n</routine-fire-payload>\n"
}
