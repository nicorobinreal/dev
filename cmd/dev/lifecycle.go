package main

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/nicorobinreal/dev/internal/devcontainer"
	"github.com/nicorobinreal/dev/internal/gh"
	"github.com/nicorobinreal/dev/internal/lima"
	"github.com/nicorobinreal/dev/internal/provision"
	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/sshconfig"
	"github.com/nicorobinreal/dev/internal/state"
	"github.com/nicorobinreal/dev/internal/tailscale"
)

func init() {
	register(newNewCmd)
	register(newUpCmd)
	register(newDownCmd)
	register(newRebuildCmd)
	register(newRmCmd)
	register(newExecCmd)
	register(newOpenCmd)
}

// provisionParams builds the provisioning inputs for a project.
func (a *App) provisionParams(ctx context.Context, p state.Project, token string) (provision.Params, error) {
	pub, err := a.limaPubKey()
	if err != nil {
		return provision.Params{}, err
	}
	return provision.Params{
		RepoURL:           p.Repo,
		Branch:            p.Branch,
		RepoDir:           p.RepoDir,
		GHToken:           token,
		TailscaleHostname: p.TailscaleHost,
		TailscaleTag:      a.Config.TailscaleTag,
		LimaPubKey:        pub,
		TailscaleAuthKey: func(ctx context.Context) (string, error) {
			return a.tsClient().MintAuthKey(ctx, tailscale.AuthKeyOptions{Tag: a.Config.TailscaleTag, Hostname: p.TailscaleHost})
		},
		Progress: func(s provision.Step) { a.Notef("  • %s\n", s.Description) },
	}, nil
}

// runProvision runs missing (or only the given) steps and resets the SSH
// master connection when group membership changed.
func (a *App) runProvision(ctx context.Context, p state.Project, token string, only []string) ([]string, error) {
	params, err := a.provisionParams(ctx, p, token)
	if err != nil {
		return nil, err
	}
	ran, err := provision.Run(ctx, a.vm(p), params, only)
	if provision.NeedsSessionReset(ran) {
		_ = a.lima().ResetSSH(ctx, p.LimaInstance)
	}
	return ran, err
}

func newNewCmd(a *App) *cobra.Command {
	var repo, branch, memory, disk, editor string
	var cpus int
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "new <name> --repo <url>",
		Short: "Create a VM, provision it, clone the repo, join the tailnet, start the devcontainer and open the editor",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			name := args[0]
			cfg := a.Config
			if err := state.ValidateName(cfg.Machine, name); err != nil {
				return err
			}
			if repo == "" {
				return fmt.Errorf("--repo is required")
			}
			repoURL, repoDir, err := gh.NormalizeRepo(repo)
			if err != nil {
				return err
			}
			if editor == "" {
				editor = cfg.Editor
			}
			if err := validEditor(editor); err != nil {
				return err
			}
			lk, err := a.LockProject(name)
			if err != nil {
				return err
			}
			defer lk.Unlock()
			if _, err := a.Project(name); err == nil {
				return Refuse("project %s already exists (dev up %s, or dev rm %s)", name, name, name)
			}
			if _, ok, err := a.lima().Get(ctx, name); err != nil {
				return err
			} else if ok {
				return Refuse("a Lima instance named %s already exists and isn't managed by dev", name)
			}
			host := state.TailscaleHost(cfg.Machine, name)
			if devs, err := a.tsClient().FindDevices(ctx, host); err != nil {
				return fmt.Errorf("check tailnet for %s: %w", host, err)
			} else if len(devs) > 0 {
				return Refuse("a tailnet node named %s already exists; remove it (dev prune, or the Tailscale admin console) or choose another name", host)
			}
			token, err := gh.HostToken(ctx, a.Runner)
			if err != nil {
				return fmt.Errorf("read gh token (is `gh auth login` done?): %w", err)
			}
			p := state.Project{
				Name: name, Repo: repoURL, RepoDir: repoDir, Branch: branch,
				LimaInstance: name, TailscaleHost: host,
				CPUs: pick(cpus, cfg.CPUs), Memory: pickS(memory, cfg.Memory), Disk: pickS(disk, cfg.Disk),
				Editor: editor, CreatedAt: time.Now().UTC(), LastUsedAt: time.Now().UTC(),
			}
			y, err := lima.GenerateYAML(lima.Spec{Name: name, CPUs: p.CPUs, Memory: p.Memory, Disk: p.Disk})
			if err != nil {
				return err
			}
			a.Notef("creating VM %s (%d CPUs, %s RAM, %s disk)…\n", name, p.CPUs, p.Memory, p.Disk)
			if err := a.lima().Create(ctx, name, y); err != nil {
				return err
			}
			// Save state right after create so `dev rm` can clean up a failed new.
			if !a.DryRun {
				if err := state.SaveProject(a.Paths, p); err != nil {
					return err
				}
			}
			fail := func(err error) error {
				return fmt.Errorf("%w\n(the VM was kept for inspection; `dev up %s` retries, `dev rm --force %s` removes it)", err, name, name)
			}
			a.Notef("starting VM…\n")
			if err := a.lima().Start(ctx, name); err != nil {
				return fail(err)
			}
			home, err := a.lima().Home(ctx, name)
			if err != nil && !a.DryRun {
				return fail(err)
			}
			p.Workspace = path.Join(home, repoDir)
			if !a.DryRun {
				_ = state.SaveProject(a.Paths, p)
			}
			a.Notef("provisioning…\n")
			if _, err := a.runProvision(ctx, p, token, nil); err != nil {
				return fail(err)
			}
			dc, err := a.prepareDevcontainer(ctx, &p)
			if err != nil {
				return fail(err)
			}
			if _, err := a.containerUp(ctx, p, devcontainer.UpOptions{}); err != nil {
				return fail(err)
			}
			if err := a.writeSSHConfig(ctx); err != nil {
				return fail(err)
			}
			a.touch(&p)
			opened := false
			if !noOpen && !a.JSON {
				opened = a.openEditor(ctx, p, editor) == nil
			}
			out := map[string]any{
				"name": p.Name, "repo": p.Repo, "workspace": p.Workspace, "tailscale_host": p.TailscaleHost,
				"url": tailscale.URL(p.TailscaleHost, 0), "ssh_hosts": []string{sshconfig.VMHost(p.Name), sshconfig.ContainerHost(p.Name)},
				"devcontainer": dc, "opened": opened,
			}
			return a.Emit(out, func() {
				a.Printf("✓ %s is ready\n", p.Name)
				a.Printf("  workspace  %s\n  tailnet    %s\n  ssh        %s, %s\n", p.Workspace, tailscale.URL(p.TailscaleHost, 0), sshconfig.VMHost(p.Name), sshconfig.ContainerHost(p.Name))
				if dc["kind"] == string(devcontainer.KindCustom) {
					a.Printf("  ! custom .devcontainer: it may lack what dev relies on (%s); some features may not work\n", strings.Join(requirementNames(), ", "))
				}
				if dc["written"] == true {
					a.Printf("  wrote a starter .devcontainer/ — commit it to the project when you're happy with it\n")
				}
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&repo, "repo", "", "GitHub repo (owner/repo or URL)")
	f.StringVar(&branch, "branch", "", "branch to check out")
	f.IntVar(&cpus, "cpus", 0, "VM CPUs (default from config)")
	f.StringVar(&memory, "memory", "", "VM memory, e.g. 8GiB")
	f.StringVar(&disk, "disk", "", "VM disk, e.g. 60GiB")
	f.BoolVar(&noOpen, "no-open", false, "don't open the editor")
	f.StringVar(&editor, "editor", "", "code|cursor|zed")
	return cmd
}

func requirementNames() []string {
	var out []string
	for _, r := range devcontainer.Requirements {
		out = append(out, r.ID)
	}
	return out
}

// prepareDevcontainer writes the starter when the repo has none and
// classifies what's there. Returns the JSON-ready description.
func (a *App) prepareDevcontainer(ctx context.Context, p *state.Project) (map[string]any, error) {
	res, err := a.lima().Shell(ctx, p.LimaInstance, devcontainer.WriteStarterScript(p.Workspace))
	if err != nil {
		return nil, fmt.Errorf("write starter .devcontainer: %w", err)
	}
	written, _ := devcontainer.StarterWritten(res.Stdout)
	if a.DryRun {
		return map[string]any{"kind": "unknown", "written": false}, nil
	}
	kind, v, _, err := a.detectDevcontainer(ctx, *p)
	if err != nil {
		return nil, err
	}
	p.Devcontainer = devcontainer.Label(kind, v)
	out := map[string]any{"kind": string(kind), "label": p.Devcontainer, "written": written}
	if kind == devcontainer.KindStarter {
		out["version"] = v
	}
	if kind == devcontainer.KindCustom {
		out["requirements"] = devcontainer.Requirements
		out["hint"] = fmt.Sprintf("custom devcontainer: read it with `dev exec %s --vm -- cat %s/.devcontainer/devcontainer.json`, compare with the requirements and suggest a merge", p.Name, p.Workspace)
	}
	return out, nil
}

func pick(v, d int) int {
	if v > 0 {
		return v
	}
	return d
}

func pickS(v, d string) string {
	if v != "" {
		return v
	}
	return d
}

func newUpCmd(a *App) *cobra.Command {
	var noOpen bool
	cmd := &cobra.Command{
		Use:   "up <name>",
		Short: "Start the VM, re-sync the gh token, start the devcontainer, refresh its gh login",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			lk, err := a.LockProject(args[0])
			if err != nil {
				return err
			}
			defer lk.Unlock()
			p, inst, err := a.loadProject(ctx, args[0])
			if err != nil {
				return err
			}
			if !inst.Running() {
				a.warnRAM(ctx, p)
				a.Notef("starting VM %s…\n", p.Name)
				if err := a.lima().Start(ctx, p.LimaInstance); err != nil {
					return err
				}
			}
			token, err := gh.HostToken(ctx, a.Runner)
			if err != nil {
				return fmt.Errorf("read gh token: %w", err)
			}
			if p.Workspace == "" { // a `new` that failed before reading $HOME
				home, err := a.lima().Home(ctx, p.LimaInstance)
				if err != nil {
					return err
				}
				p.Workspace = path.Join(home, p.RepoDir)
			}
			missing, err := provision.Missing(ctx, a.vm(p))
			if err != nil {
				return err
			}
			var hint string
			if ids := stepIDs(missing); len(ids) > 0 {
				// Steps the first `new` never finished are completed here; newly
				// released steps are left for `dev upgrade`.
				if !hasAll(ids, provision.StepContainerSSH) {
					hint = fmt.Sprintf("VM is missing provisioning steps (%s); run `dev upgrade %s`", strings.Join(ids, ", "), p.Name)
					a.Notef("hint: %s\n", hint)
				} else {
					a.Notef("finishing provisioning…\n")
					if _, err := a.runProvision(ctx, p, token, nil); err != nil {
						return err
					}
				}
			}
			a.Notef("syncing gh token…\n")
			if err := gh.SyncVM(ctx, a.vm(p), token); err != nil {
				return err
			}
			if p.Devcontainer == "" {
				if _, err := a.prepareDevcontainer(ctx, &p); err != nil {
					return err
				}
			}
			if _, err := a.containerUp(ctx, p, devcontainer.UpOptions{}); err != nil {
				return err
			}
			if err := a.containerLogin(ctx, p); err != nil {
				a.Notef("warning: gh login in the container failed: %v\n", err)
			}
			if err := a.writeSSHConfig(ctx); err != nil {
				return err
			}
			a.touch(&p)
			opened := false
			if !noOpen && !a.JSON {
				opened = a.openEditor(ctx, p, p.Editor) == nil
			}
			return a.Emit(map[string]any{"name": p.Name, "state": "running", "workspace": p.Workspace, "url": tailscale.URL(p.TailscaleHost, 0), "hint": hint, "opened": opened}, func() {
				a.Printf("✓ %s is up (%s)\n", p.Name, tailscale.URL(p.TailscaleHost, 0))
			})
		},
	}
	cmd.Flags().BoolVar(&noOpen, "no-open", false, "don't open the editor")
	return cmd
}

func stepIDs(s []provision.Step) []string {
	var out []string
	for _, x := range s {
		out = append(out, x.ID)
	}
	return out
}

// hasAll reports whether ids contains want (used to detect an unfinished new:
// the last step is missing along with others).
func hasAll(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

// warnRAM warns when starting p would push running VMs past the configured
// share of host RAM.
func (a *App) warnRAM(ctx context.Context, p state.Project) {
	out, err := run.Output(ctx, a.Runner, "sysctl", "-n", "hw.memsize")
	if err != nil {
		return
	}
	host, err := strconv.ParseInt(out, 10, 64)
	if err != nil || host == 0 {
		return
	}
	insts, err := a.lima().List(ctx)
	if err != nil {
		return
	}
	var used int64
	for _, i := range insts {
		if i.Running() {
			used += i.Memory
		}
		if i.Name == p.LimaInstance && !i.Running() {
			used += i.Memory
		}
	}
	if share := float64(used) / float64(host); share > a.Config.RAMWarnShare {
		a.Notef("warning: running VMs would use %s of %s host RAM (%.0f%%, threshold %.0f%%)\n",
			humanBytes(used), humanBytes(host), share*100, a.Config.RAMWarnShare*100)
	}
}

func newDownCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "down <name>",
		Short: "Revoke browser access and stop the VM",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			lk, err := a.LockProject(args[0])
			if err != nil {
				return err
			}
			defer lk.Unlock()
			p, inst, err := a.loadProject(ctx, args[0])
			if err != nil {
				return err
			}
			if err := a.browserRevoke(ctx, p, true); err != nil {
				return err
			}
			if inst.Running() {
				a.Notef("stopping VM %s…\n", p.Name)
				if err := a.lima().Stop(ctx, p.LimaInstance); err != nil {
					return err
				}
			}
			a.touch(&p)
			return a.Emit(map[string]any{"name": p.Name, "state": "stopped"}, func() {
				a.Printf("✓ %s stopped\n", p.Name)
			})
		},
	}
}

func newRebuildCmd(a *App) *cobra.Command {
	var noCache bool
	cmd := &cobra.Command{
		Use:   "rebuild <name>",
		Short: "Rebuild the devcontainer after config changes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			lk, err := a.LockProject(args[0])
			if err != nil {
				return err
			}
			defer lk.Unlock()
			p, inst, err := a.loadProject(ctx, args[0])
			if err != nil {
				return err
			}
			if err := requireRunning(p, inst); err != nil {
				return err
			}
			up, err := a.containerUp(ctx, p, devcontainer.UpOptions{RemoveExisting: true, BuildNoCache: noCache})
			if err != nil {
				return err
			}
			if kind, v, _, err := a.detectDevcontainer(ctx, p); err == nil {
				p.Devcontainer = devcontainer.Label(kind, v)
			}
			a.touch(&p)
			return a.Emit(map[string]any{"name": p.Name, "container_id": up.ContainerID}, func() {
				a.Printf("✓ rebuilt devcontainer for %s\n", p.Name)
			})
		},
	}
	cmd.Flags().BoolVar(&noCache, "no-cache", false, "build without the Docker cache")
	return cmd
}

func newRmCmd(a *App) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm <name>",
		Short: "Delete the VM, its tailnet node and SSH entries (refuses if there is unpushed work)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			lk, err := a.LockProject(args[0])
			if err != nil {
				return err
			}
			defer lk.Unlock()
			p, err := a.Project(args[0])
			if err != nil {
				return err
			}
			if err := a.removeProject(ctx, p, force); err != nil {
				return err
			}
			return a.Emit(map[string]any{"name": p.Name, "removed": true}, func() {
				a.Printf("✓ removed %s\n", p.Name)
			})
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove even with uncommitted or unpushed work")
	return cmd
}

// removeProject implements rm (caller holds the project lock).
func (a *App) removeProject(ctx context.Context, p state.Project, force bool) error {
	inst, exists, err := a.lima().Get(ctx, p.LimaInstance)
	if err != nil {
		return err
	}
	if exists && !force {
		if !inst.Running() {
			a.Notef("starting %s to check for unpushed work…\n", p.Name)
			if err := a.lima().Start(ctx, p.LimaInstance); err != nil {
				return fmt.Errorf("start VM to check for unpushed work (use --force to skip): %w", err)
			}
		}
		if err := a.workCheck(ctx, p); err != nil {
			return err
		}
	}
	if err := a.browserRevoke(ctx, p, false); err != nil {
		return err
	}
	if exists {
		a.Notef("deleting VM %s…\n", p.Name)
		if err := a.lima().Delete(ctx, p.LimaInstance, true); err != nil {
			return err
		}
	}
	if !a.DryRun {
		if n, err := a.tsClient().DeleteByHostname(ctx, p.TailscaleHost); err != nil {
			if !errors.Is(err, tailscale.ErrNoCredentials) {
				return fmt.Errorf("delete tailnet node %s: %w", p.TailscaleHost, err)
			}
			a.Notef("warning: no Tailscale credentials; remove node %s by hand\n", p.TailscaleHost)
		} else if n > 0 {
			a.Notef("removed tailnet node %s\n", p.TailscaleHost)
		}
		if l, err := state.LockGlobal(a.Paths); err == nil {
			_ = sshconfig.ForgetHostKey(a.Paths, p.Name)
			l.Unlock()
		}
		if err := state.DeleteProject(a.Paths, p.Name); err != nil {
			return err
		}
	}
	return a.writeSSHConfig(ctx)
}

// exitStatusError makes dev exit with a child's exit code.
type exitStatusError struct{ code int }

func (e *exitStatusError) Error() string { return fmt.Sprintf("command exited %d", e.code) }

func newExecCmd(a *App) *cobra.Command {
	var onVM bool
	cmd := &cobra.Command{
		Use:   "exec <name> [--vm] -- <cmd...>",
		Short: "Run a command in the devcontainer (or the VM with --vm)",
		Args:  cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			p, inst, err := a.loadProject(ctx, args[0])
			if err != nil {
				return err
			}
			if err := requireRunning(p, inst); err != nil {
				return err
			}
			argv := args[1:]
			if !onVM {
				argv = devcontainer.ExecArgs(p.Workspace, argv...)
			}
			wd := p.Workspace
			if wd == "" {
				wd = "/"
			}
			_, err = a.lima().Exec(ctx, p.LimaInstance, wd, argv...)
			var ee *run.ExitError
			if errors.As(err, &ee) {
				return &exitStatusError{code: ee.Result.ExitCode}
			}
			return err
		},
	}
	cmd.Flags().BoolVar(&onVM, "vm", false, "run on the VM instead of in the container")
	return cmd
}

func validEditor(e string) error {
	switch e {
	case "code", "cursor", "zed":
		return nil
	}
	return fmt.Errorf("unknown editor %q (code|cursor|zed)", e)
}

func newOpenCmd(a *App) *cobra.Command {
	var editor string
	cmd := &cobra.Command{
		Use:   "open <name>",
		Short: "Open the editor on the VM over Remote-SSH, in the repo folder",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.Project(args[0])
			if err != nil {
				return err
			}
			if editor == "" {
				editor = pickS(p.Editor, a.Config.Editor)
			}
			if err := validEditor(editor); err != nil {
				return err
			}
			if err := a.openEditor(cmd.Context(), p, editor); err != nil {
				return err
			}
			a.touch(&p)
			return a.Emit(map[string]any{"name": p.Name, "editor": editor, "opened": true}, nil)
		},
	}
	cmd.Flags().StringVar(&editor, "editor", "", "code|cursor|zed")
	return cmd
}

// openEditor launches the editor. VS Code and Cursor connect to the VM
// (lima-<name>) so Dev Containers can reopen in the container; Zed has no
// Dev Containers support and connects to the container's sshd (<name>-dev).
func (a *App) openEditor(ctx context.Context, p state.Project, editor string) error {
	var c run.Cmd
	switch editor {
	case "zed":
		c = run.Cmd{Name: "zed", Args: []string{"ssh://" + sshconfig.ContainerHost(p.Name) + p.Workspace}}
	case "cursor", "code", "":
		bin := pickS(editor, "code")
		c = run.Cmd{Name: bin, Args: []string{"--remote", "ssh-remote+" + sshconfig.VMHost(p.Name), p.Workspace}}
	default:
		return validEditor(editor)
	}
	if _, err := a.Runner.Run(ctx, c); err != nil {
		a.Notef("could not open %s: %v\n", editor, err)
		return err
	}
	return nil
}
