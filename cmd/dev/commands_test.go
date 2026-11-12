package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/state"
	"github.com/nicorobinreal/dev/internal/tailscale"
)

const (
	workCheckMarker = "git status --porcelain"
	readDCMarker    = "tar -cf"
)

func TestExitCodeMapping(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"nil", nil, ExitOK},
		{"plain", errors.New("boom"), ExitError},
		{"refused", Refuse("no"), ExitRefused},
		{"wrapped refused", fmt.Errorf("ctx: %w", Refuse("no")), ExitRefused},
		{"lock held", fmt.Errorf("%w: /x.lock", state.ErrLockHeld), ExitLocked},
		{"child exit", &exitStatusError{code: 42}, 42},
		{"wrapped child exit", fmt.Errorf("x: %w", &exitStatusError{code: 7}), 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.err); got != tc.want {
				t.Errorf("exitCode(%v) = %d, want %d", tc.err, got, tc.want)
			}
		})
	}
}

func TestLockHeldExits3(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	old := state.LockWait
	state.LockWait = 0
	t.Cleanup(func() { state.LockWait = old })
	lk, err := state.LockProject(h.paths(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	defer lk.Unlock()

	h.mustRun(ExitLocked, "down", "shop")
	if h.called("limactl stop") {
		t.Error("stopped the VM without holding the lock")
	}
}

func TestNewValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want int
		msg  string
	}{
		{"bad name", []string{"new", "Bad_Name", "--repo", "acme/shop"}, ExitError, "invalid name"},
		{"trailing dash", []string{"new", "shop-", "--repo", "acme/shop"}, ExitError, "must not end"},
		{"missing repo", []string{"new", "shop"}, ExitError, "--repo is required"},
		{"not github", []string{"new", "shop", "--repo", "https://gitlab.com/acme/shop"}, ExitError, "only github.com"},
		{"bad editor", []string{"new", "shop", "--repo", "acme/shop", "--editor", "vim"}, ExitError, "unknown editor"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			h.mustRun(tc.want, tc.args...)
			if !strings.Contains(h.err.String(), tc.msg) {
				t.Errorf("stderr %q does not mention %q", h.err.String(), tc.msg)
			}
			if h.called("limactl create") {
				t.Error("created a VM despite invalid input")
			}
		})
	}
}

func TestNewRefusesExistingProject(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.mustRun(ExitRefused, "new", "shop", "--repo", "acme/shop")
	if !strings.Contains(h.err.String(), "already exists") {
		t.Errorf("stderr: %s", h.err.String())
	}
	if h.called("limactl create") {
		t.Error("created a VM for an existing project")
	}
}

func TestNewRefusesUnmanagedLimaInstance(t *testing.T) {
	h := newHarness(t)
	h.instances(h.instance("shop", "Stopped"))
	h.mustRun(ExitRefused, "new", "shop", "--repo", "acme/shop")
	if !strings.Contains(h.err.String(), "isn't managed by dev") {
		t.Errorf("stderr: %s", h.err.String())
	}
}

func TestNewRefusesExistingTailnetNode(t *testing.T) {
	h := newHarness(t)
	h.tailnet(tailscale.Device{NodeID: "n1", Name: "mac-shop.tailnet.ts.net", Hostname: "mac-shop"})
	h.mustRun(ExitRefused, "new", "shop", "--repo", "acme/shop")
	if !strings.Contains(h.err.String(), "tailnet node named mac-shop") {
		t.Errorf("stderr: %s", h.err.String())
	}
	if h.called("limactl create") {
		t.Error("created a VM despite a tailnet name clash")
	}
}

func TestURL(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")

	h.mustRun(ExitOK, "url", "shop")
	if got := h.out.String(); got != "http://mac-shop\n" {
		t.Errorf("url = %q", got)
	}

	h.mustRun(ExitOK, "url", "shop", "8080", "--json")
	var out struct {
		Name, Host, URL string
		Port            int
	}
	h.decodeJSON(&out)
	if out.Name != "shop" || out.Host != "mac-shop" || out.Port != 8080 || out.URL != "http://mac-shop:8080" {
		t.Errorf("url --json = %+v", out)
	}

	h.mustRun(ExitError, "url", "shop", "99999")
	h.mustRun(ExitError, "url", "nope")
}

func TestLs(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	api := h.saveProject("api")
	api.Branch = "develop"
	if err := state.SaveProject(h.paths(), api); err != nil {
		t.Fatal(err)
	}
	h.instances(h.instance("shop", "Running"), h.instance("other", "Stopped")) // api's VM is gone
	h.onShell("branch --show-current", "feature-x\n")
	h.onShell("/proc/loadavg", "cpus 4\nload 0.50 0.40 0.30\nmem 8589934592 2147483648\ndisk 64424509440 10737418240\n")

	h.mustRun(ExitOK, "ls", "--json")
	var out struct {
		Projects []lsRow `json:"projects"`
	}
	h.decodeJSON(&out)
	if len(out.Projects) != 2 {
		t.Fatalf("got %d rows, want 2 (unmanaged instances are not listed): %+v", len(out.Projects), out.Projects)
	}
	a, s := out.Projects[0], out.Projects[1] // sorted by name
	if a.Name != "api" || a.State != "missing" || a.Branch != "develop" || a.Usage != nil {
		t.Errorf("api row = %+v", a)
	}
	if s.Name != "shop" || s.State != "running" || s.Branch != "feature-x" || s.TailscaleHost != "mac-shop" {
		t.Errorf("shop row = %+v", s)
	}
	if s.Usage == nil || s.Usage.MemUsed != 2<<30 || s.Memory != 8<<30 {
		t.Errorf("shop usage = %+v, memory %d", s.Usage, s.Memory)
	}
	if s.Browser.Granted() {
		t.Errorf("browser should be off: %+v", s.Browser)
	}

	h.mustRun(ExitOK, "ls")
	txt := h.out.String()
	for _, want := range []string{"NAME", "STATE", "shop", "running", "feature-x", "2.0G/8.0G", "api", "missing", "off"} {
		if !strings.Contains(txt, want) {
			t.Errorf("ls output lacks %q:\n%s", want, txt)
		}
	}
}

func TestLsEmpty(t *testing.T) {
	h := newHarness(t)
	h.mustRun(ExitOK, "ls")
	if !strings.Contains(h.out.String(), "no projects") {
		t.Errorf("ls = %q", h.out.String())
	}
	h.mustRun(ExitOK, "ls", "--json")
	if got := strings.Join(strings.Fields(h.out.String()), ""); got != `{"projects":[]}` {
		t.Errorf("ls --json = %s", got)
	}
}

func TestRmRefusesModifiedFiles(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	h.onShell(workCheckMarker, workCheckOutput(" M main.go\x00?? notes.txt\x00", []string{"abc123 wip"}, 1))
	deleted := h.tailnet()

	h.mustRun(ExitRefused, "rm", "shop")
	msg := h.err.String()
	for _, want := range []string{"1 modified (main.go)", "1 untracked (notes.txt)", "1 unpushed", "1 stash", "--force"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q lacks %q", msg, want)
		}
	}
	if h.called("limactl delete") || len(*deleted) > 0 {
		t.Error("deleted the VM or node despite the refusal")
	}
	if _, err := state.LoadProject(h.paths(), "shop"); err != nil {
		t.Errorf("project state removed: %v", err)
	}
}

func TestRmAllowsPristineStarter(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	h.onShell(workCheckMarker, workCheckOutput("?? .devcontainer/\x00", nil, 0))
	h.onShell(readDCMarker, devcontainerTar(t, starterFiles()))
	deleted := h.tailnet(tailscale.Device{NodeID: "n1", Name: "mac-shop.tailnet.ts.net", Hostname: "mac-shop"})

	h.mustRun(ExitOK, "rm", "shop", "--json")
	if !h.called("limactl delete --tty=false --force shop") {
		t.Errorf("VM not deleted; calls:\n%s", strings.Join(h.fake.Lines(), "\n"))
	}
	if len(*deleted) != 1 || (*deleted)[0] != "n1" {
		t.Errorf("tailnet deletes = %v, want [n1]", *deleted)
	}
	if _, err := state.LoadProject(h.paths(), "shop"); !errors.Is(err, state.ErrNoProject) {
		t.Errorf("project state still there: %v", err)
	}
	var out map[string]any
	h.decodeJSON(&out)
	if out["removed"] != true {
		t.Errorf("rm --json = %v", out)
	}
}

func TestRmRefusesEditedStarter(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	files := starterFiles()
	files["devcontainer.json"] = append(files["devcontainer.json"], []byte("\n// edited\n")...)
	h.onShell(workCheckMarker, workCheckOutput("?? .devcontainer/\x00", nil, 0))
	h.onShell(readDCMarker, devcontainerTar(t, files))
	h.tailnet()

	h.mustRun(ExitRefused, "rm", "shop")
	if !strings.Contains(h.err.String(), ".devcontainer/") {
		t.Errorf("refusal should name .devcontainer/: %s", h.err.String())
	}
}

func TestRmForceSkipsWorkCheck(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Stopped"))
	h.tailnet()

	h.mustRun(ExitOK, "rm", "shop", "--force")
	if h.called("limactl start") {
		t.Error("--force should not start the VM for a work check")
	}
	if !h.called("limactl delete --tty=false --force shop") {
		t.Error("VM not deleted")
	}
}

func TestExec(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))

	h.mustRun(ExitOK, "exec", "shop", "--", "docker", "compose", "up", "-d")
	want := "limactl shell --workdir " + testWorkspace + " shop -- devcontainer exec --workspace-folder " + testWorkspace + " -- docker compose up -d"
	cs := h.calls("limactl shell --workdir " + testWorkspace)
	if len(cs) != 1 || run.Line(cs[0]) != want {
		t.Fatalf("exec calls = %v, want %q", h.fake.Lines(), want)
	}
	if !cs[0].Interactive {
		t.Error("exec should be interactive")
	}

	h.fake.Calls = nil
	h.mustRun(ExitOK, "exec", "shop", "--vm", "--", "cat", "/etc/os-release")
	want = "limactl shell --workdir " + testWorkspace + " shop -- cat /etc/os-release"
	if cs := h.calls("limactl shell --workdir " + testWorkspace); len(cs) != 1 || run.Line(cs[0]) != want {
		t.Fatalf("exec --vm calls = %v, want %q", h.fake.Lines(), want)
	}
}

func TestExecPropagatesExitCode(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	h.failShell("-- false", 7)
	h.mustRun(7, "exec", "shop", "--vm", "--", "false")
}

func TestExecRequiresRunningVM(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Stopped"))
	h.mustRun(ExitError, "exec", "shop", "--", "true")
	if !strings.Contains(h.err.String(), "dev up shop") {
		t.Errorf("stderr: %s", h.err.String())
	}
}

func TestDownStopsRunningVM(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))

	h.mustRun(ExitOK, "down", "shop", "--json")
	if !h.called("limactl stop --tty=false shop") {
		t.Errorf("VM not stopped; calls:\n%s", strings.Join(h.fake.Lines(), "\n"))
	}
	uid := os.Getuid()
	for _, job := range []string{"tunnel", "bridge"} {
		if !h.called(fmt.Sprintf("launchctl bootout gui/%d/dev.browser.shop.%s", uid, job)) {
			t.Errorf("browser %s job not booted out", job)
		}
	}
	var out map[string]any
	h.decodeJSON(&out)
	if out["state"] != "stopped" {
		t.Errorf("down --json = %v", out)
	}
}

func TestDownStoppedVMIsNoop(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Stopped"))
	h.mustRun(ExitOK, "down", "shop")
	if h.called("limactl stop") {
		t.Error("stopped an already stopped VM")
	}
}

func TestDryRunDoesNotMutate(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))

	h.mustRun(ExitOK, "down", "shop", "--dry-run")
	if h.called("limactl stop") || h.called("launchctl bootout") {
		t.Errorf("--dry-run ran mutating commands:\n%s", strings.Join(h.fake.Lines(), "\n"))
	}
	if !h.called("limactl list") {
		t.Error("--dry-run should still run read-only queries")
	}
	if !strings.Contains(h.err.String(), "[dry-run] limactl stop --tty=false shop") {
		t.Errorf("stderr lacks the dry-run line:\n%s", h.err.String())
	}
}

func TestDryRunRmForce(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	deleted := h.tailnet(tailscale.Device{NodeID: "n1", Name: "mac-shop.tailnet.ts.net", Hostname: "mac-shop"})

	h.mustRun(ExitOK, "rm", "shop", "--force", "--dry-run")
	if h.called("limactl delete") || len(*deleted) > 0 {
		t.Error("--dry-run deleted something")
	}
	if _, err := state.LoadProject(h.paths(), "shop"); err != nil {
		t.Errorf("--dry-run removed project state: %v", err)
	}
	if _, err := os.Stat(h.paths().SSHConfigFile()); err == nil {
		t.Error("--dry-run wrote the SSH config")
	}
}

// rm --dry-run (without --force) should still run the read-only work check
// and report what it would do.
func TestDryRunRmChecksWork(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	h.onShell(workCheckMarker, workCheckOutput("", nil, 0))
	h.tailnet()
	h.mustRun(ExitOK, "rm", "shop", "--dry-run")
}

func TestSSHConfig(t *testing.T) {
	h := newHarness(t)
	h.saveProject("shop")
	h.instances(h.instance("shop", "Running"))
	limaCfg := filepath.Join(h.home, ".lima", "shop", "ssh.config")
	if err := os.MkdirAll(filepath.Dir(limaCfg), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(limaCfg, []byte("Host lima-shop\n  User u\n  Hostname 127.0.0.1\n  Port 60022\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	h.mustRun(ExitOK, "ssh-config", "--json")
	var out struct{ File string }
	h.decodeJSON(&out)
	if want := filepath.Join(h.home, ".config", "dev", "ssh_config"); out.File != want {
		t.Errorf("file = %q, want %q", out.File, want)
	}
	b, err := os.ReadFile(out.File)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Host lima-shop", "Port 60022", "Host shop-dev", "ProxyJump lima-shop", "Port 2222"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("ssh_config lacks %q:\n%s", want, b)
		}
	}
	inc, err := os.ReadFile(filepath.Join(h.home, ".ssh", "config"))
	if err != nil || !strings.Contains(string(inc), "Include") || !strings.Contains(string(inc), "ssh_config") {
		t.Errorf("~/.ssh/config Include missing (%v):\n%s", err, inc)
	}

	// Idempotent: a second run keeps a single Include line.
	h.mustRun(ExitOK, "ssh-config")
	inc, _ = os.ReadFile(filepath.Join(h.home, ".ssh", "config"))
	if n := strings.Count(string(inc), "\nInclude "); n != 1 {
		t.Errorf("%d Include lines after two runs:\n%s", n, inc)
	}
}
