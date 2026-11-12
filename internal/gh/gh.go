// Package gh handles the GitHub credential flow: reading the host's gh token,
// syncing it into a VM, the container re-login command, repo argument
// normalisation, and the uncommitted/unpushed work check used by rm/prune.
package gh

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nicorobinreal/dev/internal/run"
)

// Shell runs a bash script inside a VM, with optional stdin. lima.VM
// satisfies it; so does any fake in tests.
type Shell interface {
	Shell(ctx context.Context, script string, stdin io.Reader) (run.Result, error)
}

// Paths inside the VM (relative to $HOME) and the container.
const (
	VMTokenDir        = ".config/dev/gh"        // under $HOME, mode 700
	VMTokenFile       = ".config/dev/gh/token"  // under $HOME, mode 600
	ContainerTokenDir = "/run/secrets/gh"       // read-only bind mount of VMTokenDir
	ContainerToken    = "/run/secrets/gh/token" // what the container logs in from
)

// HostToken returns the token of the Mac's gh login (`gh auth token`).
func HostToken(ctx context.Context, r run.Runner) (string, error) {
	res, err := r.Run(ctx, run.Cmd{Name: "gh", Args: []string{"auth", "token", "--hostname", "github.com"}})
	if err != nil {
		return "", fmt.Errorf("read host gh token (is gh logged in? run `gh auth login`): %w", err)
	}
	tok := strings.TrimSpace(res.Stdout)
	if tok == "" {
		return "", errors.New("gh auth token printed nothing; run `gh auth login`")
	}
	if strings.ContainsAny(tok, " \t\r\n") {
		return "", errors.New("gh auth token returned malformed output")
	}
	return tok, nil
}

// SyncScript writes the token read from stdin to ~/.config/dev/gh/token
// atomically (temp file in the same dir + rename, so the container's
// directory bind mount sees the new file) and refreshes the VM's gh login.
// The token never appears in the script or argv. It is idempotent and is used
// both by the "gh-token" provisioning step and by every `dev up`.
const SyncScript = `set -euo pipefail
umask 077
d="$HOME/` + VMTokenDir + `"
mkdir -p "$d"
chmod 700 "$d"
tmp="$(mktemp "$d/.token.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
# First line of stdin is the token.
IFS= read -r tok || true
if [ -z "$tok" ]; then echo "dev: empty gh token on stdin" >&2; exit 1; fi
printf '%s\n' "$tok" > "$tmp"
unset tok
chmod 600 "$tmp"
mv -f "$tmp" "$d/token"
trap - EXIT
if command -v gh >/dev/null 2>&1; then
  gh auth login --hostname github.com --git-protocol https --with-token < "$d/token"
  gh auth setup-git --hostname github.com
fi
`

// SyncVM writes token into the VM and refreshes the VM's gh login. If gh is
// not installed yet (before the git-gh step) only the file is written.
func SyncVM(ctx context.Context, sh Shell, token string) error {
	if strings.TrimSpace(token) == "" {
		return errors.New("gh: empty token")
	}
	if _, err := sh.Shell(ctx, SyncScript, strings.NewReader(strings.TrimSpace(token)+"\n")); err != nil {
		return fmt.Errorf("sync gh token into VM: %w", err)
	}
	return nil
}

// ContainerLoginArgs is the command `dev up` runs in the devcontainer
// (`devcontainer exec ... <args>`) to log gh in from the mounted token. It is
// the same as the starter's postStartCommand, which doesn't fire when the
// container is already running.
func ContainerLoginArgs() []string {
	return []string{"sh", "-c", "gh auth login --with-token < " + ContainerToken + " && gh auth setup-git"}
}
