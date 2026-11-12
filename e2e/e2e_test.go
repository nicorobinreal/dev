//go:build e2e

// Package e2e drives a freshly built dev binary against real Lima VMs,
// Tailscale and GitHub on this Mac (see "Testing" in dev.md). Run it with
// `make e2e`; it needs limactl, gh (logged in), the Tailscale OAuth client in
// the Keychain, and access to the private fixture repos.
//
// Environment passed through to dev: DEV_MACHINE (default "e2e"),
// DEV_STARTER_IMAGE (starter image override until it's on ghcr), DEV_HOME.
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	composeRepo = "nicorobinreal/dev-e2e-compose"
	customRepo  = "nicorobinreal/dev-e2e-custom"
	okBody      = "dev-e2e-ok"
)

// devBin is the binary under test, built once in TestMain.
var devBin string

func TestMain(m *testing.M) {
	os.Exit(func() int {
		dir, err := os.MkdirTemp("", "dev-e2e-bin")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		devBin = filepath.Join(dir, "dev")
		build := exec.Command("go", "build", "-o", devBin, "./cmd/dev")
		build.Dir = ".." // module root
		build.Stdout, build.Stderr = os.Stderr, os.Stderr
		if err := build.Run(); err != nil {
			fmt.Fprintln(os.Stderr, "build dev:", err)
			return 1
		}
		return m.Run()
	}())
}

// result is one finished dev invocation.
type result struct {
	code           int
	stdout, stderr string
}

func devEnv() []string {
	env := append(os.Environ(), "DEV_NO_UPDATE=1")
	if os.Getenv("DEV_MACHINE") == "" {
		env = append(env, "DEV_MACHINE=e2e")
	}
	return env
}

// dev runs the binary with a timeout and returns its result; it never fails
// the test by itself.
func dev(t *testing.T, timeout time.Duration, args ...string) result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, devBin, args...)
	cmd.Env = devEnv()
	cmd.Stdin = nil // /dev/null: dev never prompts in these flows
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	// Progress goes to stderr; mirror it to the test log as it happens.
	cmd.Stderr = io.MultiWriter(&errb, testWriter{t})
	t.Logf("$ dev %s", strings.Join(args, " "))
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	} else if err != nil {
		r.code = -1
		r.stderr += "\n" + err.Error()
	}
	if ctx.Err() != nil {
		r.stderr += fmt.Sprintf("\n(timed out after %s)", timeout)
	}
	t.Logf("  exit %d after %s", r.code, time.Since(start).Round(time.Second))
	return r
}

// mustDev runs dev and fails the test unless it exits 0.
func mustDev(t *testing.T, timeout time.Duration, args ...string) result {
	t.Helper()
	r := dev(t, timeout, args...)
	if r.code != 0 {
		t.Fatalf("dev %s: exit %d\nstdout:\n%s\nstderr:\n%s", strings.Join(args, " "), r.code, r.stdout, tail(r.stderr, 4000))
	}
	return r
}

// devJSON runs `dev --json <args>`, requires exit 0 and decodes stdout.
func devJSON(t *testing.T, timeout time.Duration, v any, args ...string) {
	t.Helper()
	r := mustDev(t, timeout, append([]string{"--json"}, args...)...)
	if err := json.Unmarshal([]byte(r.stdout), v); err != nil {
		t.Fatalf("dev %s: decode JSON: %v\n%s", strings.Join(args, " "), err, r.stdout)
	}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	for _, l := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		w.t.Log("  | " + l)
	}
	return len(p), nil
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

// uniqueName returns a project name unique to this run, e.g. "tc1x2y3z".
func uniqueName(prefix string) string {
	return prefix + strconv.FormatInt(time.Now().Unix(), 36)
}

// newProject registers cleanup (always `dev rm --force`, also on failure)
// and runs `dev new`. It returns the decoded --json output.
func newProject(t *testing.T, name, repo string) newOutput {
	t.Helper()
	t.Cleanup(func() {
		if !projectListed(t, name) {
			return
		}
		if r := dev(t, 15*time.Minute, "--json", "rm", "--force", name); r.code != 0 {
			t.Errorf("cleanup: dev rm --force %s: exit %d\n%s\n%s", name, r.code, r.stdout, tail(r.stderr, 2000))
		}
	})
	if img := os.Getenv("DEV_STARTER_IMAGE"); img != "" {
		t.Logf("DEV_STARTER_IMAGE=%s", img)
	}
	var out newOutput
	devJSON(t, 45*time.Minute, &out, "new", name, "--repo", repo, "--no-open")
	t.Logf("new: workspace %s, tailnet %s, devcontainer %v", out.Workspace, out.URL, out.Devcontainer)
	if out.Name != name || out.Workspace == "" {
		t.Fatalf("unexpected new output: %+v", out)
	}
	return out
}

type newOutput struct {
	Name          string         `json:"name"`
	Repo          string         `json:"repo"`
	Workspace     string         `json:"workspace"`
	TailscaleHost string         `json:"tailscale_host"`
	URL           string         `json:"url"`
	SSHHosts      []string       `json:"ssh_hosts"`
	Devcontainer  map[string]any `json:"devcontainer"`
}

type lsOutput struct {
	Projects []struct {
		Name  string `json:"name"`
		State string `json:"state"`
	} `json:"projects"`
}

func listProjects(t *testing.T) lsOutput {
	t.Helper()
	var ls lsOutput
	devJSON(t, 2*time.Minute, &ls, "ls")
	return ls
}

// projectListed reports whether `dev ls` knows name (false on ls errors).
func projectListed(t *testing.T, name string) bool {
	r := dev(t, 2*time.Minute, "--json", "ls")
	var ls lsOutput
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &ls) != nil {
		return true // unknown: let cleanup try anyway
	}
	for _, p := range ls.Projects {
		if p.Name == name {
			return true
		}
	}
	return false
}

func projectState(t *testing.T, name string) string {
	t.Helper()
	for _, p := range listProjects(t).Projects {
		if p.Name == name {
			return p.State
		}
	}
	return ""
}

// eventually retries f until it returns nil or timeout passes.
func eventually(t *testing.T, timeout, every time.Duration, what string, f func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		err := f()
		if err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: still failing after %s: %v", what, timeout, err)
		}
		time.Sleep(every)
	}
}

func httpGet(url string) (string, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return string(b), fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return string(b), nil
}

// TestComposeFlow is the main flow: a private repo without .devcontainer
// (starter written), compose services on localhost, the tailnet URL from the
// Mac, SSH into the container, up/down, and rm accepting a pristine starter.
func TestComposeFlow(t *testing.T) {
	name := uniqueName("tc")
	p := newProject(t, name, composeRepo)

	// 1. Starter written and detected.
	if p.Devcontainer["kind"] != "starter" || p.Devcontainer["written"] != true {
		t.Errorf("devcontainer = %v, want a freshly written starter", p.Devcontainer)
	}

	// The git@ submodule cloned over HTTPS (proves insteadOf + token auth).
	sub := mustDev(t, 2*time.Minute, "exec", name, "--vm", "--", "git", "-C", p.Workspace, "submodule", "status", "--recursive")
	if strings.TrimSpace(sub.stdout) == "" {
		t.Errorf("no submodules reported")
	}
	for _, l := range strings.Split(strings.TrimSpace(sub.stdout), "\n") {
		if strings.HasPrefix(l, "-") {
			t.Errorf("submodule not initialized: %s", l)
		}
	}

	// Compose services, reachable on localhost from the devcontainer.
	mustDev(t, 20*time.Minute, "exec", name, "--", "docker", "compose", "up", "-d")
	eventually(t, 90*time.Second, 3*time.Second, "curl localhost:8080 in the container", func() error {
		r := dev(t, time.Minute, "exec", name, "--", "curl", "-fsS", "http://localhost:8080/")
		if r.code != 0 {
			return fmt.Errorf("exit %d: %s", r.code, tail(r.stderr, 300))
		}
		if !strings.Contains(r.stdout, okBody) {
			return fmt.Errorf("body %q lacks %q", r.stdout, okBody)
		}
		return nil
	})

	// 2. Tailscale URL, from the Mac.
	var u struct {
		URL  string `json:"url"`
		Port int    `json:"port"`
	}
	devJSON(t, time.Minute, &u, "url", name, "8080")
	if u.Port != 8080 || !strings.HasSuffix(u.URL, ":8080") {
		t.Fatalf("url = %+v", u)
	}
	eventually(t, 2*time.Minute, 3*time.Second, "GET "+u.URL+" from the Mac", func() error {
		body, err := httpGet(u.URL)
		if err != nil {
			return err
		}
		if !strings.Contains(body, okBody) {
			return fmt.Errorf("body %q lacks %q", body, okBody)
		}
		return nil
	})

	// 3. SSH straight into the container's sshd.
	sshArgs := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=30"}
	if h := os.Getenv("DEV_HOME"); h != "" {
		sshArgs = append(sshArgs, "-F", filepath.Join(h, ".ssh", "config"))
	}
	sshArgs = append(sshArgs, name+"-dev", "true")
	eventually(t, 90*time.Second, 5*time.Second, "ssh "+name+"-dev true", func() error {
		out, err := exec.Command("ssh", sshArgs...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%v: %s", err, out)
		}
		return nil
	})

	// 4. up (token re-sync, container gh login), then gh works in the container.
	var up struct {
		State string `json:"state"`
	}
	devJSON(t, 20*time.Minute, &up, "up", name, "--no-open")
	if up.State != "running" {
		t.Errorf("up state = %q", up.State)
	}
	mustDev(t, 2*time.Minute, "exec", name, "--", "gh", "auth", "status")

	// 5. down, then rm without --force: an untouched starter is not work.
	mustDev(t, 10*time.Minute, "--json", "down", name)
	if st := projectState(t, name); st != "stopped" {
		t.Errorf("state after down = %q, want stopped", st)
	}
	rm := dev(t, 20*time.Minute, "--json", "rm", name)
	if rm.code != 0 {
		t.Fatalf("dev rm %s: exit %d (2 = refused; a pristine starter must not count as work)\n%s\n%s", name, rm.code, rm.stdout, tail(rm.stderr, 3000))
	}
	if projectListed(t, name) {
		t.Errorf("%s still listed after rm", name)
	}
}

// TestCustomDevcontainer covers case 3: the repo ships its own
// .devcontainer/, which dev leaves alone and reports as custom.
func TestCustomDevcontainer(t *testing.T) {
	name := uniqueName("tx")
	p := newProject(t, name, customRepo)
	if p.Devcontainer["kind"] != "custom" {
		t.Errorf("devcontainer kind = %v, want custom", p.Devcontainer["kind"])
	}
	if p.Devcontainer["written"] == true {
		t.Errorf("dev wrote a starter over a custom .devcontainer: %v", p.Devcontainer)
	}
	if _, ok := p.Devcontainer["requirements"]; !ok {
		t.Errorf("custom devcontainer output lacks requirements: %v", p.Devcontainer)
	}
	mustDev(t, 20*time.Minute, "--json", "rm", "--force", name)
	if projectListed(t, name) {
		t.Errorf("%s still listed after rm --force", name)
	}
}
