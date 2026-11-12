package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/nicorobinreal/dev/internal/browser"
	"github.com/nicorobinreal/dev/internal/gh"
	"github.com/nicorobinreal/dev/internal/lima"
	"github.com/nicorobinreal/dev/internal/provision"
	"github.com/nicorobinreal/dev/internal/skill"
	"github.com/nicorobinreal/dev/internal/sshconfig"
	"github.com/nicorobinreal/dev/internal/state"
	"github.com/nicorobinreal/dev/internal/tailscale"
)

func init() {
	register(newLsCmd)
	register(newUpgradeCmd)
	register(newPruneCmd)
	register(newInitCmd)
	register(newSelfUninstallCmd)
	postUpdateHook = syncSkill
}

// syncSkill rewrites the embedded skill when the binary's copy differs from
// what's installed (i.e. after a self-update), and prints reload notices.
func syncSkill(a *App) {
	if !a.Config.Initialized || a.DryRun {
		return
	}
	prev, _ := os.ReadFile(skill.HashFile(a.Paths))
	if strings.TrimSpace(string(prev)) == skill.Hash() {
		return
	}
	res, err := skill.Install(a.Paths, a.Config.Harnesses)
	if err != nil {
		a.Log.Logf("skill install: %v", err)
		return
	}
	for _, n := range res.Notices() {
		a.Notef("%s\n", n)
	}
}

type lsRow struct {
	Name          string         `json:"name"`
	State         string         `json:"state"`
	Repo          string         `json:"repo"`
	Branch        string         `json:"branch,omitempty"`
	TailscaleHost string         `json:"tailscale_host"`
	CPUs          int            `json:"cpus"`
	Memory        int64          `json:"memory_bytes"`
	Disk          int64          `json:"disk_bytes"`
	Usage         *lima.Usage    `json:"usage,omitempty"`
	Browser       browser.Status `json:"browser"`
	Devcontainer  string         `json:"devcontainer,omitempty"`
	LastUsedAt    time.Time      `json:"last_used_at"`
}

func newLsCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "ls",
		Short: "List projects: state, branch, Tailscale name, CPU/RAM/disk, browser access",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			projects, err := state.ListProjects(a.Paths)
			if err != nil {
				return err
			}
			insts, err := a.lima().List(ctx)
			if err != nil {
				return err
			}
			byName := map[string]lima.Instance{}
			for _, i := range insts {
				byName[i.Name] = i
			}
			ld := browser.NewLaunchd(a.Runner, a.Paths)
			rows := []lsRow{}
			for _, p := range projects {
				r := lsRow{Name: p.Name, Repo: p.Repo, TailscaleHost: p.TailscaleHost, CPUs: p.CPUs, Devcontainer: p.Devcontainer, LastUsedAt: p.LastUsedAt, State: "missing"}
				if i, ok := byName[p.LimaInstance]; ok {
					r.State = strings.ToLower(i.Status)
					r.CPUs, r.Memory, r.Disk = i.CPUs, i.Memory, i.Disk
					if i.Running() {
						if u, err := a.lima().Usage(ctx, p.LimaInstance); err == nil {
							r.Usage = &u
						}
						if b, err := a.lima().Shell(ctx, p.LimaInstance, "git -C \"$HOME/"+p.RepoDir+"\" branch --show-current 2>/dev/null"); err == nil {
							r.Branch = strings.TrimSpace(b.Stdout)
						}
					}
				}
				if r.Branch == "" {
					r.Branch = p.Branch
				}
				r.Browser, _ = ld.Status(ctx, p.Name, p.BrowserPort)
				rows = append(rows, r)
			}
			return a.Emit(map[string]any{"projects": rows}, func() {
				if len(rows) == 0 {
					a.Printf("no projects (create one with `dev new <name> --repo <owner/repo>`)\n")
					return
				}
				tw := tabwriter.NewWriter(a.Out, 0, 2, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tSTATE\tBRANCH\tTAILSCALE\tCPU\tRAM\tDISK\tBROWSER")
				for _, r := range rows {
					cpu, ram, disk := strconv.Itoa(r.CPUs), humanBytes(r.Memory), humanBytes(r.Disk)
					if u := r.Usage; u != nil {
						cpu = fmt.Sprintf("%.1f/%d", u.Load1, r.CPUs)
						ram = humanBytes(u.MemUsed) + "/" + humanBytes(r.Memory)
						disk = humanBytes(u.DiskUsed) + "/" + humanBytes(r.Disk)
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Name, r.State, dash(r.Branch), r.TailscaleHost, cpu, ram, disk, r.Browser.Summary())
				}
				tw.Flush()
			})
		},
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func newUpgradeCmd(a *App) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "upgrade <name>",
		Short: "Run provisioning steps the VM hasn't run yet",
		Args: func(cmd *cobra.Command, args []string) error {
			if all {
				return cobra.NoArgs(cmd, args)
			}
			return cobra.ExactArgs(1)(cmd, args)
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			names := args
			if all {
				ps, err := state.ListProjects(a.Paths)
				if err != nil {
					return err
				}
				names = nil
				for _, p := range ps {
					names = append(names, p.Name)
				}
			}
			results := map[string][]string{}
			var firstErr error
			for _, n := range names {
				ran, err := a.upgradeOne(ctx, n)
				results[n] = ran
				if err != nil {
					a.Notef("%s: %v\n", n, err)
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", n, err)
					}
				}
			}
			if err := a.Emit(map[string]any{"ran": results}, func() {
				for _, n := range names {
					if len(results[n]) == 0 {
						a.Printf("%s: up to date\n", n)
					} else {
						a.Printf("%s: ran %s\n", n, strings.Join(results[n], ", "))
					}
				}
			}); err != nil {
				return err
			}
			return firstErr
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "upgrade every project (running ones; stopped VMs are started)")
	return cmd
}

func (a *App) upgradeOne(ctx context.Context, name string) ([]string, error) {
	lk, err := a.LockProject(name)
	if err != nil {
		return nil, err
	}
	defer lk.Unlock()
	p, inst, err := a.loadProject(ctx, name)
	if err != nil {
		return nil, err
	}
	if !inst.Running() {
		if err := a.lima().Start(ctx, p.LimaInstance); err != nil {
			return nil, err
		}
	}
	missing, err := provision.Missing(ctx, a.vm(p))
	if err != nil || len(missing) == 0 {
		return nil, err
	}
	token, err := gh.HostToken(ctx, a.Runner)
	if err != nil {
		return nil, err
	}
	return a.runProvision(ctx, p, token, stepIDs(missing))
}

func newPruneCmd(a *App) *cobra.Command {
	var olderThan string
	var force, dockerPrune bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Remove stopped VMs and orphaned tailnet nodes older than a threshold",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			age, err := parseAge(olderThan)
			if err != nil {
				return err
			}
			projects, err := state.ListProjects(a.Paths)
			if err != nil {
				return err
			}
			insts, err := a.lima().List(ctx)
			if err != nil {
				return err
			}
			byName := map[string]lima.Instance{}
			for _, i := range insts {
				byName[i.Name] = i
			}
			now := time.Now()
			var removed, skipped, dockerPruned []string
			skipReasons := map[string]string{}
			for _, p := range projects {
				inst, ok := byName[p.LimaInstance]
				if ok && inst.Running() {
					if dockerPrune {
						if _, err := a.lima().Shell(ctx, p.LimaInstance, "docker system prune -f"); err == nil {
							dockerPruned = append(dockerPruned, p.Name)
						}
					}
					continue
				}
				if now.Sub(p.LastUsedAt) < age {
					continue
				}
				lk, err := a.LockProject(p.Name)
				if err != nil {
					skipped = append(skipped, p.Name)
					skipReasons[p.Name] = err.Error()
					continue
				}
				err = a.removeProject(ctx, p, force)
				lk.Unlock()
				if err != nil {
					skipped = append(skipped, p.Name)
					skipReasons[p.Name] = err.Error()
					continue
				}
				removed = append(removed, p.Name)
			}
			// Orphaned tailnet nodes: tagged, this machine's prefix, no project.
			known := map[string]bool{}
			if ps, err := state.ListProjects(a.Paths); err == nil {
				for _, p := range ps {
					known[p.TailscaleHost] = true
				}
			}
			var orphans []string
			if devs, err := a.tsClient().ListDevices(ctx); err == nil {
				for _, d := range tailscale.Orphans(devs, tailscale.OrphanFilter{Tag: a.Config.TailscaleTag, Machine: a.Config.Machine, Known: known, MinAge: age, Now: now}) {
					if a.DryRun {
						a.Notef("[dry-run] delete tailnet node %s\n", d.ShortName())
						continue
					}
					if err := a.tsClient().DeleteDevice(ctx, d.APIID()); err == nil {
						orphans = append(orphans, d.ShortName())
					}
				}
			} else {
				a.Notef("warning: could not list tailnet nodes: %v\n", err)
			}
			return a.Emit(map[string]any{"removed": removed, "skipped": skipReasons, "orphan_nodes_removed": orphans, "docker_pruned": dockerPruned}, func() {
				for _, n := range removed {
					a.Printf("removed %s\n", n)
				}
				for _, n := range skipped {
					a.Printf("skipped %s: %s\n", n, skipReasons[n])
				}
				for _, n := range orphans {
					a.Printf("removed orphaned tailnet node %s\n", n)
				}
				for _, n := range dockerPruned {
					a.Printf("docker system prune in %s\n", n)
				}
				if len(removed)+len(skipped)+len(orphans)+len(dockerPruned) == 0 {
					a.Printf("nothing to prune\n")
				}
			})
		},
	}
	cmd.Flags().StringVar(&olderThan, "older-than", "14d", "only prune VMs unused for this long (e.g. 14d, 36h)")
	cmd.Flags().BoolVar(&force, "force", false, "remove even with uncommitted or unpushed work")
	cmd.Flags().BoolVar(&dockerPrune, "docker", false, "also run docker system prune inside running VMs")
	return cmd
}

func parseAge(s string) (time.Duration, error) {
	if strings.HasSuffix(s, "d") {
		n, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil {
			return 0, fmt.Errorf("invalid --older-than %q", s)
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("invalid --older-than %q", s)
	}
	return d, nil
}

func newInitCmd(a *App) *cobra.Command {
	var machine, harnesses, memory, disk, editor string
	var cpus int
	cmd := &cobra.Command{
		Use:   "init",
		Short: "First-run setup: machine prefix, harnesses, default VM resources; installs the skill and runs doctor",
		Long: `First-run setup: machine prefix, which harnesses get the dev skill, and
default VM resources. Saves the config, installs the skill and runs doctor.

In a terminal (without --json) it asks for each value, offering the flag or
current value as the default. Otherwise it never prompts: it uses the flags,
then the current config, then built-in defaults. Agents should pass --json and
any flags they need.`,
		Example: `  dev init --json
  dev init --json --machine mbp --harnesses claude,codex --cpus 6 --memory 12GiB`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c := a.Config
			f := cmd.Flags()
			if f.Changed("machine") {
				c.Machine = machine
			}
			if f.Changed("harnesses") {
				c.Harnesses = splitList(harnesses)
			}
			if f.Changed("cpus") {
				c.CPUs = cpus
			}
			if f.Changed("memory") {
				c.Memory = memory
			}
			if f.Changed("disk") {
				c.Disk = disk
			}
			if f.Changed("editor") {
				c.Editor = editor
			}
			interactive := isTTY() && !a.JSON
			if interactive {
				if err := askInit(a, &c); err != nil {
					return err
				}
			}
			if err := validateInit(c); err != nil {
				return err
			}
			c.Initialized = true
			if err := state.SaveConfig(a.Paths, c); err != nil {
				return err
			}
			a.Config = c
			res, err := skill.Install(a.Paths, c.Harnesses)
			if err != nil {
				return err
			}
			if !a.JSON {
				fmt.Fprintf(a.Out, "✓ config saved to %s\n✓ skill installed: %s\n", a.Paths.ConfigFile(), strings.Join(res.Dirs, ", "))
				for _, n := range res.Notices() {
					fmt.Fprintln(a.Out, n)
				}
				fmt.Fprintln(a.Out, "\nRunning dev doctor…")
				doc := newDoctorCmd(a)
				doc.SetContext(cmd.Context())
				return doc.RunE(doc, nil)
			}
			checks := a.doctorChecks(cmd.Context())
			failed := failedChecks(checks)
			if err := a.Emit(map[string]any{
				"config": c, "config_file": a.Paths.ConfigFile(),
				"skill":  map[string]any{"dirs": res.Dirs, "reload": res.Notices()},
				"doctor": map[string]any{"checks": checks, "ok": failed == 0},
			}, nil); err != nil {
				return err
			}
			if failed > 0 {
				return &exitStatusError{code: ExitError} // details are in the JSON
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&machine, "machine", "", "machine prefix; Tailscale names are <machine>-<project> (default: short hostname)")
	f.StringVar(&harnesses, "harnesses", "", "comma-separated harnesses that get the skill: claude,codex,opencode")
	f.IntVar(&cpus, "cpus", 0, "default VM CPUs")
	f.StringVar(&memory, "memory", "", "default VM memory, e.g. 8GiB")
	f.StringVar(&disk, "disk", "", "default VM disk, e.g. 60GiB")
	f.StringVar(&editor, "editor", "", "editor: code|cursor|zed")
	return cmd
}

// askInit prompts for each init value, offering the current one as default.
func askInit(a *App, c *state.Config) error {
	in := bufio.NewReader(os.Stdin)
	ask := func(q, def string) string {
		fmt.Fprintf(a.Out, "%s [%s]: ", q, def)
		line, _ := in.ReadString('\n')
		if line = strings.TrimSpace(line); line != "" {
			return line
		}
		return def
	}
	for {
		c.Machine = ask("Machine prefix (Tailscale names are <machine>-<project>)", c.Machine)
		err := state.ValidateMachine(c.Machine)
		if err == nil {
			break
		}
		fmt.Fprintf(a.Out, "  %v\n", err)
	}
	c.Harnesses = splitList(ask("Harnesses that get the dev skill (claude,codex,opencode)", strings.Join(c.Harnesses, ",")))
	if n, err := strconv.Atoi(ask("Default VM CPUs", strconv.Itoa(c.CPUs))); err == nil && n > 0 {
		c.CPUs = n
	}
	c.Memory = ask("Default VM memory", c.Memory)
	c.Disk = ask("Default VM disk", c.Disk)
	c.Editor = ask("Editor (code|cursor|zed)", c.Editor)
	return nil
}

func validateInit(c state.Config) error {
	if err := state.ValidateMachine(c.Machine); err != nil {
		return err
	}
	for _, h := range c.Harnesses {
		if _, ok := skill.Lookup(h); !ok {
			return fmt.Errorf("unknown harness %q (claude, codex, opencode)", h)
		}
	}
	if c.CPUs < 1 {
		return fmt.Errorf("invalid --cpus %d", c.CPUs)
	}
	if err := (lima.Spec{Name: "x", CPUs: c.CPUs, Memory: c.Memory, Disk: c.Disk}).Validate(); err != nil {
		return err
	}
	return validEditor(c.Editor)
}

func splitList(s string) []string {
	var out []string
	for _, h := range strings.Split(s, ",") {
		if h = strings.TrimSpace(h); h != "" {
			out = append(out, h)
		}
	}
	return out
}

func newSelfUninstallCmd(a *App) *cobra.Command {
	var purge bool
	cmd := &cobra.Command{
		Use:   "self-uninstall",
		Short: "Remove the binary, skills and SSH config entries; keep VMs unless --purge",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			projects, err := state.ListProjects(a.Paths)
			if err != nil {
				return err
			}
			var removedVMs []string
			for _, p := range projects {
				lk, err := a.LockProject(p.Name)
				if err != nil {
					return err
				}
				if purge {
					err = a.removeProject(ctx, p, true)
					if err == nil {
						removedVMs = append(removedVMs, p.Name)
					}
				} else {
					err = a.browserRevoke(ctx, p, false)
				}
				lk.Unlock()
				if err != nil {
					return fmt.Errorf("%s: %w", p.Name, err)
				}
			}
			if a.DryRun {
				a.Notef("[dry-run] remove skills, SSH config, binary\n")
				return nil
			}
			dirs, err := skill.Uninstall(a.Paths)
			if err != nil {
				return err
			}
			l, err := state.LockGlobal(a.Paths)
			if err != nil {
				return err
			}
			_, _ = sshconfig.RemoveInclude(a.Paths)
			_ = os.Remove(a.Paths.SSHConfigFile())
			l.Unlock()
			if purge {
				_ = os.RemoveAll(a.Paths.StateDir())
				_ = os.RemoveAll(a.Paths.ConfigDir())
			}
			exe := a.devBinary()
			_ = os.Remove(exe)
			return a.Emit(map[string]any{"removed_binary": exe, "removed_skills": dirs, "removed_vms": removedVMs, "kept_vms": !purge}, func() {
				a.Printf("✓ removed %s, the skill and SSH config entries\n", exe)
				if purge {
					a.Printf("✓ deleted %d VM(s) and their tailnet nodes\n", len(removedVMs))
				} else if len(projects) > 0 {
					a.Printf("kept %d VM(s); manage them with limactl, or reinstall dev\n", len(projects))
				}
			})
		},
	}
	cmd.Flags().BoolVar(&purge, "purge", false, "also delete VMs, their tailnet nodes, config and state")
	return cmd
}
