package gh

import (
	"context"
	"encoding/base64"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/run"
)

// fakeShell records scripts and stdin and answers with a fixed stdout.
type fakeShell struct {
	scripts, stdins []string
	out             string
	err             error
}

func (f *fakeShell) Shell(_ context.Context, script string, stdin io.Reader) (run.Result, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.scripts = append(f.scripts, script)
	f.stdins = append(f.stdins, in)
	return run.Result{Stdout: f.out}, f.err
}

func TestHostToken(t *testing.T) {
	f := (&run.Fake{}).On("gh auth token", "gho_abc123\n")
	tok, err := HostToken(context.Background(), f)
	if err != nil || tok != "gho_abc123" {
		t.Fatalf("HostToken = %q, %v", tok, err)
	}
	f2 := (&run.Fake{}).Fail("gh auth token", 1, "not logged in")
	if _, err := HostToken(context.Background(), f2); err == nil {
		t.Fatal("want error when gh is logged out")
	}
	f3 := (&run.Fake{}).On("gh auth token", "")
	if _, err := HostToken(context.Background(), f3); err == nil {
		t.Fatal("want error for empty token")
	}
}

func TestSyncVMTokenOnStdinOnly(t *testing.T) {
	sh := &fakeShell{}
	if err := SyncVM(context.Background(), sh, "gho_secret\n"); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(sh.scripts[0], "gho_secret") {
		t.Fatal("token leaked into script text")
	}
	if sh.stdins[0] != "gho_secret\n" {
		t.Fatalf("stdin = %q", sh.stdins[0])
	}
	for _, want := range []string{"mktemp", "mv -f", "chmod 600", "chmod 700", "gh auth login", "gh auth setup-git"} {
		if !strings.Contains(sh.scripts[0], want) {
			t.Errorf("script lacks %q", want)
		}
	}
	if err := SyncVM(context.Background(), sh, " "); err == nil {
		t.Fatal("empty token should fail")
	}
}

// TestSyncScriptRuns executes the script locally with a fake HOME and no gh
// on PATH, checking the file write path and permissions.
func TestSyncScriptRuns(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	home := t.TempDir()
	cmd := exec.Command(bash, "-c", SyncScript)
	cmd.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin"}
	cmd.Stdin = strings.NewReader("tok1\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		if strings.Contains(string(out), "base64") || strings.Contains(string(out), "mktemp") {
			t.Skip(string(out))
		}
		t.Fatalf("%v: %s", err, out)
	}
	b, err := os.ReadFile(filepath.Join(home, VMTokenFile))
	if err != nil || string(b) != "tok1\n" {
		t.Fatalf("token file = %q, %v", b, err)
	}
	fi, _ := os.Stat(filepath.Join(home, VMTokenFile))
	di, _ := os.Stat(filepath.Join(home, VMTokenDir))
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 {
		t.Fatalf("modes file=%v dir=%v", fi.Mode(), di.Mode())
	}
	ents, _ := os.ReadDir(filepath.Join(home, VMTokenDir))
	if len(ents) != 1 {
		t.Fatalf("temp file left behind: %v", ents)
	}
}

func TestContainerLoginArgs(t *testing.T) {
	got := strings.Join(ContainerLoginArgs(), " ")
	if got != "sh -c gh auth login --with-token < /run/secrets/gh/token && gh auth setup-git" {
		t.Fatalf("got %q", got)
	}
}

func TestParseRepo(t *testing.T) {
	ok := []string{
		"owner/repo",
		"github.com/owner/repo",
		"https://github.com/owner/repo",
		"https://github.com/owner/repo.git",
		"https://github.com/owner/repo/",
		"http://www.github.com/owner/repo",
		"git@github.com:owner/repo.git",
		"git@github.com:owner/repo",
		"ssh://git@github.com/owner/repo.git",
		"https://x-access-token@github.com/owner/repo.git",
	}
	for _, arg := range ok {
		url, dir, err := NormalizeRepo(arg)
		if err != nil || url != "https://github.com/owner/repo" || dir != "repo" {
			t.Errorf("NormalizeRepo(%q) = %q, %q, %v", arg, url, dir, err)
		}
	}
	r, err := ParseRepo("nicorobinreal/dev-e2e.compose")
	if err != nil || r.Dir() != "dev-e2e.compose" || r.String() != "nicorobinreal/dev-e2e.compose" {
		t.Errorf("ParseRepo dotted = %+v, %v", r, err)
	}
	bad := []string{
		"", "repo", "owner/repo/extra", "https://gitlab.com/owner/repo",
		"git@gitlab.com:owner/repo.git", "https://github.com/owner",
		"https://github.com/owner/repo/tree/main", "file:///tmp/repo",
		"owner/..", "-owner/repo", "bitbucket.org/owner/repo", "owner/re po",
	}
	for _, arg := range bad {
		if _, err := ParseRepo(arg); err == nil {
			t.Errorf("ParseRepo(%q) succeeded, want error", arg)
		}
	}
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

func TestWorkCheck(t *testing.T) {
	status := "?? .devcontainer/\x00 M src/a.go\x00R  new.go\x00old.go\x00?? weird name\nx\x00D  gone.txt\x00"
	out := "repo ok\nstatus " + b64(status) + "\nunpushed " + b64("abc123 fix it\x00def456 wip\x00") + "\nstashes 2\n"
	sh := &fakeShell{out: out}
	w, err := WorkCheck(context.Background(), sh, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if !w.RepoExists || strings.Join(w.Untracked, "|") != ".devcontainer/|weird name\nx" ||
		strings.Join(w.Modified, "|") != "src/a.go|new.go|gone.txt" ||
		len(w.Unpushed) != 2 || w.Unpushed[0] != "abc123 fix it" || w.Stashes != 2 {
		t.Fatalf("WorkCheck = %+v", w)
	}
	if !strings.HasPrefix(sh.scripts[0], "set -- 'shop'\n") {
		t.Fatalf("script header: %q", sh.scripts[0][:20])
	}
	if w.Clean(nil) {
		t.Fatal("dirty repo reported clean")
	}
}

func TestWorkCheckCleanWithStarter(t *testing.T) {
	out := "repo ok\nstatus " + b64("?? .devcontainer/\x00") + "\nunpushed \nstashes 0\n"
	w, err := WorkCheck(context.Background(), &fakeShell{out: out}, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if w.Clean(nil) {
		t.Fatal("untracked dir must count without a filter")
	}
	if !w.Clean(func(p string) bool { return p == ".devcontainer/" }) {
		t.Fatalf("starter-only repo should be clean: %+v", w)
	}
}

func TestWorkCheckMissingRepo(t *testing.T) {
	w, err := WorkCheck(context.Background(), &fakeShell{out: "repo missing\n"}, "shop")
	if err != nil || w.RepoExists || !w.Clean(nil) {
		t.Fatalf("missing repo = %+v, %v", w, err)
	}
	if _, err := WorkCheck(context.Background(), &fakeShell{}, "../x"); err == nil {
		t.Fatal("bad repo dir accepted")
	}
	if _, err := WorkCheck(context.Background(), &fakeShell{out: "garbage"}, "shop"); err == nil {
		t.Fatal("garbage output accepted")
	}
}

// TestWorkCheckScriptAgainstRealGit runs the script against a local repo
// when git and GNU-compatible base64 are available.
func TestWorkCheckScriptAgainstRealGit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	if err := exec.Command("bash", "-c", "echo x | base64 -w0").Run(); err != nil {
		t.Skip("base64 lacks -w0 (BSD); covered in the VM")
	}
	home := t.TempDir()
	repo := filepath.Join(home, "proj")
	sh := func(script string) string {
		cmd := exec.Command("bash", "-c", script)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "HOME="+home, "GIT_CONFIG_NOSYSTEM=1",
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", script, err, out)
		}
		return string(out)
	}
	_ = os.MkdirAll(filepath.Join(repo, ".devcontainer"), 0o755)
	sh("git init -q && echo a > a && git add a && git commit -qm one && echo x > .devcontainer/devcontainer.json")
	out := sh("set -- proj\n" + workCheckScript)
	w, err := parseWorkCheck(out)
	if err != nil {
		t.Fatal(err)
	}
	if !w.RepoExists || strings.Join(w.Untracked, ",") != ".devcontainer/" || len(w.Unpushed) != 1 {
		t.Fatalf("real git = %+v\n%s", w, out)
	}
}
