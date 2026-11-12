package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/state"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitError   = 1
	ExitRefused = 2 // refused for safety (e.g. rm with unpushed work)
	ExitLocked  = 3 // lock held by another invocation
)

// RefusedError makes the command exit 2.
type RefusedError struct{ Msg string }

func (e *RefusedError) Error() string { return e.Msg }

// Refuse returns a RefusedError.
func Refuse(format string, args ...any) error {
	return &RefusedError{Msg: fmt.Sprintf(format, args...)}
}

// exitCode maps an error to the process exit code.
func exitCode(err error) int {
	var re *RefusedError
	switch {
	case err == nil:
		return ExitOK
	case errors.As(err, &re):
		return ExitRefused
	case errors.Is(err, state.ErrLockHeld):
		return ExitLocked
	}
	var se *exitStatusError
	if errors.As(err, &se) {
		return se.code
	}
	return ExitError
}

// App is shared by every command. Commands get it from the closure in
// newRootCmd; tests build one with a fake Runner and temp Paths.
type App struct {
	Paths  state.Paths
	Config state.Config
	Runner run.Runner
	// real is the unwrapped runner under --dry-run, for read-only VM scripts.
	real    run.Runner
	Log     *state.Logger
	Out     io.Writer // stdout
	Err     io.Writer // stderr
	JSON    bool
	DryRun  bool
	Verbose bool
	Version string
}

// reader returns a view of the app whose runner really executes, even under
// --dry-run; use only for read-only queries (work check, .devcontainer read).
func (a *App) reader() *App {
	if a.real == nil {
		return a
	}
	c := *a
	c.Runner = a.real
	return &c
}

// Printf writes human output; suppressed in --json mode.
func (a *App) Printf(format string, args ...any) {
	if a.JSON {
		return
	}
	fmt.Fprintf(a.Out, format, args...)
}

// Notef writes progress/notes to stderr (always; harmless for --json parsers).
func (a *App) Notef(format string, args ...any) {
	if a.Log != nil {
		a.Log.Logf("note: "+strings.TrimRight(format, "\n"), args...)
	}
	if a.JSON && !a.Verbose {
		return
	}
	fmt.Fprintf(a.Err, format, args...)
}

// Emit prints v as JSON in --json mode, otherwise calls human().
func (a *App) Emit(v any, human func()) error {
	if a.JSON {
		enc := json.NewEncoder(a.Out)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if human != nil {
		human()
	}
	return nil
}

// LockProject takes the per-project lock.
func (a *App) LockProject(name string) (*state.Lock, error) {
	return state.LockProject(a.Paths, name)
}

// Project loads project state or returns a helpful error.
func (a *App) Project(name string) (state.Project, error) {
	return state.LoadProject(a.Paths, name)
}

// Ctx returns the base context.
func (a *App) Ctx() context.Context { return context.Background() }

// jsonError is what --json prints on failure.
type jsonError struct {
	Error    string `json:"error"`
	ExitCode int    `json:"exit_code"`
}

func main() {
	app := &App{Out: os.Stdout, Err: os.Stderr, Version: version}
	root := newRootCmd(app)
	err := root.Execute()
	code := exitCode(err)
	var se *exitStatusError
	if err != nil && !errors.As(err, &se) {
		if app.JSON {
			_ = json.NewEncoder(os.Stdout).Encode(jsonError{Error: err.Error(), ExitCode: code})
		} else {
			fmt.Fprintln(os.Stderr, "dev:", err)
		}
		if app.Log != nil {
			app.Log.Logf("error (exit %d): %v", code, err)
		}
	}
	app.Log.Close()
	os.Exit(code)
}
