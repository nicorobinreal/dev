package browser

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/state"
)

func TestFreePort(t *testing.T) {
	old1, old2 := PortMin, PortMax
	defer func() { PortMin, PortMax = old1, old2 }()

	// Find a small window we can control: occupy its first port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	busy := ln.Addr().(*net.TCPAddr).Port
	PortMin, PortMax = busy, busy+3

	p, err := FreePort([]int{busy + 1})
	if err != nil {
		t.Fatal(err)
	}
	if p == busy || p == busy+1 || p < PortMin || p > PortMax {
		t.Errorf("port = %d (busy %d, used %d)", p, busy, busy+1)
	}

	PortMin, PortMax = busy, busy
	if _, err := FreePort(nil); err == nil {
		t.Error("want ErrNoFreePort")
	}
}

func TestUsedPorts(t *testing.T) {
	prs := []state.Project{{Name: "a", BrowserPort: 9300}, {Name: "b"}, {Name: "c", BrowserPort: 9302}}
	got := UsedPorts(prs, "c")
	if len(got) != 1 || got[0] != 9300 {
		t.Errorf("used = %v", got)
	}
}

func TestRegistrationArgs(t *testing.T) {
	cases := map[string][]string{
		"claude mcp add --transport http --scope user chrome-devtools http://localhost:9300/mcp": ClaudeAddArgs(9300),
		"claude mcp remove --scope user chrome-devtools":                                         ClaudeRemoveArgs(),
		"codex mcp add chrome-devtools --url http://localhost:9300/mcp":                          CodexAddArgs(9300),
		"codex mcp remove chrome-devtools":                                                       CodexRemoveArgs(),
	}
	for want, args := range cases {
		if got := strings.Join(args, " "); got != want {
			t.Errorf("got %q want %q", got, want)
		}
	}
	oc := OpenCodeAddArgs(9300)
	if oc[0] != "node" || oc[3] != "add" || oc[4] != OpenCodeConfig || oc[5] != MCPURL(9300) {
		t.Errorf("opencode add = %q", oc)
	}
}

func runNode(t *testing.T, args []string) (string, error) {
	t.Helper()
	out, err := exec.Command(args[0], args[1:]...).CombinedOutput()
	return string(out), err
}

func TestOpenCodeEdit(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not installed")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "sub", "opencode.json")

	// Remove with no file: fine, nothing created.
	if out, err := runNode(t, openCodeArgs("remove", file)); err != nil {
		t.Fatalf("remove missing: %v %s", err, out)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Error("remove created the file")
	}

	// Add to a missing file creates it.
	if out, err := runNode(t, openCodeArgs("add", file, MCPURL(9300))); err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	var cfg map[string]any
	readJSON(t, file, &cfg)
	entry := cfg["mcp"].(map[string]any)["chrome-devtools"].(map[string]any)
	if entry["type"] != "remote" || entry["url"] != "http://localhost:9300/mcp" || entry["enabled"] != true {
		t.Errorf("entry = %v", entry)
	}

	// Add preserves other keys and replaces the URL.
	os.WriteFile(file, []byte(`{"theme":"x","mcp":{"other":{"type":"local","command":["a"]},"chrome-devtools":{"url":"old"}}}`), 0o644)
	if out, err := runNode(t, openCodeArgs("add", file, MCPURL(9311))); err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	cfg = nil
	readJSON(t, file, &cfg)
	mcp := cfg["mcp"].(map[string]any)
	if cfg["theme"] != "x" || mcp["other"] == nil || mcp["chrome-devtools"].(map[string]any)["url"] != MCPURL(9311) {
		t.Errorf("cfg = %v", cfg)
	}

	// Remove keeps the others; removing twice is fine.
	for i := 0; i < 2; i++ {
		if out, err := runNode(t, openCodeArgs("remove", file)); err != nil {
			t.Fatalf("remove: %v %s", err, out)
		}
	}
	cfg = nil
	readJSON(t, file, &cfg)
	mcp = cfg["mcp"].(map[string]any)
	if _, ok := mcp["chrome-devtools"]; ok || mcp["other"] == nil || cfg["theme"] != "x" {
		t.Errorf("after remove cfg = %v", cfg)
	}

	// The only entry removed drops the empty mcp object.
	os.WriteFile(file, []byte(`{"mcp":{"chrome-devtools":{}}}`), 0o644)
	runNode(t, openCodeArgs("remove", file))
	cfg = nil
	readJSON(t, file, &cfg)
	if _, ok := cfg["mcp"]; ok {
		t.Errorf("cfg = %v", cfg)
	}

	// Invalid JSON is left alone.
	os.WriteFile(file, []byte("{ // comment\n}"), 0o644)
	if _, err := runNode(t, openCodeArgs("add", file, MCPURL(1))); err == nil {
		t.Error("want failure on invalid JSON")
	}
	if b, _ := os.ReadFile(file); string(b) != "{ // comment\n}" {
		t.Error("invalid file was rewritten")
	}
}

func readJSON(t *testing.T, file string, v any) {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
}

// fakeBin writes a script that logs its argv to log and exits with code.
func fakeBin(t *testing.T, dir, name, log string, code int) {
	t.Helper()
	script := "#!/bin/sh\necho \"" + name + " $*\" >> " + log + "\nexit " + string(rune('0'+code)) + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runScript(t *testing.T, argv []string, path string) (string, error) {
	t.Helper()
	if argv[0] != "bash" {
		t.Fatalf("argv = %q", argv)
	}
	cmd := exec.Command("/bin/bash", argv[1:]...)
	cmd.Env = []string{"PATH=" + path}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestRegisterScript(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	os.Mkdir(bin, 0o755)
	log := filepath.Join(dir, "log")
	fakeBin(t, bin, "claude", log, 0)
	// codex not installed; node not on PATH either.

	out, err := runScript(t, RegisterScript(9300), bin)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if out != "claude: ok\ncodex: skipped\nopencode: skipped\n" {
		t.Errorf("out = %q", out)
	}
	b, _ := os.ReadFile(log)
	want := "claude mcp remove --scope user chrome-devtools\nclaude mcp add --transport http --scope user chrome-devtools http://localhost:9300/mcp\n"
	if string(b) != want {
		t.Errorf("log = %q", b)
	}

	// A failing add is reported and makes the script fail.
	os.Remove(log)
	fakeBin(t, bin, "codex", log, 1)
	out, err = runScript(t, RegisterScript(9300), bin)
	if err == nil || !strings.Contains(out, "codex: failed") {
		t.Errorf("out = %q err = %v", out, err)
	}

	// Unregister tolerates failing removes (missing entries).
	os.Remove(log)
	fakeBin(t, bin, "claude", log, 1)
	out, err = runScript(t, UnregisterScript(), bin)
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	b, _ = os.ReadFile(log)
	if string(b) != "claude mcp remove --scope user chrome-devtools\ncodex mcp remove chrome-devtools\n" {
		t.Errorf("log = %q", b)
	}
}
