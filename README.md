# dev

`dev` gives each project its own Linux VM on your Mac. Each VM has its own
Docker daemon, a devcontainer on host networking, and a Tailscale address.

- **No port clashes.** Every project's `docker compose` services sit on its own
  `localhost`. Agents and e2e tests in the container reach them directly, with
  no shared Docker networks.
- **Reachable from any of your devices** at `http://<machine>-<project>:<port>`
  over Tailscale, with no port forwarding on the Mac.
- **Agent-ready.** Claude Code, Codex and OpenCode come preinstalled in the
  container, run without approval prompts (the VM is the security boundary),
  and can drive your real Chrome when you allow it.

One command takes you from a GitHub repo to a running, VS Code-ready
environment:

```sh
dev new shop --repo owner/shop
```

## Requirements

- An Apple Silicon Mac
- [Lima](https://lima-vm.io), [Tailscale](https://tailscale.com) (logged in),
  [gh](https://cli.github.com) (logged in) and Node
- VS Code with the Remote-SSH and Dev Containers extensions, or Cursor or Zed
- Optional: Chrome 144+ for browser access, and Rosetta to run amd64-only
  images fast

`dev doctor` checks all of these, and `dev doctor --fix` installs what
Homebrew can.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/nicorobinreal/dev/main/install.sh | sh
```

This puts `dev` in `~/.local/bin`, then runs `dev init`, which:
- asks for your machine prefix, which agents get the `dev` skill, and the
  default VM size
- installs the skill
- runs `dev doctor`

`dev init` only asks questions in a terminal. Agents and scripts get no
prompts: they pass settings as flags, and anything not given keeps its current
or default value.

```sh
curl -fsSL https://raw.githubusercontent.com/nicorobinreal/dev/main/install.sh | DEV_INIT_ARGS="--json" sh
dev init --json --machine mbp --harnesses claude,codex --cpus 6
```

`dev` updates itself in the background, checking at most once an hour.

### One-time Tailscale setup

`dev` mints a single-use auth key for each VM through a Tailscale OAuth
client. `dev doctor` walks you through creating one:
- a policy that lets your devices reach `tag:devvm` (VMs can't reach your
  devices)
- MagicDNS on
- an OAuth client with **Auth Keys** (write) and **Devices Core** (read and
  write) access

The client ID and secret are stored in the macOS Keychain and never leave the
Mac.

## Quick start

```sh
dev new shop --repo owner/shop          # create the VM, clone, start the devcontainer, open VS Code
dev exec shop -- docker compose up -d   # run anything in the devcontainer
dev url shop 8080                       # http://mbp-shop:8080, works from your phone too
dev down shop                           # stop the VM
dev up shop                             # start it again
dev rm shop                             # delete it (refuses if you have unpushed work)
```

If the repo has no `.devcontainer/`, `dev new` writes a starter one for you to
commit. A repo with its own devcontainer is used as-is, but it may lack things
`dev` relies on (see [The starter devcontainer](#the-starter-devcontainer)).

## Commands

| Command | What it does |
| --- | --- |
| `dev new <name> --repo <repo>` | Create a VM, clone the repo, join the tailnet, start the devcontainer, open the editor. Flags: `--branch`, `--cpus`, `--memory`, `--disk`, `--editor code\|cursor\|zed`, `--no-open` |
| `dev up <name>` | Start the VM, refresh the GitHub token, start the devcontainer. Flag: `--no-open` |
| `dev down <name>` | Revoke browser access and stop the VM |
| `dev rebuild <name>` | Rebuild the devcontainer after changing it. Flag: `--no-cache` |
| `dev rm <name>` | Delete the VM, its tailnet node and SSH entries. Refuses if there is uncommitted or unpushed work unless `--force` |
| `dev ls` | List projects with state, branch, Tailscale name, CPU/RAM/disk and browser access |
| `dev open <name>` | Open the editor on the project. Flag: `--editor` |
| `dev exec <name> -- <cmd>` | Run a command in the devcontainer. Flag: `--vm` (run on the VM instead) |
| `dev url <name> [port]` | Print the project's Tailscale URL |
| `dev serve <name> <port>` | Serve a port over HTTPS with `tailscale serve`, for apps that need a secure context. Flag: `--off` |
| `dev browser grant\|revoke\|status <name>` | Let the project's agents drive your Chrome |
| `dev ssh-config` | Regenerate the SSH host entries |
| `dev upgrade <name>` | Apply provisioning steps added since the VM was created. Flag: `--all` |
| `dev prune` | Remove long-unused stopped VMs and orphaned tailnet nodes. Flags: `--older-than 14d`, `--docker`, `--force` |
| `dev doctor` | Check prerequisites and guide the Tailscale setup. Flag: `--fix` |
| `dev init` | Re-run first-time setup |
| `dev self-update` | Update now |
| `dev self-uninstall` | Remove `dev`, the skill and the SSH config; keeps VMs. Flag: `--purge` (also deletes VMs and their tailnet nodes) |

Global flags: `--json` (machine-readable output), `--dry-run`, `--verbose`.

Commands never prompt, so agents can call them. `dev init` and the Tailscale
setup in `dev doctor` are the exceptions. Exit codes:

| Code | Meaning |
| --- | --- |
| 0 | Success |
| 1 | Error |
| 2 | Refused for safety (e.g. `rm` with unpushed work) |
| 3 | Another `dev` command holds the project's lock |

## How it works

| Where | What runs there |
| --- | --- |
| **Mac** | Only credentials and user interfaces: your editor, agent apps, Chrome |
| **Project VM** | A Lima VM (Ubuntu LTS) with Docker, the repo clone and Tailscale |
| **Devcontainer** | Runs on the VM's Docker with host networking, so it shares `localhost` with the compose services |

**Editors and SSH.** `dev` writes two SSH hosts per project:
- `lima-<name>` is the VM. `dev open` connects VS Code or Cursor there, and
  Dev Containers reopens the folder in the container.
- `<name>-dev` is the container's own sshd, used by Zed and by the Claude and
  Codex desktop apps.

**GitHub access.** Your `gh` token is copied into the VM and mounted read-only
into the container, and refreshed on every `dev up`. Git uses HTTPS only;
`git@github.com:` URLs, including submodules, are rewritten to HTTPS.

**Agent logins.** Log in to Claude, Codex or OpenCode once inside the
container. Logins are kept in per-project Docker volumes, so they survive
rebuilds.

## Browser access

```sh
dev browser grant shop
```

Agents in the container can then drive your real Chrome, with your logins,
while you watch. Before the first use, turn on remote debugging at
`chrome://inspect/#remote-debugging`. Chrome asks you to approve each new
debugging session.

`grant` sets up three things:
- a local bridge to
  [Chrome DevTools MCP](https://github.com/ChromeDevTools/chrome-devtools-mcp)
- a reverse SSH tunnel to the VM
- the MCP server registered with Claude Code, Codex and OpenCode in the
  container

`dev down` and `dev rm` revoke access automatically.

The browser runs on the Mac, so `localhost` in the browser is the Mac, not the
container. To open a project's service in the browser, use its Tailscale URL
(`http://mbp-shop:8080`). The bridge passes this to agents through its MCP
server instructions.

## The starter devcontainer

The starter `.devcontainer/` builds on the prebuilt image
`ghcr.io/nicorobinreal/dev-starter`. That image holds Debian, Node, Go, gh,
Claude Code, Codex and OpenCode. Add project-specific lines below the `FROM`
in its `Dockerfile`.

A custom devcontainer needs the following for `dev` to work fully:
- host networking
- the workspace mounted at the same path as on the VM
- the gh token mount
- sshd on port 2222 with `dev`'s persisted host keys
- the agent login volumes

`dev new` warns when a repo's devcontainer isn't a starter.

## Files

| Path | Contents |
| --- | --- |
| `~/.config/dev/config.toml` | Machine prefix, harnesses, default VM size, editor |
| `~/.config/dev/ssh_config` | Generated SSH hosts, included from `~/.ssh/config` |
| `~/.local/state/dev/projects/` | Per-project state |
| `~/.local/state/dev/logs/` | One log per day of every command and what it ran |

## Development

```sh
make build     # ./bin/dev
make test      # unit tests
make lint      # golangci-lint
make install   # copy the build to ~/.local/bin/dev
make e2e       # real VMs against the private fixture repos (slow, local only)
```

Local builds report version `dev` and never auto-update.

| Environment variable | Effect |
| --- | --- |
| `DEV_NO_UPDATE=1` | Turn off update checks |
| `DEV_HOME` | Use a different home directory for all of dev's files |
| `DEV_MACHINE` | Override the machine prefix (e2e uses `e2e`) |
| `DEV_STARTER_IMAGE` | Build the starter from another image |

### Releasing

Push a tag:

```sh
git tag v0.2.0 && git push origin v0.2.0
```

The release workflow:
1. Builds the starter image if its tag isn't on ghcr yet, or if
   `starter-image/` changed since the previous release.
2. Checks that the image exists.
3. Publishes the binaries with GoReleaser.

Installed copies update within an hour. The version comes from the tag;
nothing in the code needs bumping.

Some changes need more than a tag:

- **What new VMs get installed:** add a new provisioning step in
  `internal/provision` instead of editing a released one. VMs only run steps
  they haven't recorded, and existing VMs pick up new steps with
  `dev upgrade`.
- **The container image (`starter-image/`):** non-breaking changes ship under
  the same tag (`v1`). A breaking change becomes a new starter version. Add
  `templates/devcontainer/vN/`, register its hash in
  `internal/devcontainer/starter.go`, and bump the `StarterImage` constant and
  `starter-image/VERSION`. Tests catch it if these disagree.
