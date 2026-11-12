package browser

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/state"
)

// ChildCommand is the MCP server the bridge runs.
var ChildCommand = []string{"npx", "-y", "chrome-devtools-mcp@latest", "--autoConnect"}

// JobPATH is the PATH launchd jobs run with (npx needs node from Homebrew).
const JobPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

// Job is one launchd user agent.
type Job struct {
	Label            string
	ProgramArguments []string
	Env              map[string]string
	LogPath          string
	ThrottleInterval int
}

// BridgeLabel is the launchd label of a project's bridge job.
func BridgeLabel(name string) string { return "dev.browser." + name + ".bridge" }

// TunnelLabel is the launchd label of a project's tunnel job.
func TunnelLabel(name string) string { return "dev.browser." + name + ".tunnel" }

// PlistPath is where the job's plist lives.
func PlistPath(p state.Paths, label string) string {
	return filepath.Join(p.LaunchAgentsDir(), label+".plist")
}

func jobEnv() map[string]string {
	return map[string]string{"PATH": JobPATH, "DEV_NO_UPDATE": "1"}
}

// BridgeJob runs `dev browser bridge --port N -- npx -y chrome-devtools-mcp@latest --autoConnect`.
func BridgeJob(p state.Paths, name, devBinary string, port int) Job {
	args := []string{devBinary, "browser", "bridge", "--port", strconv.Itoa(port), "--"}
	return Job{
		Label:            BridgeLabel(name),
		ProgramArguments: append(args, ChildCommand...),
		Env:              jobEnv(),
		LogPath:          filepath.Join(p.LogsDir(), "browser-"+name+"-bridge.log"),
		ThrottleInterval: 10,
	}
}

// TunnelJob runs the reverse SSH tunnel that makes the bridge
// localhost:<port> inside the VM (and the host-networked container).
func TunnelJob(p state.Paths, name string, port int) Job {
	fwd := fmt.Sprintf("%d:127.0.0.1:%d", port, port)
	return Job{
		Label: TunnelLabel(name),
		ProgramArguments: []string{
			"/usr/bin/ssh", "-N", "-R", fwd,
			"-o", "ServerAliveInterval=15",
			"-o", "ServerAliveCountMax=2",
			"-o", "ExitOnForwardFailure=yes",
			"-o", "BatchMode=yes",
			// Lima's ssh config enables ControlMaster; without these, ssh hands
			// the forward to an existing master and exits 0 immediately, so
			// launchd keeps respawning it and the forward dies with the master.
			"-o", "ControlMaster=no",
			"-o", "ControlPath=none",
			"-F", p.UserSSHConfig(),
			"lima-" + name,
		},
		Env:              jobEnv(),
		LogPath:          filepath.Join(p.LogsDir(), "browser-"+name+"-tunnel.log"),
		ThrottleInterval: 10,
	}
}

// Plist renders the job as a launchd property list.
func (j Job) Plist() []byte {
	var b strings.Builder
	esc := func(s string) string {
		var sb strings.Builder
		_ = xml.EscapeText(&sb, []byte(s))
		return sb.String()
	}
	key := func(k string) { fmt.Fprintf(&b, "\t<key>%s</key>\n", esc(k)) }
	str := func(indent, v string) { fmt.Fprintf(&b, "%s<string>%s</string>\n", indent, esc(v)) }

	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
`)
	key("Label")
	str("\t", j.Label)
	key("ProgramArguments")
	b.WriteString("\t<array>\n")
	for _, a := range j.ProgramArguments {
		str("\t\t", a)
	}
	b.WriteString("\t</array>\n")
	if len(j.Env) > 0 {
		key("EnvironmentVariables")
		b.WriteString("\t<dict>\n")
		ks := make([]string, 0, len(j.Env))
		for k := range j.Env {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		for _, k := range ks {
			fmt.Fprintf(&b, "\t\t<key>%s</key>\n", esc(k))
			str("\t\t", j.Env[k])
		}
		b.WriteString("\t</dict>\n")
	}
	key("RunAtLoad")
	b.WriteString("\t<true/>\n")
	key("KeepAlive")
	b.WriteString("\t<true/>\n")
	if j.ThrottleInterval > 0 {
		key("ThrottleInterval")
		fmt.Fprintf(&b, "\t<integer>%d</integer>\n", j.ThrottleInterval)
	}
	if j.LogPath != "" {
		key("StandardOutPath")
		str("\t", j.LogPath)
		key("StandardErrorPath")
		str("\t", j.LogPath)
	}
	b.WriteString("</dict>\n</plist>\n")
	return []byte(b.String())
}

// JobStatus is what `launchctl print` says about one job.
type JobStatus struct {
	Loaded  bool `json:"loaded"`
	Running bool `json:"running"`
	PID     int  `json:"pid,omitempty"`
}

// Status is a project's browser access state, for `dev browser status` and
// `dev ls`.
type Status struct {
	Port   int       `json:"port,omitempty"`
	Bridge JobStatus `json:"bridge"`
	Tunnel JobStatus `json:"tunnel"`
}

// Granted reports whether browser access is set up (either job loaded).
func (s Status) Granted() bool { return s.Bridge.Loaded || s.Tunnel.Loaded }

// Healthy reports whether both jobs are running.
func (s Status) Healthy() bool { return s.Bridge.Running && s.Tunnel.Running }

// Summary is a short human description: "off", "on (port 9300)",
// "degraded: tunnel not running".
func (s Status) Summary() string {
	switch {
	case !s.Granted():
		return "off"
	case s.Healthy():
		return fmt.Sprintf("on (port %d)", s.Port)
	}
	var down []string
	if !s.Bridge.Running {
		down = append(down, "bridge")
	}
	if !s.Tunnel.Running {
		down = append(down, "tunnel")
	}
	return fmt.Sprintf("degraded (port %d): %s not running", s.Port, strings.Join(down, ", "))
}

// Launchd manages the browser jobs through launchctl.
type Launchd struct {
	R     run.Runner
	Paths state.Paths
	UID   int
	// Sleep is used between bootstrap retries; tests stub it.
	Sleep func(time.Duration)
}

// NewLaunchd uses the current user's GUI domain.
func NewLaunchd(r run.Runner, p state.Paths) *Launchd {
	return &Launchd{R: r, Paths: p, UID: os.Getuid(), Sleep: time.Sleep}
}

func (l *Launchd) domain() string { return fmt.Sprintf("gui/%d", l.UID) }

func (l *Launchd) target(label string) string { return l.domain() + "/" + label }

// Load writes the job's plist and (re)loads it: bootout first, ignoring
// errors, then bootstrap. bootstrap is retried briefly because a bootout
// can still be finishing.
func (l *Launchd) Load(ctx context.Context, j Job) error {
	if j.LogPath != "" {
		if err := os.MkdirAll(filepath.Dir(j.LogPath), 0o755); err != nil {
			return err
		}
	}
	path := PlistPath(l.Paths, j.Label)
	if err := state.WriteFileAtomic(path, j.Plist(), 0o644); err != nil {
		return err
	}
	_, _ = l.R.Run(ctx, run.Cmd{Name: "launchctl", Args: []string{"bootout", l.target(j.Label)}})
	var err error
	for attempt := 0; attempt < 5; attempt++ {
		if attempt > 0 && l.Sleep != nil {
			l.Sleep(300 * time.Millisecond)
		}
		_, err = l.R.Run(ctx, run.Cmd{Name: "launchctl", Args: []string{"bootstrap", l.domain(), path}})
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("load %s: %w", j.Label, err)
}

// Unload boots the job out (if loaded) and deletes its plist.
func (l *Launchd) Unload(ctx context.Context, label string) error {
	_, err := l.R.Run(ctx, run.Cmd{Name: "launchctl", Args: []string{"bootout", l.target(label)}})
	if err != nil {
		st, serr := l.JobStatus(ctx, label)
		if serr != nil || st.Loaded {
			return fmt.Errorf("unload %s: %w", label, err)
		}
	}
	if err := os.Remove(PlistPath(l.Paths, label)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

var (
	stateRE = regexp.MustCompile(`(?m)^\tstate = (\S+)`)
	pidRE   = regexp.MustCompile(`(?m)^\tpid = (\d+)`)
)

// JobStatus runs `launchctl print gui/<uid>/<label>`. A job launchd does not
// know is reported as not loaded, not as an error.
func (l *Launchd) JobStatus(ctx context.Context, label string) (JobStatus, error) {
	res, err := l.R.Run(ctx, run.Cmd{Name: "launchctl", Args: []string{"print", l.target(label)}})
	if err != nil {
		var ee *run.ExitError
		if errors.As(err, &ee) {
			// 113: "Could not find service"; anything else non-zero also
			// means launchctl can't show it.
			return JobStatus{}, nil
		}
		return JobStatus{}, err
	}
	return parsePrint(res.Stdout), nil
}

func parsePrint(out string) JobStatus {
	st := JobStatus{Loaded: true}
	if m := stateRE.FindStringSubmatch(out); m != nil {
		st.Running = m[1] == "running"
	}
	if m := pidRE.FindStringSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(m[1])
	}
	if !st.Running {
		st.PID = 0
	}
	return st
}

// Grant writes and loads both jobs for a project.
func (l *Launchd) Grant(ctx context.Context, name, devBinary string, port int) error {
	if err := l.Load(ctx, BridgeJob(l.Paths, name, devBinary, port)); err != nil {
		return err
	}
	return l.Load(ctx, TunnelJob(l.Paths, name, port))
}

// Revoke unloads and removes both jobs. Missing jobs are fine.
func (l *Launchd) Revoke(ctx context.Context, name string) error {
	return errors.Join(
		l.Unload(ctx, TunnelLabel(name)),
		l.Unload(ctx, BridgeLabel(name)),
	)
}

// Status reports both jobs for a project; port comes from project state.
func (l *Launchd) Status(ctx context.Context, name string, port int) (Status, error) {
	s := Status{Port: port}
	var err1, err2 error
	s.Bridge, err1 = l.JobStatus(ctx, BridgeLabel(name))
	s.Tunnel, err2 = l.JobStatus(ctx, TunnelLabel(name))
	return s, errors.Join(err1, err2)
}
