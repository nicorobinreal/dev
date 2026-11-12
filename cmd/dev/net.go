package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/nicorobinreal/dev/internal/shellquote"
	"github.com/nicorobinreal/dev/internal/state"
	"github.com/nicorobinreal/dev/internal/tailscale"
)

func init() {
	register(newURLCmd)
	register(newServeCmd)
	register(newSSHConfigCmd)
}

func newURLCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "url <name> [port]",
		Short: "Print the Tailscale URL",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p, err := a.Project(args[0])
			if err != nil {
				return err
			}
			port := 0
			if len(args) == 2 {
				if port, err = strconv.Atoi(args[1]); err != nil || port < 1 || port > 65535 {
					return fmt.Errorf("invalid port %q", args[1])
				}
			}
			u := tailscale.URL(p.TailscaleHost, port)
			return a.Emit(map[string]any{"name": p.Name, "host": p.TailscaleHost, "port": port, "url": u}, func() {
				a.Printf("%s\n", u)
			})
		},
	}
}

func newServeCmd(a *App) *cobra.Command {
	var off bool
	cmd := &cobra.Command{
		Use:   "serve <name> <port>",
		Short: "Expose a port via tailscale serve with HTTPS (needs HTTPS certificates on the tailnet)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			port, err := strconv.Atoi(args[1])
			if err != nil || port < 1 || port > 65535 {
				return fmt.Errorf("invalid port %q", args[1])
			}
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
			if off {
				res, err := a.lima().Shell(ctx, p.LimaInstance, shellquote.Quote(tailscale.ServeOffArgs(port)...))
				if err != nil && !tailscale.IsServeNotFound(res.Stdout+res.Stderr+err.Error()) {
					return err
				}
				p.Served = removeInt(p.Served, port)
				a.touch(&p)
				return a.Emit(map[string]any{"name": p.Name, "port": port, "serving": false}, func() {
					a.Printf("stopped serving port %d\n", port)
				})
			}
			st, err := vmTailscaleStatus(ctx, a, p)
			if err != nil {
				return err
			}
			if !st.HTTPSEnabled() {
				return Refuse("%s", tailscale.HTTPSDisabledHelp(p.TailscaleHost, st.MagicDNSSuffix))
			}
			if _, err := a.lima().Shell(ctx, p.LimaInstance, shellquote.Quote(tailscale.ServeOnArgs(port)...)); err != nil {
				return err
			}
			if !containsInt(p.Served, port) {
				p.Served = append(p.Served, port)
			}
			a.touch(&p)
			u := tailscale.HTTPSURL(p.TailscaleHost, st.MagicDNSSuffix, port)
			return a.Emit(map[string]any{"name": p.Name, "port": port, "serving": true, "url": u}, func() {
				a.Printf("%s\n", u)
			})
		},
	}
	cmd.Flags().BoolVar(&off, "off", false, "stop serving the port")
	return cmd
}

func vmTailscaleStatus(ctx context.Context, a *App, p state.Project) (tailscale.Status, error) {
	res, err := a.lima().Shell(ctx, p.LimaInstance, "tailscale status --json")
	if err != nil {
		return tailscale.Status{}, fmt.Errorf("tailscale status in VM: %w", err)
	}
	return tailscale.ParseStatus([]byte(res.Stdout))
}

func newSSHConfigCmd(a *App) *cobra.Command {
	return &cobra.Command{
		Use:   "ssh-config",
		Short: "Regenerate the lima-<name> and <name>-dev SSH host entries",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.writeSSHConfig(cmd.Context()); err != nil {
				return err
			}
			return a.Emit(map[string]any{"file": a.Paths.SSHConfigFile()}, func() {
				a.Printf("wrote %s\n", a.Paths.SSHConfigFile())
			})
		},
	}
}

func containsInt(s []int, v int) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func removeInt(s []int, v int) []int {
	out := s[:0]
	for _, x := range s {
		if x != v {
			out = append(out, x)
		}
	}
	return out
}
