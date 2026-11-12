package skill

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/devcontainer"
	"github.com/nicorobinreal/dev/internal/state"
)

func paths(t *testing.T) state.Paths { return state.Paths{Home: t.TempDir()} }

func TestHarnessTable(t *testing.T) {
	want := []Harness{
		{state.HarnessClaude, "Claude Code", ".claude/skills", false},
		{state.HarnessCodex, "Codex", ".agents/skills", true},
		{state.HarnessOpenCode, "OpenCode", ".config/opencode/skills", true},
	}
	if !reflect.DeepEqual(Harnesses, want) {
		t.Errorf("Harnesses = %+v", Harnesses)
	}
	for _, id := range state.DefaultConfig().Harnesses {
		if _, ok := Lookup(id); !ok {
			t.Errorf("default harness %q missing from table", id)
		}
	}
}

func TestSkillFrontmatterAndContent(t *testing.T) {
	md := string(Files()["SKILL.md"])
	if !strings.HasPrefix(md, "---\nname: dev\ndescription: ") {
		t.Fatalf("SKILL.md must start with YAML frontmatter (name: dev, description: ...)")
	}
	end := strings.Index(md[4:], "\n---\n")
	if end < 0 {
		t.Fatal("unterminated frontmatter")
	}
	for _, want := range []string{
		"--json", "--force", `{"error": `, `"exit_code"`,
		"dev new <name> --repo <url>", "dev up <name>", "dev down <name>", "dev rebuild <name>",
		"dev rm <name>", "dev ls", "dev open <name>", "dev exec <name> -- <cmd...>", "dev url <name> [port]",
		"dev serve <name> <port>", "dev browser grant <name>", "dev browser revoke <name>", "dev browser status <name>",
		"dev ssh-config", "dev upgrade <name>", "dev doctor", "dev prune", "dev self-update", "dev self-uninstall",
		"--branch", "--cpus", "--memory", "--disk", "--no-open", "--editor", "--no-cache", "--vm", "--off",
		"--all", "--fix", "--older-than", "--purge", "--dry-run", "--verbose",
		"dev exec <name> --vm",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("SKILL.md does not mention %q", want)
		}
	}
	for _, r := range devcontainer.Requirements {
		if !strings.Contains(md, "**"+r.ID+"**") {
			t.Errorf("SKILL.md misses starter requirement %q (keep it in sync with devcontainer.Requirements)", r.ID)
		}
	}
	for code := 0; code <= 3; code++ {
		if !strings.Contains(md, "| "+string(rune('0'+code))+" |") {
			t.Errorf("SKILL.md exit code table misses %d", code)
		}
	}
}

func readSkillDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	got := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		rel, _ := filepath.Rel(dir, p)
		got[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestInstallFresh(t *testing.T) {
	p := paths(t)
	res, err := Install(p, []string{state.HarnessClaude, state.HarnessCodex, state.HarnessOpenCode})
	if err != nil {
		t.Fatal(err)
	}
	shared := filepath.Join(p.Home, ".agents", "skills", "dev")
	claude := filepath.Join(p.Home, ".claude", "skills", "dev")
	if !reflect.DeepEqual(res.Dirs, []string{shared, claude}) && !reflect.DeepEqual(res.Dirs, []string{claude, shared}) {
		t.Errorf("Dirs = %v", res.Dirs)
	}
	for _, d := range []string{shared, claude} {
		if !reflect.DeepEqual(readSkillDir(t, d), Files()) {
			t.Errorf("%s does not hold the skill", d)
		}
	}
	if _, err := os.Stat(filepath.Join(p.Home, ".config", "opencode", "skills", "dev")); !os.IsNotExist(err) {
		t.Error("OpenCode reads the shared folder; its own folder must not be written")
	}
	if !res.Changed || res.PreviousHash != "" || res.Hash != Hash() {
		t.Errorf("result = %+v", res)
	}
	if want := []string{"Claude Code", "Codex", "OpenCode"}; !reflect.DeepEqual(res.Reload, want) {
		t.Errorf("Reload = %v, want %v", res.Reload, want)
	}
	b, _ := os.ReadFile(HashFile(p))
	if strings.TrimSpace(string(b)) != Hash() {
		t.Errorf("stored hash = %q", b)
	}
	if n := res.Notices(); len(n) != 3 || n[0] != "skill updated — reload skills in Claude Code" {
		t.Errorf("Notices = %q", n)
	}
}

func TestInstallUnchangedIsQuiet(t *testing.T) {
	p := paths(t)
	all := []string{state.HarnessClaude, state.HarnessCodex, state.HarnessOpenCode}
	if _, err := Install(p, all); err != nil {
		t.Fatal(err)
	}
	md := filepath.Join(SharedSkillDir(p), "SKILL.md")
	st1, _ := os.Stat(md)
	res, err := Install(p, all)
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || len(res.Reload) != 0 {
		t.Errorf("second install: %+v", res)
	}
	st2, _ := os.Stat(md)
	if !os.SameFile(st1, st2) {
		t.Error("unchanged file was rewritten")
	}
}

func TestInstallChangedSkill(t *testing.T) {
	p := paths(t)
	all := []string{state.HarnessClaude, state.HarnessCodex, state.HarnessOpenCode}
	if _, err := Install(p, all); err != nil {
		t.Fatal(err)
	}
	// Simulate the previous version: different stored hash, old content and
	// a stale reference file.
	if err := os.WriteFile(HashFile(p), []byte("oldhash\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{SharedSkillDir(p), filepath.Join(p.Home, ".claude", "skills", "dev")} {
		os.WriteFile(filepath.Join(d, "SKILL.md"), []byte("old"), 0o644)
		os.MkdirAll(filepath.Join(d, "references"), 0o755)
		os.WriteFile(filepath.Join(d, "references", "old.md"), []byte("old"), 0o644)
	}
	res, err := Install(p, all)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed || res.PreviousHash != "oldhash" {
		t.Errorf("result = %+v", res)
	}
	// Claude Code watches existing skill files: no notice.
	if want := []string{"Codex", "OpenCode"}; !reflect.DeepEqual(res.Reload, want) {
		t.Errorf("Reload = %v, want %v", res.Reload, want)
	}
	for _, d := range res.Dirs {
		if !reflect.DeepEqual(readSkillDir(t, d), Files()) {
			t.Errorf("%s not updated / stale files left", d)
		}
		if _, err := os.Stat(filepath.Join(d, "references")); !os.IsNotExist(err) {
			t.Errorf("empty stale folder left in %s", d)
		}
	}
}

func TestInstallNewClaudeFolderNeedsReload(t *testing.T) {
	p := paths(t)
	if _, err := Install(p, []string{state.HarnessCodex}); err != nil {
		t.Fatal(err)
	}
	// Claude selected later (dev init): its folder is new, hash unchanged.
	res, err := Install(p, []string{state.HarnessCodex, state.HarnessClaude})
	if err != nil {
		t.Fatal(err)
	}
	if res.Changed || !reflect.DeepEqual(res.Reload, []string{"Claude Code"}) {
		t.Errorf("result = %+v", res)
	}
}

func TestInstallSharedOnly(t *testing.T) {
	p := paths(t)
	res, err := Install(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Dirs, []string{SharedSkillDir(p)}) || len(res.Reload) != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestInstallUnknownHarness(t *testing.T) {
	if _, err := Install(paths(t), []string{"pi"}); err == nil {
		t.Error("want error for unknown harness")
	}
}

func TestUninstall(t *testing.T) {
	p := paths(t)
	if _, err := Install(p, []string{state.HarnessClaude, state.HarnessOpenCode}); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(p.Home, ".claude", "skills", "other", "SKILL.md")
	os.MkdirAll(filepath.Dir(other), 0o755)
	os.WriteFile(other, []byte("x"), 0o644)

	removed, err := Uninstall(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(removed) != 2 {
		t.Errorf("removed = %v", removed)
	}
	for _, d := range []string{SharedSkillDir(p), filepath.Join(p.Home, ".claude", "skills", "dev")} {
		if _, err := os.Stat(d); !os.IsNotExist(err) {
			t.Errorf("%s still exists", d)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Error("other skills must be kept")
	}
	if _, err := os.Stat(HashFile(p)); !os.IsNotExist(err) {
		t.Error("hash file not removed")
	}
	if _, err := Uninstall(p); err != nil {
		t.Errorf("second uninstall: %v", err)
	}
}
