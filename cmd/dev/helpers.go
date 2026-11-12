package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/nicorobinreal/dev/internal/lima"
	"github.com/nicorobinreal/dev/internal/sshconfig"
	"github.com/nicorobinreal/dev/internal/state"
)

// lima returns a limactl client on the app's runner.
func (a *App) lima() *lima.Client { return &lima.Client{Runner: a.Runner} }

// vm returns the provision/gh Shell bound to a project's instance.
func (a *App) vm(p state.Project) lima.VM { return a.lima().VM(p.LimaInstance) }

// loadProject loads state and verifies the Lima instance exists.
func (a *App) loadProject(ctx context.Context, name string) (state.Project, lima.Instance, error) {
	p, err := a.Project(name)
	if err != nil {
		return p, lima.Instance{}, err
	}
	inst, ok, err := a.lima().Get(ctx, p.LimaInstance)
	if err != nil {
		return p, inst, err
	}
	if !ok {
		return p, inst, fmt.Errorf("project %s: Lima instance %s not found (state is stale; `dev rm --force %s` cleans it up)", name, p.LimaInstance, name)
	}
	return p, inst, nil
}

// requireRunning errors unless the instance is running.
func requireRunning(p state.Project, inst lima.Instance) error {
	if !inst.Running() {
		return fmt.Errorf("project %s is %s; run `dev up %s` first", p.Name, strings.ToLower(inst.Status), p.Name)
	}
	return nil
}

// touch updates last-used time.
func (a *App) touch(p *state.Project) {
	if a.DryRun {
		return
	}
	p.LastUsedAt = time.Now().UTC()
	_ = state.SaveProject(a.Paths, *p)
}

// writeSSHConfig regenerates ~/.config/dev/ssh_config for all projects and
// ensures the Include line, under the global lock.
func (a *App) writeSSHConfig(ctx context.Context) error {
	if a.DryRun {
		a.Notef("[dry-run] rewrite %s\n", a.Paths.SSHConfigFile())
		return nil
	}
	projects, err := state.ListProjects(a.Paths)
	if err != nil {
		return err
	}
	l, err := state.LockGlobal(a.Paths)
	if err != nil {
		return err
	}
	defer l.Unlock()
	var entries []sshconfig.Entry
	for _, p := range projects {
		raw, err := sshconfig.LoadLimaConfig(a.Paths, p.LimaInstance)
		if err != nil || raw == "" {
			raw, _ = a.lima().ShowSSH(ctx, p.LimaInstance)
		}
		entries = append(entries, sshconfig.NewEntry(a.Paths, p.Name, raw))
	}
	if err := sshconfig.Write(a.Paths, entries); err != nil {
		return err
	}
	_, err = sshconfig.EnsureInclude(a.Paths)
	return err
}

// limaPubKey reads Lima's own public key.
func (a *App) limaPubKey() (string, error) {
	b, err := os.ReadFile(a.Paths.LimaKey() + ".pub")
	if err != nil {
		return "", fmt.Errorf("read Lima public key (created on first `limactl create`): %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func humanBytes(n int64) string {
	if n <= 0 {
		return "-"
	}
	const u = 1024
	units := []string{"B", "K", "M", "G", "T"}
	f := float64(n)
	i := 0
	for f >= u && i < len(units)-1 {
		f /= u
		i++
	}
	if f >= 10 || i == 0 {
		return fmt.Sprintf("%.0f%s", f, units[i])
	}
	return fmt.Sprintf("%.1f%s", f, units[i])
}
