package sshconfig

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/state"
)

var update = flag.Bool("update", false, "rewrite golden files")

// Fixtures use /HOME for the home directory.
const homeToken = "/HOME"

func testPaths(t *testing.T) state.Paths {
	return state.Paths{Home: t.TempDir()}
}

func fixture(t *testing.T, p state.Paths, name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), homeToken, p.Home)
}

func golden(t *testing.T, p state.Paths, name string, got []byte) {
	t.Helper()
	norm := strings.ReplaceAll(string(got), p.Home, homeToken)
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.WriteFile(path, []byte(norm), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run go test -update)", err)
	}
	if norm != string(want) {
		t.Errorf("%s mismatch\n--- got ---\n%s\n--- want ---\n%s", name, norm, want)
	}
	if strings.Contains(norm, "\r") {
		t.Errorf("%s contains CR", name)
	}
}

func TestLimaOptions(t *testing.T) {
	p := testPaths(t)
	opts := LimaOptions(fixture(t, p, "lima-shop.ssh.config"))
	if len(opts) != 16 || opts[0] != `IdentityFile "`+p.Home+`/.lima/_config/user"` || opts[15] != "Port 60022" {
		t.Fatalf("%q", opts)
	}
	if got := LimaOptions("# only comments\n\n"); len(got) != 0 {
		t.Fatalf("%q", got)
	}
	two := "Host a\n  Port 1\nHost b\n  Port 2\n"
	if got := LimaOptions(two); len(got) != 1 || got[0] != "Port 1" {
		t.Fatalf("%q", got)
	}
	if got := LimaOptions("Port 3\r\nUser x\r\n"); len(got) != 2 || got[1] != "User x" {
		t.Fatalf("%q", got)
	}
}

func TestRender(t *testing.T) {
	p := testPaths(t)
	entries := []Entry{
		NewEntry(p, "shop", fixture(t, p, "lima-shop.ssh.config")),
		NewEntry(p, "api", fixture(t, p, "lima-api.ssh.config")),
		NewEntry(p, "stopped", ""),
	}
	out := Render(entries)
	golden(t, p, "render.golden", out)
	// Deterministic regardless of input order.
	rev := []Entry{entries[2], entries[0], entries[1]}
	if string(Render(rev)) != string(out) {
		t.Fatal("render depends on input order")
	}
	if err := Write(p, entries); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(p.SSHConfigFile())
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("stat: %v %v", st, err)
	}
	b, _ := os.ReadFile(p.SSHConfigFile())
	if string(b) != string(out) {
		t.Fatal("Write differs from Render")
	}
}

func TestRenderQuotesSpaces(t *testing.T) {
	p := state.Paths{Home: "/Users/a b"}
	out := string(Render([]Entry{NewEntry(p, "x", "Host lima-x\n  Port 1\n")}))
	if !strings.Contains(out, `IdentityFile "/Users/a b/.lima/_config/user"`) ||
		!strings.Contains(out, `UserKnownHostsFile "/Users/a b/.config/dev/known_hosts"`) {
		t.Fatal(out)
	}
}

func TestEnsureIncludeGolden(t *testing.T) {
	for _, c := range []string{"hosts", "existing-includes", "misplaced", "already", "empty"} {
		t.Run(c, func(t *testing.T) {
			p := testPaths(t)
			in := fixture(t, p, "include/"+c+".in")
			if err := os.MkdirAll(filepath.Dir(p.UserSSHConfig()), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p.UserSSHConfig(), []byte(in), 0o644); err != nil {
				t.Fatal(err)
			}
			changed, err := EnsureInclude(p)
			if err != nil {
				t.Fatal(err)
			}
			got, _ := os.ReadFile(p.UserSSHConfig())
			golden(t, p, "include/"+c+".golden", got)
			if c == "already" && changed {
				t.Error("already-present include changed the file")
			}
			// Mode preserved.
			if st, _ := os.Stat(p.UserSSHConfig()); st.Mode().Perm() != 0o644 {
				t.Errorf("mode %v", st.Mode().Perm())
			}
			// Backup holds the original, only when edited a non-empty file.
			bk, err := os.ReadFile(BackupPath(p))
			if changed && in != "" {
				if err != nil || string(bk) != in {
					t.Errorf("backup: %v %q", err, bk)
				}
			} else if err == nil {
				t.Error("unexpected backup")
			}
			// Idempotent.
			if again, err := EnsureInclude(p); err != nil || again {
				t.Errorf("second EnsureInclude changed=%v err=%v", again, err)
			}
			if ok, err := HasInclude(p); !ok || err != nil {
				t.Errorf("HasInclude = %v %v", ok, err)
			}
			// Remove restores the original minus dev's lines.
			if _, err := RemoveInclude(p); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(p.UserSSHConfig())
			if ok, _ := HasInclude(p); ok {
				t.Error("include still present")
			}
			golden(t, p, "include/"+c+".removed", after)
		})
	}
}

func TestEnsureIncludeCreates(t *testing.T) {
	p := testPaths(t)
	if ok, err := HasInclude(p); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	changed, err := EnsureInclude(p)
	if err != nil || !changed {
		t.Fatalf("%v %v", changed, err)
	}
	st, _ := os.Stat(filepath.Dir(p.UserSSHConfig()))
	if st.Mode().Perm() != 0o700 {
		t.Errorf("dir mode %v", st.Mode().Perm())
	}
	st, _ = os.Stat(p.UserSSHConfig())
	if st.Mode().Perm() != 0o600 {
		t.Errorf("file mode %v", st.Mode().Perm())
	}
	b, _ := os.ReadFile(p.UserSSHConfig())
	if string(b) != IncludeMarker+"\n"+IncludeLine(p)+"\n" {
		t.Fatalf("%q", b)
	}
	if _, err := os.Stat(BackupPath(p)); err == nil {
		t.Error("backup of nonexistent file")
	}
}

func TestEnsureIncludeBackupOnce(t *testing.T) {
	p := testPaths(t)
	os.MkdirAll(filepath.Dir(p.UserSSHConfig()), 0o700)
	os.WriteFile(p.UserSSHConfig(), []byte("Host a\n"), 0o600)
	EnsureInclude(p)
	RemoveInclude(p)
	os.WriteFile(p.UserSSHConfig(), []byte("Host b\n"), 0o600)
	EnsureInclude(p)
	if b, _ := os.ReadFile(BackupPath(p)); string(b) != "Host a\n" {
		t.Fatalf("backup overwritten: %q", b)
	}
}

func TestEnsureIncludeSymlink(t *testing.T) {
	p := testPaths(t)
	os.MkdirAll(filepath.Dir(p.UserSSHConfig()), 0o700)
	target := filepath.Join(p.Home, "dotfiles-ssh-config")
	os.WriteFile(target, []byte("Host a\n"), 0o600)
	if err := os.Symlink(target, p.UserSSHConfig()); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureInclude(p); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(p.UserSSHConfig()); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("symlink replaced by a file")
	}
	b, _ := os.ReadFile(target)
	if !strings.HasPrefix(string(b), IncludeMarker) {
		t.Fatalf("%q", b)
	}
}

func TestEnsureIncludeCRLF(t *testing.T) {
	p := testPaths(t)
	os.MkdirAll(filepath.Dir(p.UserSSHConfig()), 0o700)
	os.WriteFile(p.UserSSHConfig(), []byte("Host a\r\n  Port 2\r\n"), 0o600)
	EnsureInclude(p)
	b, _ := os.ReadFile(p.UserSSHConfig())
	want := IncludeMarker + "\r\n" + IncludeLine(p) + "\r\n\r\nHost a\r\n  Port 2\r\n"
	if string(b) != want {
		t.Fatalf("%q", b)
	}
}

func TestIncludePath(t *testing.T) {
	h, _ := os.UserHomeDir()
	if got := IncludeLine(state.Paths{Home: h}); got != "Include ~/.config/dev/ssh_config" {
		t.Fatal(got)
	}
	if got := IncludePath(state.Paths{Home: "/tmp/x"}); got != "/tmp/x/.config/dev/ssh_config" {
		t.Fatal(got)
	}
}

func TestForgetHostKey(t *testing.T) {
	p := testPaths(t)
	salt := []byte("0123456789abcdefghij")
	m := hmac.New(sha1.New, salt)
	m.Write([]byte("shop-dev"))
	hashed := "|1|" + base64.StdEncoding.EncodeToString(salt) + "|" + base64.StdEncoding.EncodeToString(m.Sum(nil))
	kh := strings.Join([]string{
		"shop-dev ssh-ed25519 AAAA1",
		"api-dev ssh-ed25519 AAAA2",
		"[shop-dev]:2222 ssh-ed25519 AAAA3",
		hashed + " ssh-ed25519 AAAA4",
		"other,shop-dev ecdsa-sha2-nistp256 AAAA5",
		"shop-dev-x ssh-ed25519 AAAA6",
	}, "\n") + "\n"
	if err := state.WriteFileAtomic(KnownHostsFile(p), []byte(kh), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ForgetHostKey(p, "shop"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(KnownHostsFile(p))
	if string(b) != "api-dev ssh-ed25519 AAAA2\nshop-dev-x ssh-ed25519 AAAA6\n" {
		t.Fatalf("%q", b)
	}
	if err := ForgetHostKey(testPaths(t), "shop"); err != nil {
		t.Fatal(err)
	}
}
