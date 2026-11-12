package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/state"
)

// setupResult is the --json shape of `dev setup`.
type setupResult struct {
	Config     state.Config `json:"config"`
	ConfigFile string       `json:"config_file"`
	Skill      struct {
		Dirs []string `json:"dirs"`
	} `json:"skill"`
	Doctor struct {
		OK     bool    `json:"ok"`
		Checks []Check `json:"checks"`
	} `json:"doctor"`
}

// Agents run setup without a terminal: flags + defaults, no prompts, one JSON
// document on stdout (even when doctor checks fail).
func TestSetupNonInteractiveJSON(t *testing.T) {
	h := newHarness(t)
	h.tailnet() // keep doctor's token check off the real API
	code := h.run("setup", "--json", "--machine", "mbp", "--harnesses", "claude,codex", "--cpus", "6", "--memory", "12GiB")
	if code != ExitOK && code != ExitError {
		t.Fatalf("exit %d\n%s", code, h.err.String())
	}
	var res setupResult
	h.decodeJSON(&res) // fails if stdout isn't exactly one JSON document
	if res.Config.Machine != "mbp" || res.Config.CPUs != 6 || res.Config.Memory != "12GiB" || res.Config.Disk != "60GiB" {
		t.Errorf("config = %+v", res.Config)
	}
	if strings.Join(res.Config.Harnesses, ",") != "claude,codex" {
		t.Errorf("harnesses = %v", res.Config.Harnesses)
	}
	if len(res.Doctor.Checks) == 0 {
		t.Error("no doctor checks in the result")
	}
	if (code == ExitOK) != res.Doctor.OK {
		t.Errorf("exit %d but doctor.ok = %v", code, res.Doctor.OK)
	}
	if strings.Contains(h.out.String(), `"error"`) {
		t.Errorf("a second error document was printed:\n%s", h.out.String())
	}

	t.Setenv("DEV_MACHINE", "") // the harness's override would mask the saved prefix
	saved, err := state.LoadConfig(h.paths())
	if err != nil || !saved.Initialized || saved.Machine != "mbp" {
		t.Fatalf("saved config = %+v, %v", saved, err)
	}
	for _, d := range []string{".agents/skills/dev", ".claude/skills/dev"} {
		if _, err := os.Stat(filepath.Join(h.home, d, "SKILL.md")); err != nil {
			t.Errorf("skill not installed in %s: %v", d, err)
		}
	}
}

// Without flags, setup keeps existing config values.
func TestSetupKeepsExistingConfig(t *testing.T) {
	h := newHarness(t)
	h.tailnet()
	c := state.DefaultConfig()
	c.Machine, c.CPUs, c.Editor = "studio", 8, "zed"
	if err := state.SaveConfig(h.paths(), c); err != nil {
		t.Fatal(err)
	}
	h.run("setup", "--json", "--disk", "100GiB")
	var res setupResult
	h.decodeJSON(&res)
	// DEV_MACHINE (set by the harness) overrides the file, as everywhere.
	if res.Config.CPUs != 8 || res.Config.Editor != "zed" || res.Config.Disk != "100GiB" {
		t.Errorf("config = %+v", res.Config)
	}
}

func TestSetupRejectsBadValues(t *testing.T) {
	for _, args := range [][]string{
		{"--machine", "Bad_Name"},
		{"--harnesses", "claude,vim"},
		{"--cpus", "0"},
		{"--memory", "lots"},
		{"--editor", "emacs"},
	} {
		h := newHarness(t)
		h.mustRun(ExitError, append([]string{"setup", "--json"}, args...)...)
		if _, err := os.Stat(h.paths().ConfigFile()); err == nil {
			t.Errorf("%v: config saved despite invalid value", args)
		}
	}
}

// doctor --json prints one document and exits 1 when a check fails.
func TestDoctorJSONSingleDocument(t *testing.T) {
	h := newHarness(t)
	h.tailnet()
	h.fake.Fail("which limactl", 1, "")
	h.mustRun(ExitError, "doctor", "--json")
	var res struct {
		OK     bool    `json:"ok"`
		Checks []Check `json:"checks"`
	}
	h.decodeJSON(&res)
	if res.OK {
		t.Error("ok = true with lima missing")
	}
}

// `init` still works: it was the command's name before v0.1.4 and older
// install scripts call it.
func TestInitAlias(t *testing.T) {
	h := newHarness(t)
	h.tailnet()
	h.run("init", "--json", "--cpus", "3")
	var res setupResult
	h.decodeJSON(&res)
	if res.Config.CPUs != 3 {
		t.Errorf("config = %+v", res.Config)
	}
}
