# dev CLI — Implementation Plan

Nov 11, 2026 · @Albin

## Overview

`dev` is a Go CLI for macOS that gives each project its own Lima VM, with its own Docker daemon, a devcontainer on host networking, and a Tailscale address. It replaces Docker-outside-of-Docker on the Mac and removes localhost port congestion. Agents can run `docker compose` e2e tests without shared networks.

**Goals**

- One command to go from a GitHub repo to a running, VS Code-ready dev environment.
- Agents (Claude Code, Codex, OpenCode) start in a version-controlled devcontainer and can reach every compose service on `localhost`.
- Every project is reachable from any tailnet device by name, with no Mac port forwarding.
- Installable on any Mac with one script; updates itself; ships an agent skill so host agents can drive it.

**Scope (v1)**

- macOS (Apple Silicon) host only. Lima with `vmType: vz`.
- GitHub only. One repo per VM.
- Full (trusted) credentials: the host's `gh` token is copied into each VM and container. There are no scoped-credential modes.

**Non-goals**

- Linux or Windows hosts, non-GitHub remotes, multi-repo VMs.
- SSH agent forwarding for git (git rides on the gh token over HTTPS).
- Managing `.env` or other app secrets.
- Per-repo `dev` config (no `.dev.toml`); VM resources come from flags and global defaults.
- VM snapshots and backups.
- The Pi harness.
- Commands to start agents, manage gh tokens per project, or open shells (`dev exec` covers it).

## Architecture

The Mac holds only credentials and user interfaces; everything that runs lives in the project's VM.

&#91;embedded content: architecture · Mac, per-project VM, devcontainer\]

The devcontainer and the compose services share the VM's network, so agents reach every service on `localhost`. The Mac's editors and agent apps connect over SSH, either to the VM or straight into the container's sshd. Other devices reach the VM by name over Tailscale.

## Commands

Every project has a short name, used by all commands and for everything local to the Mac (Lima instance, SSH hosts, state, locks, browser jobs). Only names shared across Macs carry the machine prefix: the Tailscale hostname is `<machine>-<name>` (see VM provisioning).

| Command | What it does | Flags |
| --- | --- | --- |
| `dev new <name> --repo <url>` | Create VM, provision, clone repo, join tailnet, write starter `.devcontainer` if none exists, start the devcontainer, write SSH config, open the editor | `--branch`, `--cpus`, `--memory`, `--disk`, `--no-open`, `--editor code\|cursor\|zed` |
| `dev up <name>` | Start VM, re-sync the gh token, start the devcontainer (`devcontainer up`), refresh the container's gh login | `--no-open` |
| `dev down <name>` | Revoke browser access, stop the VM (stops the container and compose services with it) |  |
| `dev rebuild <name>` | Rebuild the devcontainer after config changes (also possible from VS Code, see `dev open`) | `--no-cache` |
| `dev rm <name>` | Refuse if there is uncommitted or unpushed work; revoke browser access, delete the VM, remove the tailnet node and SSH config entries | `--force` |
| `dev ls` | List projects: state, branch, Tailscale name, CPU/RAM/disk, browser access |  |
| `dev open <name>` | Open the editor on the VM over Remote-SSH, in the repo folder | `--editor` |
| `dev exec <name> -- <cmd>` | Run a command in the devcontainer (`devcontainer exec`) | `--vm` (run on the VM instead) |
| `dev url <name> [port]` | Print the Tailscale URL |  |
| `dev serve <name> <port>` | Optional: expose a port via `tailscale serve` with HTTPS, for apps that need a secure context (needs HTTPS certificates on the tailnet, see Tailscale details) | `--off` |
| `dev browser grant\|revoke\|status <name>` | Start/stop the Chrome MCP bridge and reverse tunnel |  |
| `dev ssh-config` | Regenerate the `lima-<name>` and `<name>-dev` SSH host entries |  |
| `dev upgrade <name>` | Run provisioning steps the VM hasn't run yet | `--all` |
| `dev doctor` | Check host prerequisites, offer to install missing ones, guide the one-time Tailscale setup | `--fix` |
| `dev prune` | Remove stopped VMs and orphaned tailnet nodes older than a threshold; skips VMs with uncommitted or unpushed work | `--older-than 14d`, `--force` |
| `dev self-update` | Update now (normally automatic) |  |
| `dev self-uninstall` | Remove binary, skills, SSH config entries; keep VMs unless asked | `--purge` (also deletes VMs and their tailnet nodes) |

**Global flags:** `--json` (machine-readable output for the skill), `--dry-run`, `--verbose`. There is no `--yes`: commands never prompt (agents call them with `--json`), except `dev init` and the guided Tailscale setup in `dev doctor`, which a person runs. Risky actions are unlocked with `--force`.

**Exit codes:** 0 success, 1 error, 2 refused for safety (e.g. `rm` with unpushed work), 3 lock held by another invocation.

## VM provisioning

`dev` starts from an Ubuntu LTS cloud image (the release Lima's default template uses, with its URL and checksum pinned in the binary and bumped on purpose through a dev upgrade step) and installs everything itself after first boot. It does not use Lima's `docker` templates.

**Naming:**

- Lima instance: `<name>`, e.g. `shop`. SSH hosts: `lima-<name>` (the VM) and `<name>-dev` (the container).
- Tailscale hostname: `<machine>-<name>`, e.g. `mbp-shop`, so two Macs can each have a `shop`. `<machine>` is set at install (default: short hostname).
- `dev new` validates the name: lowercase letters, digits and `-`, starting with a letter, and `<machine>-<name>` at most 63 characters (a valid DNS label and Lima instance name).
- Before joining, `dev new` checks the tailnet for an existing node named `<machine>-<name>` and refuses (exit 2) rather than letting Tailscale rename the new node to `<machine>-<name>-1`.

**Lima config generated per VM:**

- `vmType: vz`, `mountType: virtiofs`, Rosetta enabled.
- `mounts: []` (no Mac folders shared into the VM).
- `containerd: {system: false, user: false}` (Lima's default would install containerd and nerdctl).
- Port auto-forwarding to the Mac turned off (access goes through Tailscale).
- CPU, memory and disk from flags, then global defaults.

**VM user:** Lima creates a regular (non-root) user with the same name as the Mac user, with passwordless `sudo`. Its home is typically `/home/<user>.linux`; `dev` reads `$HOME` from the VM instead of hardcoding it. Docker runs rootful, and the user is added to the `docker` group so the devcontainer CLI can reach the socket.

**Provisioning steps (ordered, idempotent, each with a stable ID):**

1. Base packages, time sync, unattended security upgrades.
2. Docker Engine (rootful) and the Compose plugin; add the Lima user to the `docker` group.
3. git and gh. Global git config: `url."https://github.com/".insteadOf git@github.com:`.
4. Node LTS and `@devcontainers/cli` (used by `dev up`, `dev rebuild`, `dev exec`).
5. gh token written to `~/.config/dev/gh/token` (mode 600); `gh auth login --with-token` and `gh auth setup-git` on the VM so git can clone over HTTPS.
6. Clone the repo to `$HOME/<repo>` over HTTPS.
7. Tailscale, joined as `<machine>-<name>` with a freshly minted auth key (see Credentials).
8. sshd host keys and `authorized_keys` for the container written to `~/.config/dev/container-ssh/`.

The VM records the IDs of every step it has run in `/etc/dev/provision-steps` (one per line), not just the highest number, so steps can be added in any order during development. `dev upgrade` runs any known step missing from that file. A later optimisation: provision once into a base VM and copy it for each `dev new`.

The token write in step 5 is also repeated on every `dev up` (see Credentials); that re-sync is a normal `up` action, not a provisioning step.

## Credentials

All credentials are full-access. There are no per-project scopes and no extra flags.

| Credential | Lives on the Mac | Reaches the VM | Reaches the container |
| --- | --- | --- | --- |
| GitHub (git + gh) | `gh auth token` from the host's gh login | `dev new` and every `dev up` write it to `~/.config/dev/gh/token` (600) and refresh the VM's gh login | The folder `~/.config/dev/gh/` is bind-mounted read-only at `/run/secrets/gh`; `postStartCommand` logs gh in from it on every container start, and `dev up` runs the same login via `devcontainer exec` |
| Tailscale | OAuth client ID and secret in the macOS Keychain (does not expire) | Single-use, pre-authorized, tagged auth key minted per VM; the node stays joined across restarts | Not needed (host networking) |
| Agent logins (Claude, Codex, OpenCode) | Nothing | Nothing | You log in inside the container once; auth persists in named volumes |
| Container SSH (for Claude/Codex apps and `<name>-dev`) | Lima's own key, `~/.lima/_config/user` | `~/.config/dev/container-ssh/authorized_keys` (Lima's public key) + persistent host keys | Bind-mounted into sshd |

**gh token details**

- The container keeps its own saved copy of the gh login, so a new token on the VM only takes effect when gh logs in again. That is why the login runs on every container start and on every `dev up` (the container may already be running, in which case `postStartCommand` doesn't fire).
- The folder is mounted, not the file: a single-file bind mount is pinned to the file's inode, and `dev` writes the token atomically (temp file + rename), which would leave the container seeing the old file.
- Persistent path, not `/tmp`: `/tmp` is cleared on VM reboot, and a missing bind-mount source makes Docker create an empty directory, which breaks the container.
- Git uses HTTPS only. The `insteadOf` rule rewrites SSH-style GitHub URLs, including submodules.

**Container SSH key:** `dev` reuses the key Lima already generates for itself (`~/.lima/_config/user`). Its public half goes into the container's `authorized_keys`, and the `<name>-dev` SSH entry sets `IdentityFile` to it. Nothing to choose, and it works on a Mac with no SSH key of its own.

**Tailscale details**

- One-time setup, done manually in the Tailscale admin console; `dev doctor` prints a step-by-step guide and then stores the result:
  1. Policy file: `tagOwners` gives `tag:devvm` to `autogroup:admin`; a grant lets your own devices reach `tag:devvm`; nothing lets `tag:devvm` reach other devices. Remove the default allow-all rule, or `tag:devvm` can reach everything.
  2. DNS: MagicDNS on. HTTPS certificates stay **off** by default (see below).
  3. OAuth client with scopes **Auth Keys (write)** and **Devices Core (read and write)**, tagged `tag:devvm`.
  4. `dev doctor` stores the client ID and secret with `/usr/bin/security add-generic-password -s dev-tailscale -a client-id|client-secret`.
- `dev` reads the Keychain by shelling out to `/usr/bin/security find-generic-password -s dev-tailscale -a <account> -w`, never through the Security framework. Items added by `security` trust that tool, so a self-updated `dev` binary doesn't trigger a Keychain GUI prompt (which an agent can't click).
- Auth keys are minted via the API (OAuth client-credentials flow, which needs both the client ID and the secret). The OAuth secret itself never leaves the Mac.
- **HTTPS is optional.** `dev url` prints `http://<machine>-<name>:<port>`; tailnet traffic is already encrypted by WireGuard, so plain HTTP is fine for normal dev servers and APIs. HTTPS certificates are only needed when a browser demands a secure context (service workers/PWAs, clipboard, camera/mic, WebAuthn, `Secure` cookies, OAuth providers requiring `https://` redirects, testing on a phone). Enabling them publishes the name of every machine that gets a certificate (e.g. `mbp-shop.<tailnet>.ts.net`) in public Certificate Transparency logs, permanently, so it's left off until a project needs it. `dev serve` checks and, if HTTPS is off, explains how to enable it (DNS page → HTTPS Certificates) and that the node's name will be published.
- Nodes are non-ephemeral. `dev rm` looks up the node ID by hostname and deletes it via the API using the same OAuth client; `dev prune` uses the read scope to find orphans.

**Agent login volumes** (one set per project via `${devcontainerId}`):

- Claude Code: volume at `/root/.claude-auth` with `CLAUDE_CONFIG_DIR` pointing there (keeps `~/.claude.json` inside it too).
- Codex: volume at `/root/.codex`.
- OpenCode: volume at `/root/.local/share/opencode` (where it stores auth).

`containerEnv` applies to harnesses started inside the container (terminal, `dev exec`). A harness connecting over SSH (e.g. the Claude desktop app) uses its own environment. Codex is unaffected, since its volume is its default config dir. Claude over SSH stores its login under `/root/.claude`, which is lost on rebuild; if that becomes annoying, also set `CLAUDE_CONFIG_DIR` in `/etc/environment` in the image.

## Starter .devcontainer

`dev new` writes a starter `.devcontainer/` into the clone when the repo has none. It is meant to be committed to the project, but `dev` never commits or pushes it.

**Three cases on `dev new`** (the devcontainer is started in all three, for consistency):

1. **No `.devcontainer/`** → write the starter, start the container.
2. **`.devcontainer/` matches a known starter version** (e.g. the same project cloned on another Mac) → start the container.
3. **Anything else** → leave it untouched and start it anyway. The output (and `--json`) flags it as a custom devcontainer that may lack what `dev` relies on (host network, same workspace path, gh token mount, sshd, auth volumes), so some features may not work. When `new` was run through the skill, the harness then reads the existing `.devcontainer/` (`dev exec <name> --vm -- cat ...`) and suggests how to merge in the starter's requirements.

**Detecting a known starter:** the binary embeds the SHA-256 of `.devcontainer/` contents for every released starter version. `devcontainer.json` starts with a `// dev starter vN` comment so a person can see it too.

**Uncommitted starter and `dev rm`:** an untracked `.devcontainer/` that is byte-identical to an embedded starter does not count as uncommitted work, so `rm` on a fresh project doesn't refuse.

**Starter `devcontainer.json` (sketch):**

```jsonc
// dev starter v1
{
  "name": "${localWorkspaceFolderBasename}",
  "build": { "dockerfile": "Dockerfile" },
  // gh, sshd and docker-outside-of-docker are baked into the base image
  "containerUser": "root",
  "remoteUser": "root",
  "runArgs": ["--network=host"],
  "workspaceMount": "source=${localWorkspaceFolder},target=${localWorkspaceFolder},type=bind",
  "workspaceFolder": "${localWorkspaceFolder}",
  "mounts": [
    "source=${localEnv:HOME}/.config/dev/gh,target=/run/secrets/gh,type=bind,readonly",
    "source=${localEnv:HOME}/.config/dev/container-ssh,target=/run/secrets/ssh,type=bind,readonly",
    "source=claude-${devcontainerId},target=/root/.claude-auth,type=volume",
    "source=codex-${devcontainerId},target=/root/.codex,type=volume",
    "source=opencode-${devcontainerId},target=/root/.local/share/opencode,type=volume",
    "source=history-${devcontainerId},target=/commandhistory,type=volume"
  ],
  "containerEnv": {
    "IS_SANDBOX": "1",
    "CLAUDE_CONFIG_DIR": "/root/.claude-auth",
    "HISTFILE": "/commandhistory/.bash_history"
  },
  "initializeCommand": "test -s $HOME/.config/dev/gh/token || (echo 'run dev up' && exit 1)",
  "postCreateCommand": "git config --global url.https://github.com/.insteadOf git@github.com:",
  "postStartCommand": "gh auth login --with-token < /run/secrets/gh/token && gh auth setup-git && /usr/local/bin/dev-sshd-keys"
}
```

**Key choices:**

- **Same workspace path as the VM.** Compose bind mounts (`./src:/app`) are resolved by the VM's Docker daemon, so the container must see the repo at the identical path.
- **Host networking.** Compose services' published ports are on `localhost` for agents and the dev server; the container sshd listens on port 2222 on the VM.
- **`initializeCommand`** runs on the VM and fails fast with a clear message when the token file is missing.
- **gh login in `postStartCommand`** (not `postCreateCommand`), so every container start picks up the current token.
- **`dev-sshd-keys`** (small script in the image) points sshd at the persisted host keys and installs `authorized_keys`, so rebuilds don't change the container's SSH identity.

**Starter `Dockerfile`:** `FROM ghcr.io/nicorobinreal/dev-starter:v1`, with room for project-specific lines below it. The prebuilt image runs as root only (no `vscode` user) and holds a Debian base, Node LTS, Go, Claude Code, Codex, OpenCode, the baked-in features and `dev-sshd-keys`. A new VM pulls it in seconds; only lines a project adds are built locally. Logins happen inside the container and persist in the volumes above. `DEV_STARTER_IMAGE` overrides the image (used by e2e before the image is published).

**Harnesses run without approval prompts** (the VM is the security boundary):

| Harness | Setting in the image |
| --- | --- |
| Claude Code | `IS_SANDBOX=1` in `containerEnv` (lets the bypass flag run as root) + `alias claude='claude --dangerously-skip-permissions'` in `/root/.bashrc` |
| Codex | `alias codex='codex --dangerously-bypass-approvals-and-sandbox'` (its config dir is a volume, so a flag beats `config.toml`) |
| OpenCode | `/root/.config/opencode/opencode.json` with `"permission": "allow"` (it has no bypass flag; verify the string form is accepted, else set each permission to `"allow"`) |

Aliases only apply in interactive shells. The Claude and Codex desktop apps, connecting over SSH, use their own permission settings.

**Bash history:** `PROMPT_COMMAND='history -a'` in `/root/.bashrc`, with `HISTFILE` on the per-project history volume, so history survives rebuilds.

## Editor access

`dev open` connects the editor to the **VM** over Remote-SSH (`lima-<name>`) and opens the repo folder. From there VS Code's Dev Containers extension reopens it in the container, and you can rebuild the container from inside VS Code after editing the Dockerfile, without running `dev rebuild` on the host and reconnecting. Cursor works the same way. Zed has no Dev Containers support, so `--editor zed` connects to `<name>-dev` (the container's sshd) instead.

The container's sshd (`<name>-dev`) is still there for the Claude and Codex desktop apps and anything else that wants to land directly in the container.

## Browser access

Agents in a container can drive your real Chrome profile, with your logins, while you watch. Access is granted and revoked per project.

**One-time setup (`dev doctor` checks it):** Chrome 144 or later, with remote debugging turned on at `chrome://inspect/#remote-debugging`; Node on the Mac (for `npx chrome-devtools-mcp`).

**`dev browser grant <name>`**

1. Pick a free port for the project and store it in project state.
2. Write and load two launchd user agents, `dev.browser.<name>.bridge` and `dev.browser.<name>.tunnel`, in `~/Library/LaunchAgents/`, both with `KeepAlive`:
   - **bridge:** runs `chrome-devtools-mcp --autoConnect` behind a bridge built into dev (official MCP Go SDK: stdio to the child process, Streamable HTTP outward; mcp-proxy is the fallback) listening on `127.0.0.1:<port>`.
   - **tunnel:** runs `ssh -N -R <port>:127.0.0.1:<port> -o ServerAliveInterval=15 -o ServerAliveCountMax=2 -o ExitOnForwardFailure=yes lima-<name>`. Host networking makes it `localhost:<port>` in the container.
   launchd restarts either job after sleep, a dropped network or a reboot; the ssh options make a dead tunnel exit quickly so launchd can start a fresh one.
3. Register the MCP server with the agents in the container, HTTP transport, `http://localhost:<port>/mcp`: `claude mcp add` and `codex mcp add` via `dev exec`; for OpenCode (whose `mcp add` is interactive), write the entry into `/root/.config/opencode/opencode.json`.
4. Chrome asks you to approve each new debugging session.

**`dev browser revoke <name>`** unloads and removes both launchd jobs, then removes the MCP registration. `status` reports whether each piece is running (via `launchctl print`). `dev down` and `dev rm` revoke automatically.

## Distribution and updates

A public GitHub repo, `github.com/nicorobinreal/dev`; releases are built by GoReleaser in GitHub Actions (darwin/arm64, plus darwin/amd64 if cheap) with a `checksums.txt`.

**Starter image:** the same release workflow runs `devcontainer build --push` to publish `ghcr.io/nicorobinreal/dev-starter:vN` (linux/arm64 and linux/amd64; build arm64 on GitHub's arm64 runners rather than under QEMU). The gh, sshd and docker-outside-of-docker features are baked in, so their settings travel in the image's devcontainer metadata. A new starter version bumps the image tag and adds the starter's hash to the binary.

- **No repo secrets needed:** the workflow logs in to ghcr with the built-in `GITHUB_TOKEN`, using `permissions: { contents: write, packages: write }`.
- **One-time manual step:** new ghcr packages start private. After the first push, set `dev-starter` to public in its package settings (github.com/users/nicorobinreal/packages), so VMs can pull it without auth.

**Install script** (`curl -fsSL https://raw.githubusercontent.com/nicorobinreal/dev/main/install.sh | sh`):

1. Download the latest release binary, verify it against `checksums.txt`, install to `~/.local/bin/dev`.
2. First-run setup (interactive, re-runnable as `dev init`): machine prefix, which harnesses get the skill, default VM resources.
3. Install the skill (below), then run `dev doctor`.

**Auto-update**

- On each invocation, if the last check is older than 1 hour, spawn a detached `dev self-update --background` subprocess (a goroutine would die when the command exits) that checks the GitHub releases API (unauthenticated limit is 60 requests an hour, hence the cache).
- A newer version is downloaded, checksum-verified and swapped in atomically (write to a temp file, `rename`). The current run is unaffected; the next run uses the new version.
- After a swap, the next run prints one line: `dev updated vX → vY`.
- `DEV_NO_UPDATE=1` turns this off; `dev self-update` forces it.
- Update steps run under the global lock so two concurrent invocations don't both swap.

**Skill install**

- The skill (`SKILL.md` plus any references) is embedded in the binary and written out on install and after every update.
- Always written to `~/.agents/skills/dev/` (read natively by Codex and several others).
- Also written to each selected harness's own folder when it doesn't read the shared one. The mapping lives in one table in the code:

| Harness | Global skills folder | Reads `~/.agents/skills` |
| --- | --- | --- |
| Claude Code | `~/.claude/skills/` | No |
| Codex | `~/.agents/skills/` | Yes |
| OpenCode | `~/.config/opencode/skills/` | Yes |

**Reloading after a skill change:** Claude Code watches existing skill files and picks up edits on its own; only new skill folders need a reload. Since updates only edit the existing `dev` skill, Claude Code needs nothing. For other harnesses, when the skill's hash changed, print `skill updated — reload skills in <harness>` on the next run.

**What the skill tells the harness:** when to use `dev`; to always pass `--json`; how to read the output; that after `dev new` reports a custom `.devcontainer`, it should read it from the VM (`dev exec <name> --vm -- cat ...`), compare it with the starter's requirements (host network, same workspace path, gh token mount, sshd, auth volumes) and suggest a merge to the user.

## Cross-cutting concerns

**Config and state**

- `~/.config/dev/config.toml`: machine prefix, selected harnesses, default CPU/RAM/disk, Tailscale tag, editor.
- `~/.local/state/dev/projects/<name>.json`: repo, branch, Lima instance, Tailscale hostname, browser port, created/last-used times.

**Locking:** a file lock per project (`flock` on `~/.local/state/dev/locks/<name>.lock`), plus a global lock for self-update and SSH config writes. A held lock waits up to 30 s, then exits with code 3.

**Logging:** every invocation appends to `~/.local/state/dev/logs/<date>.log` (command, project, steps, external commands run, durations, errors). `--verbose` mirrors it to stderr.

**SSH config:** `dev` owns one `Include ~/.config/dev/ssh_config` line in `~/.ssh/config` and rewrites only its own file. Entries: `lima-<name>` (from Lima's generated config) and `<name>-dev` (`HostName 127.0.0.1`, port 2222, `ProxyJump lima-<name>`, `IdentityFile ~/.lima/_config/user`).

**`dev doctor` checks:** Lima installed and recent, Rosetta installed (warn only: it lets VMs run amd64-only images fast; without it they fail with `exec format error` or need slow QEMU emulation), Tailscale installed and logged in (CLI on PATH), Tailscale OAuth client ID and secret in Keychain (if missing: print the setup guide) and able to mint an API token, gh logged in, Node on the Mac, VS Code + Remote-SSH + Dev Containers extensions, Chrome 144+ (warn only), `~/.local/bin` on PATH, SSH `Include` present. `--fix` installs what Homebrew can install.

**Resources:** `dev ls` shows per-VM CPU, RAM and disk use. `dev up` warns when running VMs would exceed a configurable share of host RAM (default 75%). `dev prune` removes stopped VMs past a threshold and runs `docker system prune` inside running ones on request. Before removing a VM it starts it if needed and runs the same uncommitted/unpushed check as `rm`; VMs that fail the check are skipped and listed, unless `--force`.

**Upgrades of existing VMs:** provisioning steps have stable IDs and are append-only once released. A VM missing steps gets a one-line hint on `dev up`; `dev upgrade` applies them.

## Implementation

**Repo layout**

```text
cmd/dev/              main, cobra commands (one file per command)
internal/lima/        limactl wrapper, Lima YAML generation
internal/provision/   provisioning steps with stable IDs, run over limactl shell
internal/tailscale/   OAuth client, auth-key minting, node lookup and deletion, Keychain access
internal/gh/          host token read, VM token sync, container re-login
internal/devcontainer/ starter templates (embed), hash registry, CLI wrapper
internal/sshconfig/   Include management, entry generation
internal/browser/     MCP bridge, launchd job lifecycle
internal/skill/       embedded SKILL.md, harness table, install/notify
internal/update/      release check, download, verify, atomic swap
internal/state/       config.toml, project state, locks, logs
templates/devcontainer/  starter files (embedded with go:embed)
skill/dev/SKILL.md
install.sh
```

**External commands are behind interfaces** (`limactl`, `tailscale`, `gh`, `ssh`, `devcontainer`, `docker`, `security`, `launchctl`), so tests swap in fakes.

**Testing**

- Unit tests with fake runners that record commands and return scripted output: provisioning order, idempotence, naming and validation, starter detection, SSH config rewriting, update swap, launchd job generation.
- Golden-file tests for generated Lima YAML, SSH config, launchd plists and the starter files.
- GitHub Actions: lint, unit tests, GoReleaser snapshot build. GitHub's macOS runners can't run nested VMs, so no VM tests in CI.
- A `make e2e` target run on your own Mac, with `DEV_NO_UPDATE=1`, a dedicated machine prefix (`e2e`) so test VMs and tailnet nodes are easy to spot, and cleanup on failure (`dev rm --force` plus node deletion). Until the starter image is on ghcr, `DEV_STARTER_IMAGE` points at an image built inside the VM. Flow against a small **private** sample repo (proves token auth) with a `docker-compose.yml` and a `git@github.com:` submodule (proves `insteadOf`):
  1. `new` → `exec -- docker compose up -d` → service reachable on `localhost` from `exec`.
  2. `url` → `curl` the Tailscale URL from the Mac.
  3. `ssh <name>-dev true` works.
  4. Refresh the token, `up`, then `exec -- gh auth status` succeeds.
  5. `down` → `rm` (with an untouched starter, `rm` must not refuse).
  - A second sample repo with a custom `.devcontainer/` covers case 3.

**Milestones**

1. **Skeleton:** Go module, cobra, config/state, locking, logging, `doctor`, release pipeline, `install.sh`, self-update.
2. **VM lifecycle:** Lima YAML, provisioning steps 1–6 (including gh token and clone), `new` (without Tailscale), `up`, `down`, `rm`, `ls`, `exec --vm`.
3. **Tailscale:** setup guide and Keychain storage, key minting, step 7, `url`, `serve`, node lookup and deletion on `rm`.
4. **Devcontainer:** starter templates, prebuilt starter image published to ghcr, hash detection, container gh login, `up` and `rebuild` via the devcontainer CLI, `exec`, `open`.
5. **SSH and agents:** container sshd with persisted keys (step 8), `ssh-config`, auth volumes; verify the Claude and Codex desktop apps connect to `<name>-dev`.
6. **Skill:** embedded skill, harness table, install and update notices.
7. **Browser:** `browser grant`, `revoke`, `status` with launchd jobs.
8. **Operations:** `upgrade`, `prune`, resource warnings, `self-uninstall`.

## Before implementation (one-time setup)

- **Tailscale:** ✓ done (Nov 12, 2026). Tailnet `taildbf6fc.ts.net`; Mac machine name `black` (default `<machine>` prefix). Policy file saved with `tag:devvm` (tests pass); MagicDNS on; HTTPS certificates deliberately off; OAuth trust credential with `devices:core` + `auth_keys` (write, tag `tag:devvm`), stored as Keychain service `dev-tailscale`, accounts `client-id` and `client-secret`; verified it mints an API token.
- **GitHub:** repo `nicorobinreal/dev` with Actions read/write ✓; gh logged in with `repo` and `workflow` scopes (no packages scopes needed: the starter image is public, and CI pushes with `GITHUB_TOKEN`); make `dev-starter` public after its first push.
- **e2e fixtures:** ✓ created (private):
  - `nicorobinreal/dev-e2e-compose`: no `.devcontainer` (case 1); `docker-compose.yml` with `web` (nginx on 8080, bind-mounts `./site`, serves `dev-e2e-ok`) and `db` (Postgres on 5432, password `e2e`); submodule `vendor/custom` with a `git@github.com:` URL. Verified it clones recursively over HTTPS with only the `insteadOf` rule and the gh token.
  - `nicorobinreal/dev-e2e-custom`: custom `.devcontainer` (case 3), also the submodule above.
- **Mac:** `lima`, `go`, `goreleaser`, `golangci-lint`, `node` (Homebrew is just the easiest way; `dev doctor --fix` uses it); Rosetta; VS Code + Remote-SSH + Dev Containers; Chrome 144+ with remote debugging; about 50 GB free; back up `~/.ssh/config`.
- **Agent permissions:** allow `limactl`, `ssh`, `security`, `tailscale`, `gh`, `launchctl` so the implementing agent doesn't stall on approvals.
