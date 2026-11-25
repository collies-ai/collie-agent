# collie-agent

collie-agent connects your own machine to [collie](https://collies.sh), so collie can wake coding agents
(Claude Code, Codex and others) on its tasks with your own subscription, from behind NAT, without pasting tokens or
opening ports.

It runs as one background service per OS user: it holds the machine's pairing and its outbound connection to collie.

> Status: this version installs, pairs, keeps the machine online, reports the repos it finds, syncs its bindings and
> receives dispatches from collie. Running agent sessions comes next: until then every dispatch is declined with that
> reason, so collie gives the work to another runner.

## Install

macOS or Linux, amd64 or arm64:

```sh
curl -fsSL https://github.com/collies-ai/collie-agent/releases/latest/download/install.sh | sh
```

The script picks the build for your OS and CPU, verifies its SHA-256 against the release's `checksums.txt` and
installs `collie-agent` to `~/.local/bin` (`/usr/local/bin` as root). Set `COLLIE_AGENT_VERSION=v0.1.0` for a given
release or `COLLIE_AGENT_INSTALL_DIR` for another directory. Running it again upgrades in place; then run
`collie-agent service install` again so the service restarts on the new binary.

## Pair and run

```sh
collie-agent pair               # shows a code and opens collie in the browser; approve it there
collie-agent roots add ~/code   # where your repos live: the only folders collie-agent looks in or works in
collie-agent service install    # launchd user agent on macOS, systemd user unit on Linux
collie-agent status             # pairing, connection, roots, bindings and service
```

`pair` uses the OAuth device flow (RFC 8628): it prints a code and a link, opens the browser when there is one
(`--no-browser` only prints them, e.g. over SSH), and waits while you approve the machine in collie. Use
`--server https://your-collie` for another collie and `--name` to name the machine (default: the hostname).

The machine id and token are stored in `~/.config/collie-agent/credentials.json` (or under `$XDG_CONFIG_HOME`)
with `0600` permissions. The token belongs to you, not to a project: in collie you choose which projects the
machine serves. Revoke the machine in collie to cut it off; the agent logs that collie rejected its token and stops
without retrying. To pair again, run `collie-agent pair --force`.

| Command | What it does |
| --- | --- |
| `pair [--server URL] [--name NAME] [--no-browser] [--force]` | Pairs this machine with collie. |
| `unpair` | Forgets the local pairing (revoke the machine in collie too). |
| `service install` | Installs and starts the background service (re-run it after an upgrade or a new pairing). |
| `service uninstall` | Stops and removes it. |
| `service status` | What launchd or systemd says about it (exit 0 when running). |
| `roots add\|remove <folder>`, `roots list` | Manages the root folders (restart the service after a change). |
| `status` | Pairing, last heartbeat, how dispatches arrive, roots, repos, bindings, last error and the service. |
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
  heartbeat. It scans the root folders (up to 4 levels, skipping hidden and dependency folders) for git repos, asks
  collie for the codebases of the projects granted to this machine, and reports only the repos whose remote matches
  one (`owner/name`, from https, `ssh://` or `git@host:` remotes, SSH host aliases included). It scans again every 10
  minutes.
- **Bindings.** In collie you bind a codebase found on this machine to an agent. collie-agent fetches the bindings
  whenever collie's `bindings_version` changes. **The machine has the last word**: a binding whose folder is outside
  the root folders (after resolving symlinks) is refused whatever collie says, and the local config can pin or
  override any binding.
- **Dispatches.** It holds a cable connection to collie (`wss://<server>/cable`, `MachineChannel`) and reconnects
  with backoff; while the cable is down it long-polls `GET /machine/v1/dispatches`. Each offer is only a job
  reference. collie-agent accepts it when the binding is usable and there is room (machine-wide and per binding), and
  declines it with a reason otherwise.
- A 401 anywhere means the machine was revoked: everything stops.

### Local config

`~/.config/collie-agent/config.json` (0600) is the machine's own settings. It overrides collie:

```json
{
  "root_folders": ["~/code"],
  "max_concurrency": 2,
  "bindings": [
    { "project": "collies", "codebase": "collie", "agents": { "code": "codex" }, "max_concurrency": 2 },
    { "runner_id": 12, "folder": "~/code/collie-agent", "permission_mode": "plan" },
    { "runner_id": 13, "disabled": true }
  ]
}
```

A pin matches a binding from collie by `runner_id`, or by `project` + `codebase` (+ `purpose`), and replaces the
fields it sets; `disabled` turns that binding off on this machine. A pinned folder must still be inside the root
folders. Restart the service after editing it.

## Develop

Go 1.24 or newer:

```sh
go build ./cmd/collie-agent
go vet ./...
go test -race ./...
shellcheck install.sh
```

Layout: `cmd/collie-agent` (entry point) and `internal/`: `cli` (commands), `pairing` (device flow), `config`
(profiles, credentials, settings, state), `machine` (the `/machine/v1` API client), `discovery` (agent CLIs, repo
scan, remote matching), `bindings` (local overrides and the root-folder check), `cable` (ActionCable client),
`dispatch` (capacity and acks), `agent` (`run`: heartbeat, inventory, bindings sync, connection, single-instance
lock), `service` (launchd and systemd), `browser`, `version`.

The tests run the device flow and the heartbeat against fake collie servers, and render the launchd plist and the
systemd unit (checked with `plutil -lint` on macOS and `systemd-analyze verify` on Linux when available).

### Release

Push a `v*` tag. `.github/workflows/release.yml` runs the tests and [GoReleaser](https://goreleaser.com)
(`.goreleaser.yaml`), which publishes `collie-agent_<os>_<arch>.tar.gz` for darwin and linux on amd64 and arm64,
`checksums.txt` and `install.sh` to the GitHub release.

```sh
git tag v0.1.0 && git push origin v0.1.0
```

## Protocol

collie-agent talks to collie over HTTPS:

- `POST /oauth/device_authorization` with `client_id=collie-agent`, `name`, `os`, `hostname`, then
  `POST /oauth/token` with `grant_type=urn:ietf:params:oauth:grant-type:device_code` until collie answers
  `{machine_id, machine_token}`. It waits `interval` seconds between polls and 5 more after every `slow_down`.
- `POST /machine/v1/heartbeat` with `Authorization: Bearer <machine_token>` and
  `{version, os, max_concurrency, agents, running}` every 30 seconds (backing off up to 5 minutes on errors). A 401
  means the machine was revoked or its token is invalid.
- `GET /machine/v1/codebases`, `PUT /machine/v1/inventory {repos: [{codebase_id, path}]}`, `GET /machine/v1/bindings`.
- `wss://<server>/cable` (bearer token, `Origin: <server>`, `MachineChannel`), with `GET /machine/v1/dispatches?wait=25`
  as the fallback, and `POST /machine/v1/dispatches/:id/ack {accepted, reason}`.

The full contract is in collie's docs.

## License

MIT
