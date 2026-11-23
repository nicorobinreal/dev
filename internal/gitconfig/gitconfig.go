// Package gitconfig forwards a fixed set of the Mac's global git settings
// (commit identity and default branch) into a project VM and, through a
// read-only bind mount, into its devcontainer.
//
// Only the keys in Keys are copied. A Mac's ~/.gitconfig often points at
// things that don't exist on Linux (osxkeychain, gpg.program paths,
// core.excludesfile, include.path), so copying it whole would break git in
// the VM.
//
// In the VM the settings live in ~/.config/dev/git/config, which the VM's
// ~/.gitconfig includes. The starter mounts that folder read-only at
// /run/dev/git and its postCreateCommand adds the same include to the
// container's ~/.gitconfig. Not git's XDG path (~/.config/git/config): when
// ~/.gitconfig doesn't exist yet, `git config --global` writes to the XDG
// file, which would be the read-only mount.
package gitconfig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/shellquote"
)

// Keys are the settings forwarded from the Mac, in file order.
var Keys = []string{"user.name", "user.email", "init.defaultBranch"}

// Paths inside the VM (relative to $HOME) and the container.
const (
	VMDir        = ".config/dev/git"        // under $HOME, mode 755
	VMFile       = ".config/dev/git/config" // under $HOME, mode 644
	ContainerDir = "/run/dev/git"           // read-only bind mount of VMDir
)

// Entry is one forwarded setting.
type Entry struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Shell runs a bash script inside a VM, with optional stdin. lima.VM
// satisfies it.
type Shell interface {
	Shell(ctx context.Context, script string, stdin io.Reader) (run.Result, error)
}

// Host reads Keys from the Mac's global git config. Unset keys are skipped.
// A missing git or an unreadable config is an error; callers treat it as a
// warning and forward nothing.
func Host(ctx context.Context, r run.Runner) ([]Entry, error) {
	var out []Entry
	for _, k := range Keys {
		res, err := r.Run(ctx, run.Cmd{Name: "git", Args: []string{"config", "--global", "--get", k}})
		if err != nil {
			// git config --get exits 1 when the key is unset.
			var ee *run.ExitError
			if errors.As(err, &ee) && ee.Result.ExitCode == 1 {
				continue
			}
			return nil, fmt.Errorf("read git config %s: %w", k, err)
		}
		v := strings.TrimRight(res.Stdout, "\r\n")
		if v == "" {
			continue
		}
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, fmt.Errorf("git config %s has a multi-line value; not forwarding it", k)
		}
		out = append(out, Entry{Key: k, Value: v})
	}
	return out, nil
}

// SyncScript returns a bash script that rewrites ~/.config/dev/git/config in
// the VM with exactly entries (an empty file when there are none, so the
// container's bind mount always has a source) and makes the VM's own
// ~/.gitconfig include it. The file is written to a temp file in the same
// folder and renamed into place; the container mounts the folder, so it sees
// the new file without a restart. Idempotent; used by the "git-config"
// provisioning step and by every `dev up`.
func SyncScript(entries []Entry) (string, error) {
	var b strings.Builder
	b.WriteString(`set -euo pipefail
d="$HOME/` + VMDir + `"
mkdir -p "$d"
chmod 755 "$d"
tmp="$(mktemp "$d/.config.XXXXXX")"
trap 'rm -f "$tmp"' EXIT
`)
	for _, e := range entries {
		if !allowed(e.Key) {
			return "", fmt.Errorf("git config key %q is not forwarded", e.Key)
		}
		if strings.ContainsAny(e.Value, "\n\r\x00") {
			return "", fmt.Errorf("git config %s has a multi-line value", e.Key)
		}
		b.WriteString(`git config --file "$tmp" ` + shellquote.Quote(e.Key, e.Value) + "\n")
	}
	b.WriteString(`chmod 644 "$tmp"
mv -f "$tmp" "$d/config"
trap - EXIT
inc="~/` + VMFile + `"
git config --global --get-all include.path | grep -qxF "$inc" ||
  git config --global --add include.path "$inc"
`)
	return b.String(), nil
}

// SyncVM writes entries into the VM (see SyncScript).
func SyncVM(ctx context.Context, sh Shell, entries []Entry) error {
	script, err := SyncScript(entries)
	if err != nil {
		return err
	}
	if _, err := sh.Shell(ctx, script, nil); err != nil {
		return fmt.Errorf("sync git config into VM: %w", err)
	}
	return nil
}

func allowed(k string) bool {
	for _, a := range Keys {
		if a == k {
			return true
		}
	}
	return false
}
