package main

import (
	"time"

	"github.com/spf13/cobra"

	"github.com/nicorobinreal/dev/internal/update"
)

func init() {
	register(newSelfUpdateCmd)
	preRunHook = autoUpdateHook
}

// autoUpdateHook prints a pending "dev updated" notice and, at most once an
// hour, spawns a detached background update check.
func autoUpdateHook(a *App, cmd *cobra.Command) {
	if msg, err := update.TakeNotice(a.Paths); err == nil && msg != "" {
		a.Notef("%s\n", msg)
	}
	if postUpdateHook != nil {
		postUpdateHook(a)
	}
	if a.DryRun || cmd.Name() == "self-update" || cmd.Hidden {
		return
	}
	now := time.Now()
	if update.AutoEnabled(a.Version) && update.ShouldCheck(a.Paths, now) {
		if err := update.Spawn(a.Paths, now); err != nil {
			a.Log.Logf("auto-update spawn: %v", err)
		}
	}
}

// postUpdateHook keeps the installed skill in sync with the binary (set in
// skill.go); runs on every invocation, cheap when nothing changed.
var postUpdateHook func(a *App)

func newSelfUpdateCmd(a *App) *cobra.Command {
	var background bool
	cmd := &cobra.Command{
		Use:   "self-update",
		Short: "Update dev now (normally automatic)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if background && !update.AutoEnabled(a.Version) {
				return nil
			}
			exe, err := update.Executable()
			if err != nil {
				return err
			}
			from, to, updated, err := update.New(a.Paths).Update(cmd.Context(), a.Version, exe)
			if background {
				if err != nil {
					a.Log.Logf("background update: %v", err)
				}
				return nil
			}
			if err != nil {
				return err
			}
			_, _ = update.TakeNotice(a.Paths)
			return a.Emit(map[string]any{"from": from, "to": to, "updated": updated}, func() {
				if updated {
					a.Printf("dev updated %s → %s\n", from, to)
				} else {
					a.Printf("already up to date (%s)\n", to)
				}
			})
		},
	}
	cmd.Flags().BoolVar(&background, "background", false, "run as the detached auto-update check")
	_ = cmd.Flags().MarkHidden("background")
	return cmd
}
