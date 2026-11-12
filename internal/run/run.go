// Package run puts every external command (limactl, tailscale, gh, ssh,
// devcontainer, docker, security, launchctl) behind one interface so tests can
// swap in a fake that records commands and returns scripted output.
package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Cmd describes one external command invocation.
type Cmd struct {
	Name  string
	Args  []string
	Stdin io.Reader
	Env   []string // extra KEY=VALUE pairs appended to os.Environ()
	Dir   string
	// Interactive connects the process to the real stdin/stdout/stderr
	// (used by `dev exec`). Result.Stdout/Stderr stay empty.
	Interactive bool
}

func (c Cmd) String() string {
	parts := append([]string{c.Name}, c.Args...)
	for i, p := range parts {
		if strings.ContainsAny(p, " \t\n'\"$") {
			parts[i] = fmt.Sprintf("%q", p)
		}
	}
	return strings.Join(parts, " ")
}

// Result is the outcome of a finished command.
type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	Duration time.Duration
}

// ExitError is returned when a command ran but exited non-zero.
type ExitError struct {
	Cmd    Cmd
	Result Result
}

func (e *ExitError) Error() string {
	msg := strings.TrimSpace(e.Result.Stderr)
	if msg == "" {
		msg = strings.TrimSpace(e.Result.Stdout)
	}
	if len(msg) > 2000 {
		msg = msg[len(msg)-2000:]
	}
	return fmt.Sprintf("%s: exit %d: %s", e.Cmd.Name, e.Result.ExitCode, msg)
}

// Runner runs external commands.
type Runner interface {
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Logger receives a line per command run; state.Logger satisfies it.
type Logger interface {
	Logf(format string, args ...any)
}

// Exec is the real Runner.
type Exec struct {
	Log Logger
}

func (e *Exec) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if len(c.Env) > 0 {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	var stdout, stderr bytes.Buffer
	if c.Interactive {
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		if c.Stdin != nil {
			cmd.Stdin = c.Stdin
		}
	} else {
		cmd.Stdin = c.Stdin
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
	}
	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(start)}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		res.ExitCode = ee.ExitCode()
		err = &ExitError{Cmd: c, Result: res}
	case err != nil:
		res.ExitCode = -1
	}
	if e.Log != nil {
		status := "ok"
		if err != nil {
			status = "error: " + firstLine(err.Error())
		}
		e.Log.Logf("exec %s (%s) %s", c.String(), res.Duration.Round(time.Millisecond), status)
	}
	return res, err
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// Output runs c and returns trimmed stdout.
func Output(ctx context.Context, r Runner, name string, args ...string) (string, error) {
	res, err := r.Run(ctx, Cmd{Name: name, Args: args})
	return strings.TrimSpace(res.Stdout), err
}

// Fake is a Runner for tests. It records every command and answers from
// Responses: the first rule whose Match returns true wins. Unmatched commands
// succeed with empty output.
type Fake struct {
	mu        sync.Mutex
	Calls     []Cmd
	StdinSeen []string
	Rules     []Rule
}

// Rule scripts a response for matching commands.
type Rule struct {
	// Prefix matches when the command line (name + args joined by spaces)
	// starts with it.
	Prefix string
	Result Result
	Err    error
	// Func, if set, computes the response instead of Result/Err.
	Func func(c Cmd) (Result, error)
}

// On adds a rule and returns the fake for chaining.
func (f *Fake) On(prefix, stdout string) *Fake {
	f.Rules = append(f.Rules, Rule{Prefix: prefix, Result: Result{Stdout: stdout}})
	return f
}

// Fail adds a rule that exits non-zero.
func (f *Fake) Fail(prefix string, code int, stderr string) *Fake {
	f.Rules = append(f.Rules, Rule{Prefix: prefix, Func: func(c Cmd) (Result, error) {
		r := Result{Stderr: stderr, ExitCode: code}
		return r, &ExitError{Cmd: c, Result: r}
	}})
	return f
}

func (f *Fake) Run(_ context.Context, c Cmd) (Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, c)
	in := ""
	if c.Stdin != nil {
		b, _ := io.ReadAll(c.Stdin)
		in = string(b)
	}
	f.StdinSeen = append(f.StdinSeen, in)
	line := Line(c)
	for _, r := range f.Rules {
		if strings.HasPrefix(line, r.Prefix) {
			if r.Func != nil {
				return r.Func(c)
			}
			return r.Result, r.Err
		}
	}
	return Result{}, nil
}

// Lines returns every recorded command as a single line.
func (f *Fake) Lines() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.Calls))
	for i, c := range f.Calls {
		out[i] = Line(c)
	}
	return out
}

// Line joins name and args with spaces (no quoting), for matching.
func Line(c Cmd) string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}
