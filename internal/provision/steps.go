package provision

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nicorobinreal/dev/internal/gh"
)

// Stable step IDs. Append-only once released.
const (
	StepBasePackages = "base-packages"
	StepDocker       = "docker"
	StepGitGH        = "git-gh"
	StepNode         = "node-devcontainer-cli"
	StepGHToken      = "gh-token"
	StepClone        = "clone"
	StepTailscale    = "tailscale"
	StepContainerSSH = "container-ssh"
)

// Image bumps: lima pins the Ubuntu cloud image for new VMs. When it is bumped
// on purpose, add a step here (e.g. "ubuntu-26.04-YYYYMMDD": apt full-upgrade,
// or do-release-upgrade for a new LTS) so `dev upgrade` brings existing VMs in
// line. Fresh VMs run it too, harmlessly, since it is idempotent.

// prelude is prepended to every step script.
const prelude = `set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
# First boot: let cloud-init (and its apt runs) finish before touching apt.
command -v cloud-init >/dev/null 2>&1 && sudo cloud-init status --wait >/dev/null 2>&1 || true
apt_get() { sudo -E apt-get -o DPkg::Lock::Timeout=600 -y -q "$@"; }
apt_install() { apt_get install --no-install-recommends "$@"; }
`

// Steps returns every known step in execution order.
func Steps() []Step {
	return []Step{
		{
			ID:          StepBasePackages,
			Description: "base packages, time sync, unattended security upgrades",
			Script:      static(basePackages),
		},
		{
			ID:          StepDocker,
			Description: "Docker Engine (rootful) and Compose plugin; user in docker group",
			Script:      static(docker),
		},
		{
			ID:          StepGitGH,
			Description: "git and gh; rewrite git@github.com: to HTTPS",
			Script:      static(gitGH),
		},
		{
			ID:          StepNode,
			Description: "Node LTS and @devcontainers/cli",
			Script:      static(node),
		},
		{
			ID:          StepGHToken,
			Description: "gh token in ~/.config/dev/gh/token; gh login on the VM",
			Script:      func(Params) (string, error) { return gh.SyncScript, nil },
			Stdin: func(_ context.Context, p Params) (io.Reader, error) {
				tok := strings.TrimSpace(p.GHToken)
				if tok == "" {
					return nil, errors.New("no gh token")
				}
				return strings.NewReader(tok + "\n"), nil
			},
		},
		{
			ID:          StepClone,
			Description: "clone the repo to $HOME/<repo>",
			Script:      cloneScript,
		},
		{
			ID:          StepTailscale,
			Description: "Tailscale, joined as <machine>-<name>",
			Script:      tailscaleScript,
			Stdin: func(ctx context.Context, p Params) (io.Reader, error) {
				if p.TailscaleAuthKey == nil {
					return nil, errors.New("no Tailscale auth key source")
				}
				key, err := p.TailscaleAuthKey(ctx)
				if err != nil {
					return nil, fmt.Errorf("mint Tailscale auth key: %w", err)
				}
				if strings.TrimSpace(key) == "" {
					return nil, errors.New("empty Tailscale auth key")
				}
				return strings.NewReader(strings.TrimSpace(key) + "\n"), nil
			},
			// Already joined (e.g. the step ran but recording failed): skip
			// without minting a key.
			Done: func(Params) string {
				return "command -v tailscale >/dev/null && tailscale status >/dev/null 2>&1"
			},
		},
		{
			ID:          StepContainerSSH,
			Description: "persistent sshd host keys and authorized_keys for the container",
			Script:      containerSSHScript,
		},
	}
}

func static(body string) func(Params) (string, error) {
	return func(Params) (string, error) { return prelude + body, nil }
}

const basePackages = `
apt_get update
apt_install ca-certificates curl gnupg git jq unzip acl unattended-upgrades
# Time sync: Ubuntu ships chrony (25.10+) or systemd-timesyncd; install chrony
# only if neither is present.
if ! dpkg -s chrony >/dev/null 2>&1 && ! dpkg -s systemd-timesyncd >/dev/null 2>&1; then
  apt_install chrony
fi
sudo timedatectl set-ntp true || true
# Unattended upgrades: the default 50unattended-upgrades only allows the
# security pocket.
sudo tee /etc/apt/apt.conf.d/20auto-upgrades >/dev/null <<'EOF'
APT::Periodic::Update-Package-Lists "1";
APT::Periodic::Unattended-Upgrade "1";
EOF
sudo systemctl enable --now unattended-upgrades.service >/dev/null 2>&1 || true
`

const docker = `
if ! { command -v docker >/dev/null && docker compose version >/dev/null 2>&1; } &&
   ! { dpkg -s docker-ce >/dev/null 2>&1 && dpkg -s docker-compose-plugin >/dev/null 2>&1; }; then
  . /etc/os-release
  sudo install -m 0755 -d /etc/apt/keyrings
  sudo curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc
  sudo chmod a+r /etc/apt/keyrings/docker.asc
  sudo tee /etc/apt/sources.list.d/docker.sources >/dev/null <<EOF
Types: deb
URIs: https://download.docker.com/linux/ubuntu
Suites: ${UBUNTU_CODENAME:-$VERSION_CODENAME}
Components: stable
Architectures: $(dpkg --print-architecture)
Signed-By: /etc/apt/keyrings/docker.asc
EOF
  apt_get update
  apt_install docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi
sudo systemctl enable --now docker.service containerd.service
# Rootful daemon; the Lima user reaches /var/run/docker.sock via the group.
# NOTE: takes effect on the next SSH login only (see NeedsSessionReset).
sudo usermod -aG docker "$(id -un)"
`

const gitGH = `
if ! command -v gh >/dev/null; then
  sudo install -m 0755 -d /etc/apt/keyrings
  curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg |
    sudo tee /etc/apt/keyrings/githubcli-archive-keyring.gpg >/dev/null
  sudo chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" |
    sudo tee /etc/apt/sources.list.d/github-cli.list >/dev/null
  apt_get update
  apt_install gh
fi
command -v git >/dev/null || apt_install git
# Git rides on the gh token over HTTPS: rewrite SSH-style GitHub URLs,
# including in submodules.
add_insteadof() {
  git config --global --get-all url.https://github.com/.insteadOf | grep -qxF "$1" ||
    git config --global --add url.https://github.com/.insteadOf "$1"
}
add_insteadof git@github.com:
add_insteadof ssh://git@github.com/
`

const node = `
if ! command -v node >/dev/null; then
  curl -fsSL https://deb.nodesource.com/setup_lts.x -o /tmp/nodesource_setup.sh
  sudo -E bash /tmp/nodesource_setup.sh
  rm -f /tmp/nodesource_setup.sh
  apt_install nodejs
fi
command -v devcontainer >/dev/null || sudo npm install -g --no-fund --no-audit @devcontainers/cli
devcontainer --version >/dev/null
`

func cloneScript(p Params) (string, error) {
	if p.RepoURL == "" || p.RepoDir == "" {
		return "", errors.New("repo URL and dir are required")
	}
	if strings.ContainsAny(p.RepoDir, "/\x00") || p.RepoDir == "." || p.RepoDir == ".." {
		return "", fmt.Errorf("invalid repo dir %q", p.RepoDir)
	}
	if !strings.HasPrefix(p.RepoURL, "https://") {
		return "", fmt.Errorf("repo URL must be https: %q", p.RepoURL)
	}
	branch := ""
	if p.Branch != "" {
		branch = "--branch " + shq(p.Branch) + " "
	}
	return prelude + fmt.Sprintf(`
dest="$HOME/"%s
if [ -d "$dest/.git" ]; then
  echo "already cloned: $dest"
  exit 0
fi
if [ -e "$dest" ] && [ -n "$(ls -A "$dest" 2>/dev/null)" ]; then
  echo "dev: $dest exists and is not a git clone" >&2
  exit 1
fi
git clone --recurse-submodules %s-- %s "$dest"
`, shq(p.RepoDir), branch, shq(p.RepoURL)), nil
}

func tailscaleScript(p Params) (string, error) {
	if p.TailscaleHostname == "" || p.TailscaleTag == "" {
		return "", errors.New("tailscale hostname and tag are required")
	}
	return prelude + fmt.Sprintf(`
if ! command -v tailscale >/dev/null; then
  curl -fsSL https://tailscale.com/install.sh | sh
fi
sudo systemctl enable --now tailscaled.service
if tailscale status >/dev/null 2>&1; then
  echo "tailscale already up"
  cat >/dev/null
  exit 0
fi
# The auth key arrives on stdin and goes to a private file; tailscale reads
# it via file:, so it never appears in argv or ps.
umask 077
kf="$(mktemp)"
trap 'rm -f "$kf"' EXIT
IFS= read -r key || true
[ -n "$key" ] || { echo "dev: empty Tailscale auth key" >&2; exit 1; }
printf '%%s\n' "$key" > "$kf"
unset key
sudo tailscale up --auth-key="file:$kf" --hostname=%s --advertise-tags=%s
`, shq(p.TailscaleHostname), shq(p.TailscaleTag)), nil
}

func containerSSHScript(p Params) (string, error) {
	key := strings.TrimSpace(p.LimaPubKey)
	if key == "" || strings.ContainsAny(key, "\n\r") {
		return "", errors.New("a single-line Lima public key (~/.lima/_config/user.pub) is required")
	}
	return prelude + fmt.Sprintf(`
d="$HOME/.config/dev/container-ssh"
mkdir -p "$d"
chmod 700 "$d"
# Persistent host keys: rebuilding the container keeps its SSH identity.
[ -s "$d/ssh_host_ed25519_key" ] || ssh-keygen -q -t ed25519 -N '' -C dev-container -f "$d/ssh_host_ed25519_key"
[ -s "$d/ssh_host_rsa_key" ] || ssh-keygen -q -t rsa -b 4096 -N '' -C dev-container -f "$d/ssh_host_rsa_key"
umask 077
tmp="$(mktemp "$d/.authorized_keys.XXXXXX")"
printf '%%s\n' %s > "$tmp"
chmod 600 "$tmp"
mv -f "$tmp" "$d/authorized_keys"
`, shq(key)), nil
}
