package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/nicorobinreal/dev/internal/devcontainer"
	"github.com/nicorobinreal/dev/internal/gh"
	"github.com/nicorobinreal/dev/internal/lima"
	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/state"
)

// containerExec runs argv in the project's devcontainer (non-interactive).
func (a *App) containerExec(ctx context.Context, p state.Project, argv ...string) (run.Result, error) {
	script := devcontainer.Script(devcontainer.ExecArgs(p.Workspace, argv...))
	return a.lima().Shell(ctx, p.LimaInstance, script, lima.WithWorkdir(p.Workspace))
}

// containerUp runs `devcontainer up` (or a rebuild) in the VM.
func (a *App) containerUp(ctx context.Context, p state.Project, o devcontainer.UpOptions) (devcontainer.UpResult, error) {
	o.StarterImage = os.Getenv(devcontainer.StarterImageEnv)
	a.Notef("starting devcontainer (devcontainer up)…\n")
	script := devcontainer.Script(devcontainer.UpArgs(p.Workspace, o))
	res, err := a.lima().Shell(ctx, p.LimaInstance, script, lima.WithWorkdir(p.Workspace))
	if a.DryRun {
		return devcontainer.UpResult{Outcome: "success"}, nil
	}
	up, perr := devcontainer.ParseUpOutput(res.Stdout)
	if perr != nil {
		if err != nil {
			return up, fmt.Errorf("%v (%w)", perr, err)
		}
		return up, perr
	}
	return up, err
}

// containerLogin refreshes the container's gh login from the mounted token
// (postStartCommand doesn't fire when the container was already running).
func (a *App) containerLogin(ctx context.Context, p state.Project) error {
	_, err := a.containerExec(ctx, p, gh.ContainerLoginArgs()...)
	return err
}

// detectDevcontainer reads .devcontainer/ from the VM and classifies it.
func (a *App) detectDevcontainer(ctx context.Context, p state.Project) (devcontainer.Kind, int, map[string][]byte, error) {
	res, err := a.lima().Shell(ctx, p.LimaInstance, devcontainer.ReadScript(p.Workspace))
	if err != nil {
		return "", 0, nil, err
	}
	files, err := devcontainer.ParseReadOutput(res.Stdout)
	if err != nil {
		return "", 0, nil, err
	}
	kind, v := devcontainer.Detect(files)
	return kind, v, files, nil
}

// workCheck returns a refusal describing uncommitted or unpushed work, or
// nil when the clone is clean (an untracked pristine starter doesn't count).
func (a *App) workCheck(ctx context.Context, p state.Project) error {
	ws, err := gh.WorkCheck(ctx, a.vm(p), p.RepoDir)
	if err != nil {
		return fmt.Errorf("check for unpushed work: %w", err)
	}
	var files map[string][]byte
	for _, u := range ws.Untracked {
		if strings.TrimSuffix(u, "/") == devcontainer.Dir || strings.HasPrefix(u, devcontainer.Dir+"/") {
			_, _, files, _ = a.detectDevcontainer(ctx, p)
			break
		}
	}
	untracked := devcontainer.FilterUntracked(ws.Untracked, files)
	if len(untracked) == 0 && len(ws.Modified) == 0 && len(ws.Unpushed) == 0 && ws.Stashes == 0 {
		return nil
	}
	var parts []string
	if n := len(ws.Modified); n > 0 {
		parts = append(parts, fmt.Sprintf("%d modified (%s)", n, preview(ws.Modified)))
	}
	if n := len(untracked); n > 0 {
		parts = append(parts, fmt.Sprintf("%d untracked (%s)", n, preview(untracked)))
	}
	if n := len(ws.Unpushed); n > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commit(s)", n))
	}
	if ws.Stashes > 0 {
		parts = append(parts, fmt.Sprintf("%d stash(es)", ws.Stashes))
	}
	return Refuse("%s has uncommitted or unpushed work: %s (use --force to discard)", p.Name, strings.Join(parts, ", "))
}

func preview(s []string) string {
	if len(s) > 3 {
		return strings.Join(s[:3], ", ") + ", …"
	}
	return strings.Join(s, ", ")
}
