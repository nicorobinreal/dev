---
name: dev
description: Create and manage per-project development VMs on this Mac with the `dev` CLI (one Lima VM per GitHub repo, with its own Docker daemon, a devcontainer on host networking, and a Tailscale name). Use when the user wants to start working on a repo in an isolated environment, start/stop/rebuild/remove a project VM, run commands or `docker compose` e2e tests inside a project's devcontainer, get a project's Tailscale URL or HTTPS serve a port, give agents in the container access to their Chrome browser, open a project in VS Code/Cursor/Zed, or diagnose dev setup problems.
---

# dev: per-project dev VMs

`dev` gives each project its own VM (Lima, Docker, Tailscale) with the repo
cloned at `$HOME/<repo>` in the VM and a devcontainer running on the VM's host
network. Services started with `docker compose` inside the container are on
`localhost` for the container, and reachable from any tailnet device at
`http://<machine>-<name>:<port>`.

Every project has a short name (lowercase letters, digits and `-`, starting
with a letter), used by all commands.

## Rules

- **Always pass `--json`.** Parse stdout as JSON. Progress and notes go to
  stderr; ignore them unless something failed.
- **Commands never prompt.** Do not wait for input or pipe `yes`. There is no
  `--yes` flag.
- **Never add `--force` on your own.** It skips safety checks (for example it
  deletes a VM with uncommitted or unpushed work). Explain what was refused and
  ask the user first.
- `dev init` and the guided Tailscale setup in `dev doctor` are for the user to
  run in their own terminal. Tell them to run it; don't run it yourself.
- `--dry-run` shows what would happen without doing it; use it when unsure.
- `--verbose` mirrors the log to stderr. Full logs are in
  `~/.local/state/dev/logs/<date>.log`.

## Exit codes

| Code | Meaning | What to do |
| --- | --- | --- |
| 0 | Success | Use the JSON result. |
| 1 | Error | Read `error` from the JSON; fix the cause if you can (e.g. `dev doctor --json` for setup problems), otherwise report it. |
| 2 | Refused for safety (e.g. `rm` with unpushed work, a tailnet name already taken) | Explain the reason to the user. Only retry with `--force` if they explicitly agree. |
| 3 | Another `dev` invocation holds the lock for this project (or the global lock) | Wait a little and retry the same command. Don't start parallel commands on the same project. |

## Output

On success, stdout is one JSON object (or an array for list-style commands)
describing the result, e.g. the project's name, state, Tailscale host, URL or
the devcontainer kind. Field names are stable snake_case; read what you need
and don't depend on field order.

On failure, stdout is:

```json
{"error": "human-readable message", "exit_code": 2}
```

## Commands

Global flags for every command: `--json`, `--dry-run`, `--verbose`.

| Command | What it does | Flags |
| --- | --- | --- |
| `dev new <name> --repo <url>` | Create the VM, provision it, clone the repo, join the tailnet, write a starter `.devcontainer/` if the repo has none, start the devcontainer, write SSH config, open the editor | `--branch <branch>`, `--cpus <n>`, `--memory <size>` (e.g. `8GiB`), `--disk <size>`, `--no-open`, `--editor code\|cursor\|zed` |
| `dev up <name>` | Start the VM, re-sync the gh token, start the devcontainer, refresh the container's gh login | `--no-open` |
| `dev down <name>` | Revoke browser access and stop the VM (stops the container and compose services) | |
| `dev rebuild <name>` | Rebuild the devcontainer after `.devcontainer/` changes | `--no-cache` |
| `dev rm <name>` | Delete the VM, its tailnet node and SSH entries. Refuses (exit 2) with uncommitted or unpushed work | `--force` |
| `dev ls` | List projects: state, branch, Tailscale name, CPU/RAM/disk, browser access | |
| `dev open <name>` | Open the editor on the VM over Remote-SSH, in the repo folder | `--editor code\|cursor\|zed` |
| `dev exec <name> -- <cmd...>` | Run a command in the devcontainer | `--vm` (run on the VM instead) |
| `dev url <name> [port]` | Print the Tailscale URL (`http://<machine>-<name>[:port]`) | |
| `dev serve <name> <port>` | Expose a port over HTTPS with `tailscale serve`, for apps that need a secure context | `--off` |
| `dev browser grant <name>` | Let agents in the container drive the user's Chrome (MCP server at `http://localhost:<port>/mcp` in the container) | |
| `dev browser revoke <name>` | Remove that access | |
| `dev browser status <name>` | Report whether the bridge and tunnel are running | |
| `dev ssh-config` | Regenerate the `lima-<name>` (VM) and `<name>-dev` (container) SSH host entries | |
| `dev upgrade <name>` | Run provisioning steps the VM hasn't run yet | `--all` (every project) |
| `dev doctor` | Check host prerequisites | `--fix` (install what Homebrew can) |
| `dev prune` | Remove stopped VMs and orphaned tailnet nodes older than a threshold; skips VMs with uncommitted or unpushed work | `--older-than 14d`, `--force` |
| `dev self-update` | Update dev now (normally automatic) | |
| `dev self-uninstall` | Remove the binary, skills and SSH config entries; keeps VMs | `--purge` (also deletes VMs and their tailnet nodes) |

Notes:

- Put everything after `--` in `dev exec`: `dev exec shop --json -- docker compose up -d`.
  Quote shell syntax explicitly: `dev exec shop -- bash -lc 'cd web && npm test'`.
  In the container the command runs in the repo folder; with `--vm`, use absolute paths.
- `dev new` takes minutes (VM boot, provisioning, image pull). Pass `--no-open`
  unless the user wants the editor opened.
- Use `dev rebuild` after editing `.devcontainer/`. `--no-cache` only when a
  cached layer is the problem.
- `dev down` keeps everything; `dev up` brings it back.
- `dev serve` needs HTTPS certificates on the tailnet, which publishes the
  machine name in public Certificate Transparency logs. If it reports HTTPS is
  off, relay its explanation to the user rather than working around it. Plain
  `http://` from `dev url` is fine for normal dev servers.
- After `dev browser grant`, Chrome asks the user to approve each new debugging
  session; tell them to watch for it.

## Typical flows

**Start on a repo:** `dev new shop --repo https://github.com/owner/shop --no-open --json`.
Then check the devcontainer kind in the result (see below).

**e2e with compose:** `dev exec shop --json -- docker compose up -d`, then
`dev exec shop --json -- curl -fsS localhost:8080` (services are on `localhost`
inside the container). From the Mac or another device use `dev url shop 8080 --json`.

**Clean up:** `dev rm shop --json`. On exit 2, show the user what is
uncommitted or unpushed; only add `--force` if they say so.

## Custom devcontainers

`dev new` reports which `.devcontainer/` the repo has:

- **starter**: dev wrote it (or the repo already has an unmodified starter).
  Everything works. The starter is meant to be committed by the user; dev
  never commits or pushes it.
- **custom**: the repo has its own config. dev started it anyway, but some
  features may not work. The result lists the requirements.

For a custom devcontainer:

1. Read it from the VM (the workspace path is in the JSON result):
   `dev exec <name> --vm --json -- cat <workspace>/.devcontainer/devcontainer.json`
   (also any Dockerfile or compose file it references, and `<workspace>/.devcontainer.json` if that is the config).
2. Compare it with the requirements below.
3. Suggest a concrete merge to the user (a diff of `devcontainer.json`). Don't
   edit or commit it without their go-ahead; after editing, `dev rebuild <name>`.

Requirements (the starter's settings that provide each):

- **host-network**: `"runArgs": ["--network=host"]`. Compose ports are on
  `localhost`, the container sshd listens on VM port 2222, and the browser
  tunnel lands on `localhost`.
- **workspace-path**: the repo is mounted at the same path as on the VM:
  `"workspaceMount": "source=${localWorkspaceFolder},target=${localWorkspaceFolder},type=bind"`
  and `"workspaceFolder": "${localWorkspaceFolder}"`. Compose bind mounts are
  resolved by the VM's Docker daemon.
- **gh-token**: mount `source=${localEnv:HOME}/.config/dev/gh,target=/run/secrets/gh,type=bind,readonly`
  and log in on every start: `"postStartCommand": "gh auth login --with-token < /run/secrets/gh/token && gh auth setup-git"`
  (plus `"postCreateCommand": "git config --global url.https://github.com/.insteadOf git@github.com:"`).
  The image needs `gh`.
- **sshd**: sshd on port 2222 using the persisted keys mounted from
  `source=${localEnv:HOME}/.config/dev/container-ssh,target=/run/secrets/ssh,type=bind,readonly`.
  Easiest: build `FROM ghcr.io/nicorobinreal/dev-starter:v1` (it has the sshd
  feature and `/usr/local/bin/dev-sshd-keys`) and append `&& /usr/local/bin/dev-sshd-keys`
  to `postStartCommand`. Needed for the `<name>-dev` SSH host (Zed, the Claude
  and Codex desktop apps).
- **auth-volumes**: named volumes so agent logins survive rebuilds:
  `source=claude-${devcontainerId},target=/root/.claude-auth,type=volume` (with
  `"containerEnv": {"CLAUDE_CONFIG_DIR": "/root/.claude-auth"}`),
  `source=codex-${devcontainerId},target=/root/.codex,type=volume`,
  `source=opencode-${devcontainerId},target=/root/.local/share/opencode,type=volume`.

The starter also runs as root, sets `IS_SANDBOX=1`, and keeps bash history on a
`history-${devcontainerId}` volume; carry those over when the project's image
allows it.

## Inside the container

- Claude Code, Codex and OpenCode are installed and run without approval
  prompts (the VM is the security boundary). Logins happen once inside the
  container and persist in the volumes above.
- `docker` talks to the VM's Docker daemon; `gh` and `git` are logged in with
  the user's GitHub token over HTTPS (SSH-style GitHub URLs are rewritten).
- If gh says the token is invalid, run `dev up <name> --json` to re-sync it.
