package main

import (
	"context"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/state"
)

// version is set by GoReleaser via -ldflags "-X main.version=...".
var version = "dev"

// commandRegistry holds constructors for subcommands. Each command file
// registers itself in init() with register(newXCmd).
var commandRegistry []func(*App) *cobra.Command

func register(f func(*App) *cobra.Command) { commandRegistry = append(commandRegistry, f) }

func newRootCmd(app *App) *cobra.Command {
	root := &cobra.Command{
		Use:           "dev",
		Short:         "Per-project Lima VMs with Docker, a devcontainer and a Tailscale address",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return app.setup(cmd, args)
		},
	}
	pf := root.PersistentFlags()
	pf.BoolVar(&app.JSON, "json", false, "machine-readable output")
	pf.BoolVar(&app.DryRun, "dry-run", false, "print what would be done without doing it")
	pf.BoolVar(&app.Verbose, "verbose", false, "mirror the log to stderr")
	for _, f := range commandRegistry {
		root.AddCommand(f(app))
	}
	return root
}

// setup runs before every command: paths, config, logger, runner, and the
// post-run hooks (auto-update check, update/skill notices).
func (a *App) setup(cmd *cobra.Command, args []string) error {
	if a.Paths.Home == "" {
		a.Paths = state.DefaultPaths()
	}
	if a.Log == nil {
		a.Log = state.OpenLogger(a.Paths, a.Verbose)
	}
	a.Log.Logf("start %s %s (version %s)", cmd.CommandPath(), strings.Join(args, " "), a.Version)
	cfg, err := state.LoadConfig(a.Paths)
	if err != nil {
		return err
	}
	a.Config = cfg
	if a.Runner == nil {
		a.Runner = &run.Exec{Log: a.Log}
	}
	if a.DryRun {
		a.real = a.Runner
		a.Runner = &dryRunner{app: a, real: a.Runner}
	}
	if hook := preRunHook; hook != nil {
		hook(a, cmd)
	}
	return nil
}

// preRunHook is set by selfupdate.go (auto-update + notices). Kept as a var
// so tests can disable it.
var preRunHook func(a *App, cmd *cobra.Command)

// dryRunner prints mutating commands instead of running them. Read-only
// commands (status queries) still run so later steps can plan correctly.
type dryRunner struct {
	app  *App
	real run.Runner
}

var readOnlyPrefixes = []string{
	"limactl list", "limactl info", "tailscale status", "gh auth token", "gh auth status",
	"security find-generic-password", "launchctl print", "git ", "uname", "sw_vers",
	"which ", "brew list", "code --list-extensions", "node --version", "sysctl",
}

func (d *dryRunner) Run(ctx context.Context, c run.Cmd) (run.Result, error) {
	line := run.Line(c)
	for _, p := range readOnlyPrefixes {
		if strings.HasPrefix(line, p) {
			return d.real.Run(ctx, c)
		}
	}
	d.app.Notef("[dry-run] %s\n", c.String())
	return run.Result{}, nil
}
