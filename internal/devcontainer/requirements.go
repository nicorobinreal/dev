package devcontainer

// Requirement is one thing dev relies on from a project's devcontainer. The
// starter satisfies all of them; a custom devcontainer may not. The list is
// reported in `dev new --json` for custom devcontainers and mirrored in the
// skill, so a harness can suggest a merge.
type Requirement struct {
	ID string `json:"id"`
	// What the devcontainer must do.
	Title string `json:"title"`
	// Why dev needs it / what breaks without it.
	Why string `json:"why"`
	// Starter is the starter's devcontainer.json setting(s) that provide it.
	Starter []string `json:"starter"`
}

// Requirements is what a devcontainer needs for every dev feature to work.
var Requirements = []Requirement{
	{
		ID:      "host-network",
		Title:   "Run on the VM's host network",
		Why:     "compose services' published ports are reachable on localhost from the container, the container sshd listens on VM port 2222, and the browser bridge tunnel lands on localhost",
		Starter: []string{`"runArgs": ["--network=host"]`},
	},
	{
		ID:    "workspace-path",
		Title: "Mount the repo at the same path as on the VM",
		Why:   "compose bind mounts (./src:/app) are resolved by the VM's Docker daemon, so the container must see the repo at the identical path",
		Starter: []string{
			`"workspaceMount": "source=${localWorkspaceFolder},target=${localWorkspaceFolder},type=bind"`,
			`"workspaceFolder": "${localWorkspaceFolder}"`,
		},
	},
	{
		ID:    "gh-token",
		Title: "Mount the gh token folder and log gh in on every start",
		Why:   "git and gh in the container authenticate with the host's gh token; dev up refreshes it and re-runs the login",
		Starter: []string{
			`"mounts": ["source=${localEnv:HOME}/.config/dev/gh,target=/run/secrets/gh,type=bind,readonly"]`,
			`"initializeCommand": "test -s $HOME/.config/dev/gh/token || (echo 'run dev up' && exit 1)"`,
			`"postCreateCommand": "git config --global url.https://github.com/.insteadOf git@github.com:"`,
			`"postStartCommand": "gh auth login --with-token < /run/secrets/gh/token && gh auth setup-git"`,
		},
	},
	{
		ID:    "sshd",
		Title: "Run sshd on port 2222 with the persisted host keys and authorized_keys",
		Why:   "the <name>-dev SSH host (Zed, the Claude and Codex desktop apps) connects straight into the container",
		Starter: []string{
			`base image ghcr.io/nicorobinreal/dev-starter (sshd feature, port 2222, /usr/local/bin/dev-sshd-keys)`,
			`"mounts": ["source=${localEnv:HOME}/.config/dev/container-ssh,target=/run/secrets/ssh,type=bind,readonly"]`,
			`"postStartCommand": "... && /usr/local/bin/dev-sshd-keys"`,
		},
	},
	{
		ID:    "auth-volumes",
		Title: "Keep agent logins in per-project named volumes",
		Why:   "Claude Code, Codex and OpenCode logins survive rebuilds",
		Starter: []string{
			`"mounts": ["source=claude-${devcontainerId},target=/root/.claude-auth,type=volume", "source=codex-${devcontainerId},target=/root/.codex,type=volume", "source=opencode-${devcontainerId},target=/root/.local/share/opencode,type=volume"]`,
			`"containerEnv": {"CLAUDE_CONFIG_DIR": "/root/.claude-auth"}`,
		},
	},
}
