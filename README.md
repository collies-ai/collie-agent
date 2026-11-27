# collie-agent

collie-agent connects your own machine to [collie](https://collies.sh), so collie can wake coding agents
(Claude Code, Codex and others) on its tasks with your own subscription, from behind NAT, without pasting tokens or
opening ports.

It runs as one background service per OS user: it holds the machine's pairing and its outbound connection to collie.

> Status: this version installs, pairs, keeps the machine online, reports the repos it finds, syncs its bindings,
> receives dispatches from collie and runs them as Claude Code or Codex sessions in their own git worktree.

## Install

macOS or Linux, amd64 or arm64:

```sh
curl -fsSL https://github.com/collies-ai/collie-agent/releases/latest/download/install.sh | sh
```

The script picks the build for your OS and CPU, verifies its SHA-256 against the release's `checksums.txt` and
installs `collie-agent` to `~/.local/bin` (`/usr/local/bin` as root). Set `COLLIE_AGENT_VERSION=v0.1.0` for a given
release or `COLLIE_AGENT_INSTALL_DIR` for another directory. Running it again upgrades in place; then run
`collie-agent service restart` so the service runs the new binary.

## Pair and run

```sh
collie-agent pair               # shows a code and opens collie in the browser; approve it there
collie-agent roots add ~/code   # optional: more folders to look for repos in (see Discovery below)
collie-agent service install    # launchd user agent on macOS, systemd user unit on Linux
collie-agent status             # pairing, connection, roots, bindings and service
```

`pair` uses the OAuth device flow (RFC 8628): it prints a code and a link, opens the browser when there is one
(`--no-browser` only prints them, e.g. over SSH), and waits while you approve the machine in collie. Use
`--server https://your-collie` for another collie and `--name` to name the machine (default: the hostname).

Once paired, `pair` asks **"Use the repos you already opened with Claude Code or Codex? [Y/n]"** (Enter means yes).
The answer goes to `config.json` as `"discover": ["claude", "codex"]` (or `[]` for no) and can be edited there later.
With it, collie-agent finds the repos you already trust in Claude Code or Codex without any root folder, and those
exact repo folders may be bound (see Discovery below). It asks only on a terminal (stdin redirected from a file or
`/dev/null` is not one), and only when `config.json` has no answer yet, so `pair --force` keeps an earlier one. No
answer (end of input) leaves it unset. `--discover claude,codex` (or `claude`, `codex`, `none`) answers
it from a script.

The machine id and token are stored in `~/.config/collie-agent/credentials.json` (or under `$XDG_CONFIG_HOME`)
with `0600` permissions. The token belongs to you, not to a project: in collie you choose which projects the
machine serves. Revoke the machine in collie to cut it off; the agent logs that collie rejected its token and stops
without retrying. To pair again, run `collie-agent pair --force`.

| Command | What it does |
| --- | --- |
| `pair [--server URL] [--name NAME] [--no-browser] [--force] [--discover claude,codex\|none]` | Pairs this machine with collie and asks about discovery. |
| `unpair` | Forgets the local pairing (revoke the machine in collie too). |
| `service install` | Installs and starts the background service (re-run it after an upgrade or a new pairing). |
| `service uninstall` | Stops and removes it. |
| `service status` | What launchd or systemd says about it (exit 0 when running). |
| `service start\|stop\|restart` (or `start`, `stop`, `restart`) | Starts, stops or restarts the installed service. A stopped service starts again at the next login. |
| `roots add\|remove <folder>`, `roots list` | Manages the root folders. A running agent picks the change up within a minute. |
| `status` | Pairing, last heartbeat, how dispatches arrive, the sessions restored at startup, roots, discovery, repos, bindings, last error and the service. |
| `run` | Runs in the foreground: what the service runs. |
| `version` | Prints the version. |

### The service

- **macOS:** `~/Library/LaunchAgents/sh.collies.collie-agent.plist`, started at login, restarted if it crashes.
  Logs: `~/Library/Logs/collie-agent.log`.
- **Linux:** `~/.config/systemd/user/collie-agent.service`, restarted on failure. Logs:
  `journalctl --user -u collie-agent -f`. A user service stops when you log out unless lingering is on:
  `loginctl enable-linger "$USER"`.

The service gets the `PATH` of the shell that ran `service install`, so it finds the same agent CLIs you do.
When collie-agent stops on purpose (the machine was revoked, or the profile is not paired) it exits cleanly and the
service manager does not restart it.

### A second instance: profiles

One instance per OS user is the default, and only one `run` per profile can run at a time. For a second one, with
its own pairing, config and service, pass `--profile <name>` (or set `COLLIE_AGENT_PROFILE`) to every command:

```sh
collie-agent --profile work pair
collie-agent --profile work service install   # sh.collies.collie-agent-work / collie-agent-work.service
```

Its files live in `~/.config/collie-agent-work`. For strong isolation, run the second instance as another OS user
instead.

## What `run` does

- **Discovery.** It finds the agent CLIs it knows (`claude`, `codex`) and their versions, and reports them in the
  heartbeat. It looks for git repos in:
  - the root folders (source `root`), up to 4 levels, skipping hidden and dependency folders;
  - with `"discover"` including `claude`: the folders in `~/.claude.json` whose trust dialog you accepted (source
    `claude`); their last use (`lastStartTime`, or the mtime of `~/.claude/projects/<folder>`) ranks clones. That file
    is not a documented interface, so when it is missing or unreadable collie-agent scans `~/Projects`, `~/code`,
    `~/src`, `~/dev` and `~/work` instead, up to 4 levels (source `scan`);
  - with `"discover"` including `codex`: the `[projects."<path>"]` of `~/.codex/config.toml` with
    `trust_level = "trusted"` (source `codex`), read with a conforming TOML decoder: a file TOML rejects trusts
    nothing.

  Missing folders, temp dirs (`/tmp`, `/var/folders`) and worktrees (`*/.claude/worktrees/*`) are skipped, and repos
  without a remote are ignored. It then asks collie for the codebases of the projects granted to this machine, and
  reports only the repos whose remote matches one. A remote matches when it is a github.com repo with the codebase's `owner/name` (https, `ssh://` or `git@host:`
  remotes; for ssh remotes the host is resolved through `~/.ssh/config` as ssh does, so an alias whose `HostName` is
  github.com counts and a `Host github.com` sent elsewhere does not): a gitlab.com remote with the same `owner/name`
  does not. Every remote is read from `.git/config` without running git; `origin` wins, and a
  clone matched through another remote (the `upstream` of a fork) is reported as such. A folder inside a repo, or a
  linked worktree, counts as its main checkout, and symlinked paths to one clone count once (a worktree whose main
  checkout lies outside the root folders is left out). When a codebase has several clones, all are reported, the
  preferred first: a main checkout over a worktree, the most recently used, the one on its default branch, then one
  matched through `origin` over one matched through another remote. Each codebase is reported once: the chosen clone,
  where it was found and the remote that matched, with the other clones as alternatives. It scans again every 10
  minutes, and right away when collie says the codebases changed (a project enabled for this machine, a codebase added
  or its repo changed): over the cable (`codebases_changed`), or through a newer `codebases_version` in the heartbeat
  answer while the cable is down. A burst of changes makes one rescan, and a report that fails is retried after 5
  seconds, doubling up to 2 minutes, instead of waiting for the next interval.
- **Bindings.** In collie you bind a codebase found on this machine to an agent. collie-agent fetches the bindings
  whenever collie's `bindings_version` changes. **The machine has the last word**: a binding whose folder is outside
  the root folders (after resolving symlinks) is refused whatever collie says, unless discovery is on and the folder is
  exactly one of the repos discovery found (never a folder inside it, and only while that symlink-free path still
  resolves to itself: a discovered folder later replaced by a symlink is refused). A repo discovery no longer finds is
  refused from the next offer, even before collie's bindings can be fetched again, and the local config can pin or
  override any binding.
- **Dispatches.** It holds a cable connection to collie (`wss://<server>/cable`, `MachineChannel`) and reconnects
  with backoff; while the cable is down it long-polls `GET /machine/v1/dispatches`. Each offer is only a job
  reference. collie-agent accepts it when the binding is usable and there is room (machine-wide and per binding), and
  declines it with a reason otherwise (also when the binding's agent CLI is not installed). Reviews count only against
  their binding's `max_concurrency`, not the machine's: builds waiting for their reviews never keep the reviews from
  starting.
- **Coordinator.** A project can run its coordinator (the agent that plans goals and reacts to their events) on this
  machine instead of collie's own Anthropic API loop. In collie, an owner binds this machine with the purpose
  `coordinator` and picks its agent (`claude` or `codex`) in the project's settings. That binding belongs to the whole
  project: it has no codebase and no folder, so it needs no root folders and no repo, and a pinned `folder` is
  ignored. collie dispatches `collie coordinate #<run>` to it, and collie-agent runs it as a coordinator session:
  - in an empty scratch dir, `<profile dir>/sessions/<id>/work` (0700), never in a repo, with no worktree or branch;
  - with collie's coordinator prompt (it calls `get_coordinator_run`, acts with collie's coordinator tools and closes
    with `finish_coordinator_run`) and the job reference in `<routine-fire-payload>`;
  - with collie's MCP tools only, whatever permission mode the binding has, and isolated from your own agent setup
    (the session's environment holds its token, so nothing of yours runs inside it):
    - **Claude Code** runs headless (`claude -p --output-format stream-json --verbose`, no Remote Control) with
      `--setting-sources ""` (none of your settings files: no hooks, plugins, permission rules or env of yours),
      `--settings` turning every hook off (`disableAllHooks`), `--tools ""` (no built-in tool: shell, files, web,
      subagents; they are also in `--disallowedTools`), `--strict-mcp-config` with only collie's server,
      `--allowedTools mcp__collie` and `--permission-mode dontAsk` when the CLI has it. Your claude.ai login still
      works (it is not a setting; an `apiKeyHelper` in your settings is not used). It exits as soon as the run is
      done, and the usage of its final `result` event goes to collie with the session's end;
    - **Codex** runs `codex exec --json --sandbox read-only --skip-git-repo-check` with approvals off,
      `--ignore-user-config` (none of your `config.toml`: your MCP servers, profiles and hooks; sign-in still comes from
      `CODEX_HOME`) so collie's server is its only MCP server, `--ignore-rules`, `--disable shell_tool`, and the
      newer exec shell, apps, plugins, hooks, browser and computer use, image tools, subagents and web search off;
    - a CLI too old for these flags refuses to start, and the session ends with that exit code: it never runs with less
      isolation;
  - outside the machine-wide `max_concurrency`, like reviews: builds waiting to be planned or unblocked never keep the
    coordinator from starting. Its binding allows one run at a time.

  Coordinator sessions are never resumed after a restart: a stop reports them finished and collie runs the
  coordinator again. `session_setup` runs for them too, under the `coordinator` purpose, in the scratch dir.
- **Sessions.** An accepted dispatch runs as one agent session:
  - a git worktree of the bound repo in `<repo>/.claude/worktrees/collie-session-<id>` (where Claude Code's own
    `--worktree` puts them; collie-agent adds `/.claude/worktrees/` to the repo's `.git/info/exclude` when it is not
    ignored already, so it never shows in `git status`), on a new branch
    `collie/session-<id>` from the remote's default branch: `origin/HEAD`, or what `origin` says its HEAD is, and
    never the branch the repo happens to be on. When neither tells, the session fails with that reason (fix it with
    `git remote set-head origin --auto`). A `collie/session-<id>` branch that already exists is never reused or
    deleted;
  - the agent CLI runs there with collie's runner prompt for the binding's purpose (worker, `qa-browser` or
    `reviewer`; a coordinator session runs outside any repo, see **Coordinator** above) and the job reference appended in `<routine-fire-payload>`;
  - it reaches collie's MCP server with a short-lived session token (1 hour, renewed 10 minutes before it expires)
    passed only in the `COLLIE_SESSION_TOKEN` environment variable: Claude Code gets an MCP config that references
    the variable, Codex `bearer_token_env_var`;
  - the binding's permission mode (from collie, or from the local config: see below) maps to Claude Code / Codex as:

    | Mode | Claude Code `--permission-mode` | Codex `--sandbox` |
    | --- | --- | --- |
    | `auto` (default) | `auto`: Claude decides what needs you and works on its own otherwise (`acceptEdits` on a Claude Code without auto mode, read from `claude --help`) | `workspace-write`, with the network on (tests that listen on localhost, `gh`) |
    | `full` | `bypassPermissions` | `danger-full-access` |
    | `plan` | `plan` | `read-only` |

  - **Claude Code** runs as an interactive session with Remote Control (`claude --remote-control <name> <prompt>`) in
    a pseudo-terminal, so it shows in claude.ai/code and the Claude app, where you can watch it, answer its
    permission prompts or step in. Inside a repo you already trust in Claude Code the worktree asks nothing (trust
    covers the folders below); otherwise collie-agent answers the workspace trust dialog "Yes, I trust this folder",
    moving to that option rather than pressing Enter on the default (in Claude Code 2.1 it is "No, exit"), and leaves
    a dialog it does not recognize to you. It reports the session's claude.ai link to collie. Remote Control needs a claude.ai login (`claude auth login`), not an API
    key; if `claude --remote-control` asks you to enable Remote Control, run it once yourself and accept. An
    interactive session does not exit on its own: hooks mark when it works and when it waits, and once it has waited
    15 minutes with nothing to do collie-agent types `/exit`. Claude reports its own usage to collie;
  - **Codex** runs headless (`codex exec --json`), and the token usage of its turns goes to collie with the session's
    end;
  - a review session, whatever its agent, gets `COLLIE_REVIEW_FILE` and `COLLIE_REVIEW_SCHEMA`: where to write its verdict
    in collie's review shape (verdict, summary, reason and findings with file, line, whether it blocks, severity and
    origin), as collie's review steps ask. Codex is also held to that shape (`--output-schema` / `-o`). collie-agent
    checks the answer and sends it to collie with the session's end;
  - output goes to `<profile dir>/sessions/<id>/agent.log`, next to the session's private files (MCP config, hooks)
    and its record, `session.json` (0600: repo, worktree, branch, the dispatch, agent, permission mode and the agent's
    own session id; never the session token). When the agent exits (or the token dies, or it sat idle) collie-agent
    reports the exit code, stops the processes still running in the worktree (whatever the agent left outside its process group, such as a `nohup … &` or a dev server that daemonizes: SIGTERM, then SIGKILL after 5 seconds) and removes the worktree and the branch it created.
    Lifecycle reports never hold the agent up.
  - **Restarts and reboots.** Claude Code sessions start with `--session-id <uuid>`, and collie-agent notes the Codex
    thread id (`thread.started`), so a session can be resumed. When the service stops (SIGTERM/SIGINT: a restart, an
    upgrade, a reboot) collie-agent stops each agent and reports it to collie as *suspended*, keeping its worktree,
    branch and record. When `run` starts again, before it takes new work, it picks up every session the last run left
    (after a graceful stop or a crash): it lists them in its first heartbeat so collie keeps them, stops whatever still
    runs in their worktrees, asks collie for a new session token (`POST sessions/:id/token/reissue`), checks that the
    worktree, its branch and the agent's transcript are still there, and runs the agent again in the same worktree
    with the same flags: `claude --resume <uuid> --remote-control …` or `codex exec resume <thread> --json …`. The
    prompt is "continue", with the instruction to first check with collie (`claim_task`, `get_goal`) that the task is
    still its own and to stop if it is not. collie hears *resumed* (with the new claude.ai link). A session that
    cannot be resumed is reported as *resume_failed* with the reason and cleaned up; one collie no longer holds is only
    cleaned up. Resumed sessions count against `max_concurrency`. `collie-agent status` lists them (`Restored:`).
  - The agent runs as your OS user, with your access: bind only repos you trust collie's work in.
- A 401 anywhere, session endpoints included, means the machine was revoked: everything stops.

### Local config

`~/.config/collie-agent/config.json` (0600) is the machine's own settings. It overrides collie:

```json
{
  "root_folders": ["~/code"],
  "discover": ["claude", "codex"],
  "max_concurrency": 2,
  "permission_mode": "auto",
  "bindings": [
    { "project": "collies", "codebase": "collie", "agents": { "code": "codex" }, "max_concurrency": 2 },
    { "runner_id": 12, "folder": "~/code/collie-agent", "permission_mode": "plan" },
    { "runner_id": 13, "disabled": true }
  ]
}
```

`session_setup` runs a command in each session's worktree before its agent starts, by the dispatch's purpose (`code`,
`research`, `reviewer`, `qa-browser`, `coordinator`: its scratch dir), with `COLLIE_SESSION_ID`, `COLLIE_PURPOSE`, `COLLIE_PROJECT` and `COLLIE_CODEBASE`
set: for example, a script of yours that gives the reviewer its own git and GitHub identity in that worktree. Its output
goes to the session's log; if it fails (or runs past two minutes) the session stops before the agent starts. collie-agent
knows nothing about what the command does.

```json
{ "session_setup": { "reviewer": "~/bin/use-review-identity" } }
```

Claude Code sessions don't push to your phone: collie-agent points `CLAUDE_CLIENT_PRESENCE_FILE` at a file in the
session's folder, so Claude Code acts as if you were at the terminal. Set `"phone_notifications": true` to get them.

`permission_mode` (`auto`, `full` or `plan`) is the mode of every binding on this machine: when it is set, the mode
chosen in collie is ignored. A pin matches a binding from collie by `runner_id`, or by `project` + `codebase`
(+ `purpose`), and replaces the fields it sets, its `permission_mode` included (it wins over the machine's);
`disabled` turns that binding off on this machine. An unknown permission mode is an error, never a silent default.
`collie-agent status` shows each binding's mode and where it comes from. A pinned folder must still be inside the root
folders or be a discovered repo. `discover` lists the discovery sources (`claude`, `codex`); `[]` turns discovery off.

The running agent rereads `config.json` at every heartbeat (30 seconds): new root folders or discovery sources make it
look for repos again right away, and pins and `permission_mode` apply to the bindings. `max_concurrency`,
`session_setup` and `phone_notifications` apply after `collie-agent service restart`. A `config.json` that does not
load is logged and the settings in use are kept.

## Develop

Go 1.24 or newer:

```sh
go build ./cmd/collie-agent
go vet ./...
go test -race ./...
shellcheck install.sh scripts/*.sh
```

Layout: `cmd/collie-agent` (entry point) and `internal/`: `cli` (commands), `pairing` (device flow), `config`
(profiles, credentials, settings, state), `machine` (the `/machine/v1` API client), `discovery` (agent CLIs, repo
scan, remote matching), `bindings` (local overrides and the root-folder check), `cable` (ActionCable client),
`dispatch` (capacity and acks), `session` (worktrees, the Claude Code and Codex adapters, prompts, token renewal), `agent` (`run`: heartbeat, inventory, bindings sync, connection, single-instance
lock), `service` (launchd and systemd), `browser`, `version`.

The tests run the device flow and the heartbeat against fake collie servers, and render the launchd plist and the
systemd unit (checked with `plutil -lint` on macOS and `systemd-analyze verify` on Linux when available).

### Commits and releases

Commits follow [Conventional Commits](https://www.conventionalcommits.org): `<type>[(scope)][!]: <description>`,
with type `feat`, `fix`, `perf`, `revert`, `build`, `chore`, `ci`, `docs`, `refactor`, `style` or `test`. CI checks
every commit of a pull request (`scripts/check-commits.sh`).

Every merge to `main` releases by itself. `.github/workflows/release.yml` runs `scripts/release.sh`, which tags
every commit on `main` since the last `v*` tag that brings a releasable change, each with the version
`scripts/next-version.sh` picks from the commits it adds:

| Commits since the last tag | Release |
| --- | --- |
| a `!` after the type, or a `BREAKING CHANGE:` footer | major (minor while the version is `0.x`) |
| `feat` | minor |
| `fix`, `perf`, `revert` | patch |
| only `build`, `chore`, `ci`, `docs`, `refactor`, `style`, `test` | none |

Then it publishes, with [GoReleaser](https://goreleaser.com) (`.goreleaser.yaml`), every `v*` tag whose release is
missing or incomplete: `collie-agent_<os>_<arch>.tar.gz` for darwin and linux on amd64 and arm64, `checksums.txt` and
`install.sh`, with a changelog grouped by type. A release run that failed, was canceled or was dropped from the queue
is therefore finished by the next run (or by running the workflow by hand on `main`). Only the highest version is
marked latest. Finally it installs that release with `install.sh` on Linux and macOS. The first release is `v0.1.0`.
To pick a version by hand, push the tag yourself:

```sh
git tag v1.0.0 && git push origin v1.0.0
```

## Protocol

collie-agent talks to collie over HTTPS:

- `POST /oauth/device_authorization` with `client_id=collie-agent`, `name`, `os`, `hostname`, then
  `POST /oauth/token` with `grant_type=urn:ietf:params:oauth:grant-type:device_code` until collie answers
  `{machine_id, machine_token}`. It waits `interval` seconds between polls and 5 more after every `slow_down`.
- `POST /machine/v1/heartbeat` with `Authorization: Bearer <machine_token>` and
  `{version, os, max_concurrency, agents, running, roots_count}` every 30 seconds (backing off up to 5 minutes on
  errors). `roots_count` is the number of root folders configured, always sent (0 when there are none), so collie can
  tell a machine with no roots from one whose roots hold no matching repo. A 401 means the machine was revoked or its
  token is invalid. collie answers `{bindings_version, codebases_version}`.
- `GET /machine/v1/codebases`,
  `PUT /machine/v1/inventory {repos: [{codebase_id, path, source, remote, alternatives: [{path, source, remote}]}]}`
  (`source` is `root`, `claude`, `codex` or `scan`; the keys after `path` are optional for collie),
  `GET /machine/v1/bindings`.
- `wss://<server>/cable` (bearer token, `Origin: <server>`, `MachineChannel`: `dispatch` and
  `{type: "codebases_changed", codebases_version}` messages), with `GET /machine/v1/dispatches?wait=25`
  as the fallback, and `POST /machine/v1/dispatches/:id/ack {accepted, reason}`.
- `POST /machine/v1/sessions/:id/token` (renew; 409 means the session is gone),
  `POST /machine/v1/sessions/:id/token/reissue` (a new token for a session resumed after a restart; 409 when collie no
  longer holds it) and `POST /machine/v1/sessions/:id/events` (`started` with the session URL, `finished` with the exit
  code, `suspended`, `resumed` with the session URL, `resume_failed` with the reason).

The full contract is in collie's docs.

## License

MIT
