package browser

import (
	"context"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/state"
)

var update = flag.Bool("update", false, "rewrite golden files")

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update)", err)
	}
	if string(got) != string(want) {
		t.Errorf("%s mismatch:\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func lint(t *testing.T, plist []byte) {
	t.Helper()
	if _, err := exec.LookPath("plutil"); err != nil {
		return
	}
	f := filepath.Join(t.TempDir(), "x.plist")
	os.WriteFile(f, plist, 0o644)
	if out, err := exec.Command("plutil", "-lint", f).CombinedOutput(); err != nil {
		t.Errorf("plutil -lint: %v: %s", err, out)
	}
}

var testPaths = state.Paths{Home: "/Users/alice"}

func TestBridgePlistGolden(t *testing.T) {
	j := BridgeJob(testPaths, "shop", "mbp-shop", "/Users/alice/.local/bin/dev", 9301)
	golden(t, "bridge.plist", j.Plist())
	lint(t, j.Plist())
}

func TestTunnelPlistGolden(t *testing.T) {
	j := TunnelJob(testPaths, "shop", 9301)
	golden(t, "tunnel.plist", j.Plist())
	lint(t, j.Plist())
}

func TestPlistEscaping(t *testing.T) {
	j := Job{
		Label:            "a&b",
		ProgramArguments: []string{`/path with <odd> & "chars"`},
		Env:              map[string]string{"X": "1<2"},
	}
	golden(t, "escaped.plist", j.Plist())
	lint(t, j.Plist())
}

func newTestLaunchd(t *testing.T, f *run.Fake) (*Launchd, state.Paths) {
	p := state.Paths{Home: t.TempDir()}
	return &Launchd{R: f, Paths: p, UID: 501, Sleep: func(time.Duration) {}}, p
}

func TestGrantLoadsBothJobs(t *testing.T) {
	f := (&run.Fake{}).Fail("launchctl bootout", 3, "Boot-out failed: 3: No such process")
	l, p := newTestLaunchd(t, f)
	if err := l.Grant(context.Background(), "shop", "mbp-shop", "/bin/dev", 9300); err != nil {
		t.Fatal(err)
	}
	bp := PlistPath(p, "dev.browser.shop.bridge")
	tp := PlistPath(p, "dev.browser.shop.tunnel")
	want := []string{
		"launchctl bootout gui/501/dev.browser.shop.bridge",
		"launchctl bootstrap gui/501 " + bp,
		"launchctl bootout gui/501/dev.browser.shop.tunnel",
		"launchctl bootstrap gui/501 " + tp,
	}
	if got := f.Lines(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for _, path := range []string{bp, tp} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(b), "<string>9300</string>") && !strings.Contains(string(b), "9300:127.0.0.1:9300") {
			t.Errorf("%s lacks port", path)
		}
	}
	if _, err := os.Stat(p.LogsDir()); err != nil {
		t.Errorf("log dir not created: %v", err)
	}
}

func TestLoadRetriesBootstrap(t *testing.T) {
	n := 0
	f := &run.Fake{}
	f.Rules = append(f.Rules, run.Rule{Prefix: "launchctl bootstrap", Func: func(c run.Cmd) (run.Result, error) {
		n++
		if n < 3 {
			r := run.Result{ExitCode: 5, Stderr: "Bootstrap failed: 5: Input/output error"}
			return r, &run.ExitError{Cmd: c, Result: r}
		}
		return run.Result{}, nil
	}})
	l, p := newTestLaunchd(t, f)
	if err := l.Load(context.Background(), TunnelJob(p, "x", 9300)); err != nil {
		t.Fatal(err)
	}
	if n != 3 {
		t.Errorf("bootstrap attempts = %d", n)
	}

	f2 := (&run.Fake{}).Fail("launchctl bootstrap", 5, "nope")
	l2, p2 := newTestLaunchd(t, f2)
	if err := l2.Load(context.Background(), TunnelJob(p2, "x", 9300)); err == nil {
		t.Error("want error after retries")
	}
}

func TestRevoke(t *testing.T) {
	f := (&run.Fake{}).
		Fail("launchctl bootout gui/501/dev.browser.shop.tunnel", 3, "No such process").
		Fail("launchctl print", 113, `Could not find service "x" in domain for user gui: 501`)
	l, p := newTestLaunchd(t, f)
	for _, lbl := range []string{BridgeLabel("shop"), TunnelLabel("shop")} {
		os.MkdirAll(p.LaunchAgentsDir(), 0o755)
		os.WriteFile(PlistPath(p, lbl), []byte("x"), 0o644)
	}
	if err := l.Revoke(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	for _, lbl := range []string{BridgeLabel("shop"), TunnelLabel("shop")} {
		if _, err := os.Stat(PlistPath(p, lbl)); !os.IsNotExist(err) {
			t.Errorf("%s plist still there", lbl)
		}
	}
	// Revoking again (nothing loaded, no plists) is fine.
	f.Fail("launchctl bootout", 3, "No such process")
	if err := l.Revoke(context.Background(), "shop"); err != nil {
		t.Errorf("second revoke: %v", err)
	}
}

func TestUnloadFailsWhenStillLoaded(t *testing.T) {
	f := (&run.Fake{}).
		Fail("launchctl bootout", 1, "Operation not permitted").
		On("launchctl print", "gui/501/x = {\n\tstate = running\n\tpid = 7\n}\n")
	l, _ := newTestLaunchd(t, f)
	if err := l.Unload(context.Background(), "x"); err == nil {
		t.Error("want error")
	}
}

const printRunning = `gui/501/dev.browser.shop.bridge = {
	active count = 1
	path = /Users/alice/Library/LaunchAgents/dev.browser.shop.bridge.plist
	type = LaunchAgent
	state = running

	program = /Users/alice/.local/bin/dev
	arguments = {
		/Users/alice/.local/bin/dev
		browser
	}

	endpoints = {
		"x" = {
			state = waiting
			pid = 1
		}
	}
	runs = 3
	pid = 4242
	last exit code = 1
}
`

const printWaiting = `gui/501/dev.browser.shop.tunnel = {
	active count = 0
	state = spawn scheduled
	runs = 12
	last exit code = 255
}
`

func TestStatus(t *testing.T) {
	f := (&run.Fake{}).
		On("launchctl print gui/501/dev.browser.shop.bridge", printRunning).
		On("launchctl print gui/501/dev.browser.shop.tunnel", printWaiting).
		Fail("launchctl print", 113, "Could not find service")
	l, _ := newTestLaunchd(t, f)
	s, err := l.Status(context.Background(), "shop", 9300)
	if err != nil {
		t.Fatal(err)
	}
	want := Status{Port: 9300, Bridge: JobStatus{Loaded: true, Running: true, PID: 4242}, Tunnel: JobStatus{Loaded: true}}
	if s != want {
		t.Errorf("status = %+v, want %+v", s, want)
	}
	if !s.Granted() || s.Healthy() {
		t.Error("granted/healthy")
	}
	if got := s.Summary(); got != "degraded (port 9300): tunnel not running" {
		t.Errorf("summary = %q", got)
	}

	s, err = l.Status(context.Background(), "other", 0)
	if err != nil {
		t.Fatal(err)
	}
	if s.Granted() || s.Summary() != "off" {
		t.Errorf("other = %+v", s)
	}
	healthy := Status{Port: 1, Bridge: JobStatus{true, true, 1}, Tunnel: JobStatus{true, true, 2}}
	if healthy.Summary() != "on (port 1)" {
		t.Error(healthy.Summary())
	}
}

func TestNewLaunchdUsesRealUID(t *testing.T) {
	l := NewLaunchd(&run.Fake{}, testPaths)
	if l.UID != os.Getuid() {
		t.Errorf("uid = %d", l.UID)
	}
}
