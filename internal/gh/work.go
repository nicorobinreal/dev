package gh

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// WorkStatus is what `dev rm` and `dev prune` need to decide whether a VM
// holds work that would be lost.
type WorkStatus struct {
	// RepoExists is false when $HOME/<repodir> is missing or not a git repo;
	// the other fields are then empty.
	RepoExists bool `json:"repo_exists"`
	// Untracked lists untracked paths relative to the repo root, in git's
	// default (non -uall) form: an untracked directory appears once with a
	// trailing slash, e.g. ".devcontainer/". The caller can exclude an
	// untracked ".devcontainer/" that is byte-identical to a known starter.
	Untracked []string `json:"untracked,omitempty"`
	// Modified lists tracked paths with staged or unstaged changes
	// (including deletions, renames' new paths, conflicts, dirty submodules).
	Modified []string `json:"modified,omitempty"`
	// Unpushed lists commits reachable from a local branch or HEAD that are on
	// no remote-tracking branch, as "<short hash> <subject>".
	Unpushed []string `json:"unpushed,omitempty"`
	// Stashes is the number of stash entries.
	Stashes int `json:"stashes"`
}

// Clean reports whether nothing would be lost, given an optional filter for
// untracked paths the caller considers harmless (e.g. a pristine starter
// ".devcontainer/").
func (w WorkStatus) Clean(ignoreUntracked func(path string) bool) bool {
	for _, p := range w.Untracked {
		if ignoreUntracked == nil || !ignoreUntracked(p) {
			return false
		}
	}
	return len(w.Modified) == 0 && len(w.Unpushed) == 0 && w.Stashes == 0
}

// WorkCheckScript prints the repo's state in a line-oriented format with the
// NUL-separated git output base64-encoded, so odd file names survive.
// $1 is the repo directory relative to $HOME.
const workCheckScript = `set -uo pipefail
cd "$HOME/$1" 2>/dev/null && git rev-parse --git-dir >/dev/null 2>&1 || { echo "repo missing"; exit 0; }
echo "repo ok"
st="$(git status --porcelain=v1 -z --untracked-files=normal --ignore-submodules=none | base64 -w0)" || exit 1
echo "status $st"
# Commits on local branches or HEAD (detached) that no remote-tracking branch has.
up="$(git log --branches HEAD --not --remotes --format='%h %s' -z 2>/dev/null | base64 -w0)"
echo "unpushed $up"
echo "stashes $(git stash list 2>/dev/null | wc -l)"
`

// WorkCheck inspects $HOME/<repoDir> in the VM. The VM must be running.
func WorkCheck(ctx context.Context, sh Shell, repoDir string) (WorkStatus, error) {
	if repoDir == "" || strings.Contains(repoDir, "/") || repoDir == "." || repoDir == ".." {
		return WorkStatus{}, fmt.Errorf("invalid repo dir %q", repoDir)
	}
	script := "set -- " + shellQuote(repoDir) + "\n" + workCheckScript
	res, err := sh.Shell(ctx, script, nil)
	if err != nil {
		return WorkStatus{}, fmt.Errorf("check for uncommitted work: %w", err)
	}
	return parseWorkCheck(res.Stdout)
}

func parseWorkCheck(out string) (WorkStatus, error) {
	var w WorkStatus
	sawRepo := false
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	for sc.Scan() {
		key, val, _ := strings.Cut(sc.Text(), " ")
		switch key {
		case "repo":
			sawRepo = true
			w.RepoExists = val == "ok"
		case "status":
			b, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				return w, fmt.Errorf("work check: decode status: %w", err)
			}
			w.Untracked, w.Modified = parsePorcelainZ(string(b))
		case "unpushed":
			b, err := base64.StdEncoding.DecodeString(val)
			if err != nil {
				return w, fmt.Errorf("work check: decode log: %w", err)
			}
			for _, c := range strings.Split(string(b), "\x00") {
				if c = strings.TrimSpace(c); c != "" {
					w.Unpushed = append(w.Unpushed, c)
				}
			}
		case "stashes":
			n, err := strconv.Atoi(strings.TrimSpace(val))
			if err != nil {
				return w, fmt.Errorf("work check: bad stash count %q", val)
			}
			w.Stashes = n
		}
	}
	if !sawRepo {
		return w, fmt.Errorf("work check: unexpected output %q", out)
	}
	return w, sc.Err()
}

// parsePorcelainZ splits `git status --porcelain=v1 -z` output. Entries are
// "XY <path>\0"; renames and copies (X or Y is R/C) are followed by an extra
// "<orig path>\0" that is skipped.
func parsePorcelainZ(s string) (untracked, modified []string) {
	fields := strings.Split(s, "\x00")
	for i := 0; i < len(fields); i++ {
		e := fields[i]
		if len(e) < 4 {
			continue
		}
		xy, path := e[:2], e[3:]
		switch xy {
		case "??":
			untracked = append(untracked, path)
		case "!!":
			// ignored files are not work
		default:
			modified = append(modified, path)
			if strings.ContainsAny(xy, "RC") {
				i++ // skip the original path
			}
		}
	}
	return untracked, modified
}

// shellQuote single-quotes s for bash.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
