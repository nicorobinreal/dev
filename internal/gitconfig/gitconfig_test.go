package gitconfig

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/run"
)

func TestHost(t *testing.T) {
	f := (&run.Fake{}).
		On("git config --global --get user.name", "Ada Lovelace\n").
		On("git config --global --get user.email", "1+ada@users.noreply.github.com\n").
		Fail("git config --global --get init.defaultBranch", 1, "")
	got, err := Host(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	want := []Entry{{"user.name", "Ada Lovelace"}, {"user.email", "1+ada@users.noreply.github.com"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Host = %+v, want %+v", got, want)
	}
}

func TestHostErrors(t *testing.T) {
	// Exit codes other than 1 (e.g. 3: invalid config file) are errors.
	f := (&run.Fake{}).Fail("git config --global --get user.name", 3, "bad config")
	if _, err := Host(context.Background(), f); err == nil {
		t.Fatal("want an error for an unreadable config")
	}
}

func TestSyncScriptRejects(t *testing.T) {
	if _, err := SyncScript([]Entry{{"credential.helper", "osxkeychain"}}); err == nil {
		t.Error("forwarded a key outside Keys")
	}
	if _, err := SyncScript([]Entry{{"user.name", "a\nb"}}); err == nil {
		t.Error("forwarded a multi-line value")
	}
}

type fakeShell struct{ scripts []string }

func (f *fakeShell) Shell(_ context.Context, script string, _ io.Reader) (run.Result, error) {
	f.scripts = append(f.scripts, script)
	return run.Result{}, nil
}

func TestSyncVMQuotes(t *testing.T) {
	sh := &fakeShell{}
	if err := SyncVM(context.Background(), sh, []Entry{{"user.name", "O'Brien; rm -rf /"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sh.scripts[0], `git config --file "$tmp" user.name 'O'\''Brien; rm -rf /'`) {
		t.Fatalf("value not quoted:\n%s", sh.scripts[0])
	}
}

// TestSyncScriptRuns executes the script with real bash and git in a fake
// HOME: the file holds exactly the entries, a second run replaces it, and the
// include line is added once.
func TestSyncScriptRuns(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	home := t.TempDir()
	runScript := func(entries []Entry) {
		t.Helper()
		script, err := SyncScript(entries)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("bash", "-c", script)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("script failed: %v\n%s", err, out)
		}
	}
	gitGet := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Env = []string{"HOME=" + home, "PATH=" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1"}
		out, _ := cmd.Output()
		return strings.TrimSpace(string(out))
	}

	runScript([]Entry{{"user.name", "O'Brien"}, {"user.email", "1+ob@users.noreply.github.com"}, {"init.defaultBranch", "main"}})
	file := filepath.Join(home, VMFile)
	if got := gitGet("config", "--file", file, "user.name"); got != "O'Brien" {
		t.Errorf("user.name = %q", got)
	}
	// The VM's global config sees it through the include.
	if got := gitGet("config", "--global", "--includes", "user.email"); got != "1+ob@users.noreply.github.com" {
		t.Errorf("included user.email = %q", got)
	}
	if fi, err := os.Stat(file); err != nil || fi.Mode().Perm() != 0o644 {
		t.Errorf("file mode: %v %v", fi, err)
	}

	// A second run with fewer keys replaces the file and keeps one include.
	runScript([]Entry{{"user.name", "Ada"}})
	if got := gitGet("config", "--file", file, "--list"); got != "user.name=Ada" {
		t.Errorf("file after resync = %q", got)
	}
	if got := gitGet("config", "--global", "--get-all", "include.path"); got != "~/"+VMFile {
		t.Errorf("include.path = %q", got)
	}

	// No entries still leaves a file for the bind mount.
	runScript(nil)
	if fi, err := os.Stat(file); err != nil || fi.Size() != 0 {
		t.Errorf("empty sync: %v %v", fi, err)
	}
	if left, _ := filepath.Glob(filepath.Join(home, VMDir, ".config.*")); len(left) != 0 {
		t.Errorf("temp files left behind: %v", left)
	}
}
