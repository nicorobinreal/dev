// Package provision installs everything a project VM needs, as an ordered list
// of idempotent steps with stable IDs, run over `limactl shell`.
//
// The VM records the ID of every step it has completed in
// /etc/dev/provision-steps (one per line). Run executes, in order, every known
// step not in that file and appends its ID after it succeeds; `dev upgrade`
// uses the same mechanism to bring old VMs forward. Released step IDs are
// append-only: never rename or remove one, add a new step instead.
package provision

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/nicorobinreal/dev/internal/run"
)

// StateFile is where the VM records completed step IDs.
const StateFile = "/etc/dev/provision-steps"

// Shell runs a bash script inside the VM with optional stdin; lima.VM
// satisfies it.
type Shell interface {
	Shell(ctx context.Context, script string, stdin io.Reader) (run.Result, error)
}

// Params are the inputs the step scripts need. Secrets (GHToken, the
// Tailscale auth key) reach the VM only on stdin, never in a script.
type Params struct {
	RepoURL string // https://github.com/owner/repo
	Branch  string // optional
	RepoDir string // clone dir under $HOME, e.g. "repo"

	GHToken string

	// TailscaleAuthKey mints a single-use auth key. It is called only when the
	// tailscale step actually has to join the tailnet, so no key is wasted.
	TailscaleAuthKey  func(ctx context.Context) (string, error)
	TailscaleHostname string // <machine>-<name>
	TailscaleTag      string // e.g. tag:devvm

	LimaPubKey string // contents of ~/.lima/_config/user.pub

	// Progress, if set, is called before each step runs.
	Progress func(Step)
}

// Step is one provisioning step.
type Step struct {
	ID          string
	Description string
	// Script returns the idempotent bash script for the step. It must not
	// contain secrets.
	Script func(p Params) (string, error)
	// Stdin, if set, returns the secret input for the script. It is called
	// only when the step runs.
	Stdin func(ctx context.Context, p Params) (io.Reader, error)
	// Done, if set, is a cheap check script; exit 0 means the work is already
	// in place, so the step is recorded without running Script (and without
	// calling Stdin). Used to avoid minting a Tailscale key needlessly.
	Done func(p Params) string
}

// Run executes, in order, every step not yet recorded in StateFile and
// records each one after it succeeds. If only is non-empty, it restricts the
// run to those IDs (still skipping recorded ones, still in canonical order);
// unknown IDs are an error. It returns the IDs that were executed or newly
// recorded. On error, earlier steps stay recorded, so a re-run resumes.
func Run(ctx context.Context, sh Shell, p Params, only []string) ([]string, error) {
	steps, err := selectSteps(only)
	if err != nil {
		return nil, err
	}
	done, err := Recorded(ctx, sh)
	if err != nil {
		return nil, err
	}
	var ran []string
	for _, s := range steps {
		if done[s.ID] {
			continue
		}
		if err := runStep(ctx, sh, p, s); err != nil {
			return ran, err
		}
		if _, err := sh.Shell(ctx, recordScript(s.ID), nil); err != nil {
			return ran, fmt.Errorf("record provisioning step %s: %w", s.ID, err)
		}
		done[s.ID] = true
		ran = append(ran, s.ID)
	}
	return ran, nil
}

func runStep(ctx context.Context, sh Shell, p Params, s Step) error {
	if p.Progress != nil {
		p.Progress(s)
	}
	if s.Done != nil {
		if _, err := sh.Shell(ctx, s.Done(p), nil); err == nil {
			return nil
		}
	}
	script, err := s.Script(p)
	if err != nil {
		return fmt.Errorf("provisioning step %s: %w", s.ID, err)
	}
	var stdin io.Reader
	if s.Stdin != nil {
		if stdin, err = s.Stdin(ctx, p); err != nil {
			return fmt.Errorf("provisioning step %s: %w", s.ID, err)
		}
	}
	if _, err := sh.Shell(ctx, script, stdin); err != nil {
		return fmt.Errorf("provisioning step %s (%s): %w", s.ID, s.Description, err)
	}
	return nil
}

// Missing returns the known steps the VM has not recorded, in order.
func Missing(ctx context.Context, sh Shell) ([]Step, error) {
	done, err := Recorded(ctx, sh)
	if err != nil {
		return nil, err
	}
	var out []Step
	for _, s := range Steps() {
		if !done[s.ID] {
			out = append(out, s)
		}
	}
	return out, nil
}

// Recorded reads StateFile from the VM. A missing file means no steps.
// Unknown IDs (e.g. from a newer dev) are kept, harmlessly.
func Recorded(ctx context.Context, sh Shell) (map[string]bool, error) {
	res, err := sh.Shell(ctx, "cat "+StateFile+" 2>/dev/null || true", nil)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", StateFile, err)
	}
	done := map[string]bool{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		if id := strings.TrimSpace(line); id != "" && !strings.HasPrefix(id, "#") {
			done[id] = true
		}
	}
	return done, nil
}

func recordScript(id string) string {
	return fmt.Sprintf("set -euo pipefail\nsudo mkdir -p /etc/dev\n"+
		"grep -qxF %[1]s %[2]s 2>/dev/null || echo %[1]s | sudo tee -a %[2]s >/dev/null\n", shq(id), StateFile)
}

// Lookup returns the step with the given ID.
func Lookup(id string) (Step, bool) {
	for _, s := range Steps() {
		if s.ID == id {
			return s, true
		}
	}
	return Step{}, false
}

func selectSteps(only []string) ([]Step, error) {
	all := Steps()
	if len(only) == 0 {
		return all, nil
	}
	want := map[string]bool{}
	for _, id := range only {
		if _, ok := Lookup(id); !ok {
			return nil, fmt.Errorf("unknown provisioning step %q", id)
		}
		want[id] = true
	}
	var out []Step
	for _, s := range all {
		if want[s.ID] {
			out = append(out, s)
		}
	}
	return out, nil
}

// NeedsSessionReset reports whether ran includes the docker step. Group
// membership changes only apply to new SSH logins, and limactl shell reuses a
// ControlMaster connection, so the caller should then call
// lima.Client.ResetSSH before running docker or devcontainer commands.
func NeedsSessionReset(ran []string) bool {
	for _, id := range ran {
		if id == StepDocker {
			return true
		}
	}
	return false
}

// shq single-quotes s for bash.
func shq(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
