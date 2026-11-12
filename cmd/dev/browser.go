package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/nicorobinreal/dev/internal/browser"
	"github.com/nicorobinreal/dev/internal/state"
)

func init() { register(newBrowserCmd) }

func newBrowserCmd(a *App) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "browser",
		Short: "Give a project's agents access to your Chrome (grant|revoke|status)",
	}
	cmd.AddCommand(&cobra.Command{
		Use:   "grant <name>",
		Short: "Start the Chrome MCP bridge and reverse tunnel, and register the MCP server",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return a.browserGrant(c.Context(), args[0])
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "revoke <name>",
		Short: "Stop the bridge and tunnel and remove the MCP registration",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			lk, err := a.LockProject(args[0])
			if err != nil {
				return err
			}
			defer lk.Unlock()
			p, err := a.Project(args[0])
			if err != nil {
				return err
			}
			if err := a.browserRevoke(c.Context(), p, true); err != nil {
				return err
			}
			return a.Emit(map[string]any{"name": p.Name, "granted": false}, func() {
				a.Printf("browser access revoked for %s\n", p.Name)
			})
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "status <name>",
		Short: "Report whether the bridge and tunnel are running",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			p, err := a.Project(args[0])
			if err != nil {
				return err
			}
			st, err := browser.NewLaunchd(a.Runner, a.Paths).Status(c.Context(), p.Name, p.BrowserPort)
			if err != nil {
				return err
			}
			return a.Emit(map[string]any{"name": p.Name, "status": st, "summary": st.Summary(), "url": mcpURLIf(st)}, func() {
				a.Printf("%s: %s\n", p.Name, st.Summary())
			})
		},
	})
	var port int
	bridge := &cobra.Command{
		Use:    "bridge --port N -- <cmd...>",
		Short:  "Run the stdio→Streamable HTTP MCP bridge (used by the launchd job)",
		Hidden: true,
		Args:   cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if port == 0 {
				return fmt.Errorf("--port is required")
			}
			ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()
			return browser.Bridge(ctx, fmt.Sprintf("127.0.0.1:%d", port), args)
		},
	}
	bridge.Flags().IntVar(&port, "port", 0, "listen port on 127.0.0.1")
	cmd.AddCommand(bridge)
	return cmd
}

func mcpURLIf(st browser.Status) string {
	if st.Port == 0 {
		return ""
	}
	return browser.MCPURL(st.Port)
}

func (a *App) devBinary() string {
	if exe, err := os.Executable(); err == nil {
		if r, err := filepath.EvalSymlinks(exe); err == nil {
			// `go run` binaries live in a temp dir; prefer the installed one.
			if !strings.Contains(r, "/go-build") {
				return r
			}
		}
	}
	return a.Paths.Binary()
}

func (a *App) browserGrant(ctx context.Context, name string) error {
	lk, err := a.LockProject(name)
	if err != nil {
		return err
	}
	defer lk.Unlock()
	p, inst, err := a.loadProject(ctx, name)
	if err != nil {
		return err
	}
	if err := requireRunning(p, inst); err != nil {
		return err
	}
	if p.BrowserPort == 0 {
		all, err := state.ListProjects(a.Paths)
		if err != nil {
			return err
		}
		port, err := browser.FreePort(browser.UsedPorts(all, p.Name))
		if err != nil {
			return err
		}
		p.BrowserPort = port
		if err := state.SaveProject(a.Paths, p); err != nil {
			return err
		}
	}
	ld := browser.NewLaunchd(a.Runner, a.Paths)
	if err := ld.Grant(ctx, p.Name, a.devBinary(), p.BrowserPort); err != nil {
		return err
	}
	res, err := a.containerExec(ctx, p, browser.RegisterScript(p.BrowserPort)...)
	reg := strings.Split(strings.TrimSpace(res.Stdout), "\n")
	if err != nil {
		return fmt.Errorf("bridge and tunnel are running, but registering the MCP server in the container failed: %w", err)
	}
	st, _ := ld.Status(ctx, p.Name, p.BrowserPort)
	a.touch(&p)
	return a.Emit(map[string]any{"name": p.Name, "granted": true, "port": p.BrowserPort, "url": browser.MCPURL(p.BrowserPort), "registered": reg, "status": st}, func() {
		a.Printf("browser access granted for %s: %s in the container\n", p.Name, browser.MCPURL(p.BrowserPort))
		for _, l := range reg {
			if l != "" {
				a.Printf("  %s\n", l)
			}
		}
		a.Printf("Chrome will ask you to approve the first debugging session.\n")
	})
}

// browserRevoke unloads the launchd jobs and, if the container is reachable,
// removes the MCP registration. Caller holds the project lock.
func (a *App) browserRevoke(ctx context.Context, p state.Project, unregister bool) error {
	ld := browser.NewLaunchd(a.Runner, a.Paths)
	st, _ := ld.Status(ctx, p.Name, p.BrowserPort)
	if err := ld.Revoke(ctx, p.Name); err != nil {
		return err
	}
	if unregister && st.Granted() {
		if inst, ok, _ := a.lima().Get(ctx, p.LimaInstance); ok && inst.Running() {
			if _, err := a.containerExec(ctx, p, browser.UnregisterScript()...); err != nil {
				a.Notef("note: could not remove MCP registration in the container: %v\n", err)
			}
		}
	}
	return nil
}
