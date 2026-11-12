package main

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nicorobinreal/dev/internal/devcontainer"
	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/state"
	"github.com/nicorobinreal/dev/internal/tailscale"
)

func TestMain(m *testing.M) {
	// No background auto-update or skill sync from tests.
	preRunHook = nil
	postUpdateHook = nil
	os.Exit(m.Run())
}

const (
	testMachine   = "mac"
	testHome      = "/home/u.linux"
	testWorkspace = testHome + "/shop"
)

// harness is an App wired to a fake runner and a temp home.
type harness struct {
	t    *testing.T
	home string
	fake *run.Fake
	out  bytes.Buffer
	err  bytes.Buffer

	mu      sync.Mutex
	scripts []scriptRule // answers for `limactl shell` calls, matched by substring
	added   bool
}

type scriptRule struct {
	contains string
	result   run.Result
	code     int // non-zero: fail with this exit code
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, home: t.TempDir(), fake: &run.Fake{}}
	t.Setenv("DEV_MACHINE", testMachine)
	t.Setenv("DEV_HOME", h.home)
	t.Setenv(devcontainer.StarterImageEnv, "")
	tsClientCache = nil
	t.Cleanup(func() { tsClientCache = nil })
	// Every `limactl shell` goes through one dispatcher so tests can script
	// scripts by content regardless of instance or workdir.
	h.fake.Rules = append(h.fake.Rules, run.Rule{Prefix: "limactl shell", Func: h.dispatchShell})
	return h
}

func (h *harness) paths() state.Paths { return state.Paths{Home: h.home} }

// onShell answers `limactl shell` calls whose command line contains s.
func (h *harness) onShell(contains, stdout string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scripts = append(h.scripts, scriptRule{contains: contains, result: run.Result{Stdout: stdout}})
}

// failShell makes `limactl shell` calls containing s exit with code.
func (h *harness) failShell(contains string, code int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.scripts = append(h.scripts, scriptRule{contains: contains, code: code, result: run.Result{ExitCode: code, Stderr: "failed"}})
}

func (h *harness) dispatchShell(c run.Cmd) (run.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	line := run.Line(c)
	for _, s := range h.scripts {
		if strings.Contains(line, s.contains) {
			if s.code != 0 {
				return s.result, &run.ExitError{Cmd: c, Result: s.result}
			}
			return s.result, nil
		}
	}
	return run.Result{}, nil
}

// instances scripts `limactl list --json` (one JSON object per line).
func (h *harness) instances(insts ...map[string]any) {
	var b strings.Builder
	for _, i := range insts {
		j, _ := json.Marshal(i)
		b.Write(j)
		b.WriteByte('\n')
	}
	h.fake.On("limactl list --json", b.String())
}

func (h *harness) instance(name, status string) map[string]any {
	return map[string]any{
		"name": name, "status": status, "dir": filepath.Join(h.home, ".lima", name),
		"cpus": 4, "memory": int64(8) << 30, "disk": int64(60) << 30,
	}
}

// saveProject writes a project state file and returns it.
func (h *harness) saveProject(name string) state.Project {
	h.t.Helper()
	p := state.Project{
		Name: name, Repo: "https://github.com/acme/shop", RepoDir: "shop", Workspace: testWorkspace,
		LimaInstance: name, TailscaleHost: state.TailscaleHost(testMachine, name),
		CPUs: 4, Memory: "8GiB", Disk: "60GiB", Editor: "code",
		CreatedAt: time.Now().UTC(), LastUsedAt: time.Now().UTC(),
	}
	if err := state.SaveProject(h.paths(), p); err != nil {
		h.t.Fatal(err)
	}
	return p
}

// run executes `dev <args>` and returns the exit code.
func (h *harness) run(args ...string) int {
	h.t.Helper()
	if !h.added {
		h.added = true
		// Defaults go last so tests' own rules win. No browser jobs loaded.
		h.fake.Fail("launchctl print", 113, "Could not find service")
		h.fake.On("gh auth token", "gho_test")
	}
	h.out.Reset()
	h.err.Reset()
	app := &App{Paths: h.paths(), Runner: h.fake, Out: &h.out, Err: &h.err, Version: "test"}
	root := newRootCmd(app)
	root.SetArgs(args)
	root.SetOut(&h.out)
	root.SetErr(&h.err)
	err := root.Execute()
	app.Log.Close()
	if err != nil {
		fmt.Fprintf(&h.err, "dev: %v\n", err)
	}
	return exitCode(err)
}

// mustRun fails the test unless the command exits with want.
func (h *harness) mustRun(want int, args ...string) {
	h.t.Helper()
	if got := h.run(args...); got != want {
		h.t.Fatalf("dev %s: exit %d, want %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), got, want, h.out.String(), h.err.String())
	}
}

// called reports whether any recorded command line starts with prefix.
func (h *harness) called(prefix string) bool {
	for _, l := range h.fake.Lines() {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

// calls returns the recorded commands whose line starts with prefix.
func (h *harness) calls(prefix string) []run.Cmd {
	var out []run.Cmd
	for _, c := range h.fake.Calls {
		if strings.HasPrefix(run.Line(c), prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (h *harness) decodeJSON(v any) {
	h.t.Helper()
	if err := json.Unmarshal(h.out.Bytes(), v); err != nil {
		h.t.Fatalf("decode --json output: %v\n%s", err, h.out.String())
	}
}

// tailnet points the Tailscale API client at a fake control server holding
// devs. It returns the IDs of deleted devices (read after the command ran).
func (h *harness) tailnet(devs ...tailscale.Device) *[]string {
	h.t.Helper()
	var mu sync.Mutex
	deleted := &[]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v2/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"tok","token_type":"Bearer","expires_in":3600}`))
	})
	mux.HandleFunc("GET /api/v2/tailnet/-/devices", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"devices": devs})
	})
	mux.HandleFunc("DELETE /api/v2/device/{id}", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*deleted = append(*deleted, r.PathValue("id"))
		mu.Unlock()
	})
	srv := httptest.NewServer(mux)
	h.t.Cleanup(srv.Close)
	tsClientCache = &tailscale.Client{
		BaseURL: srv.URL,
		Creds:   tailscale.StaticCredentials(tailscale.Credentials{ClientID: "id", ClientSecret: "secret"}),
	}
	return deleted
}

// workCheckOutput renders gh.WorkCheck's script output.
func workCheckOutput(porcelain string, unpushed []string, stashes int) string {
	enc := base64.StdEncoding.EncodeToString
	return fmt.Sprintf("repo ok\nstatus %s\nunpushed %s\nstashes %d\n",
		enc([]byte(porcelain)), enc([]byte(strings.Join(unpushed, "\x00"))), stashes)
}

// devcontainerTar renders devcontainer.ReadScript's output for files keyed by
// path relative to .devcontainer/.
func devcontainerTar(t *testing.T, files map[string][]byte) string {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	if err := tw.WriteHeader(&tar.Header{Name: devcontainer.Dir + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
		t.Fatal(err)
	}
	for _, n := range names {
		b := files[n]
		if err := tw.WriteHeader(&tar.Header{Name: devcontainer.Dir + "/" + n, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes()) + "\n"
}

// starterFiles returns a copy of the latest starter's files.
func starterFiles() map[string][]byte {
	out := map[string][]byte{}
	for k, v := range devcontainer.Latest().Files {
		out[k] = append([]byte(nil), v...)
	}
	return out
}
