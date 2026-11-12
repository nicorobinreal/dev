package devcontainer

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func bash(t *testing.T, script string) string {
	t.Helper()
	sh, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	out, err := exec.Command(sh, "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("script failed: %v\n%s\nscript:\n%s", err, out, script)
	}
	return string(out)
}

// workspace returns a temp dir whose name needs quoting.
func workspace(t *testing.T) string {
	ws := filepath.Join(t.TempDir(), "my repo's")
	if err := os.Mkdir(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func writeFiles(t *testing.T, root string, files map[string][]byte) {
	t.Helper()
	for p, b := range files {
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Golden: what the script renders in a workspace equals the template files.
func TestWriteStarterScriptRendersTemplates(t *testing.T) {
	ws := workspace(t)
	out := bash(t, WriteStarterScript(ws))
	if ok, err := StarterWritten(out); !ok || err != nil {
		t.Fatalf("StarterWritten(%q) = %v, %v", out, ok, err)
	}
	got := readDir(t, filepath.Join(ws, Dir))
	want := readDir(t, filepath.Join("..", "..", "templates", "devcontainer", "v1"))
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rendered starter differs from templates/devcontainer/v1")
	}
	st, err := os.Stat(filepath.Join(ws, Dir))
	if err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("dir mode = %v, %v", st.Mode(), err)
	}
	ents, _ := os.ReadDir(ws)
	if len(ents) != 1 {
		t.Errorf("leftover files in workspace: %v", ents)
	}
}

func TestWriteStarterScriptKeepsExisting(t *testing.T) {
	cases := map[string]func(ws string){
		"custom dir": func(ws string) {
			writeFiles(t, ws, map[string][]byte{".devcontainer/devcontainer.json": []byte("{}")})
		},
		"root json": func(ws string) {
			writeFiles(t, ws, map[string][]byte{".devcontainer.json": []byte("{}")})
		},
		"symlink": func(ws string) {
			if err := os.Symlink("/nonexistent", filepath.Join(ws, Dir)); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			ws := workspace(t)
			setup(ws)
			out := bash(t, WriteStarterScript(ws))
			if ok, err := StarterWritten(out); ok || err != nil {
				t.Fatalf("StarterWritten(%q) = %v, %v", out, ok, err)
			}
			if b, err := os.ReadFile(filepath.Join(ws, Dir, "devcontainer.json")); err == nil && string(b) != "{}" {
				t.Error("existing devcontainer.json overwritten")
			}
		})
	}
}

func TestWriteStarterScriptFillsEmptyDir(t *testing.T) {
	ws := workspace(t)
	if err := os.Mkdir(filepath.Join(ws, Dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, _ := StarterWritten(bash(t, WriteStarterScript(ws))); !ok {
		t.Fatal("empty .devcontainer/ not filled")
	}
	if !reflect.DeepEqual(readDir(t, filepath.Join(ws, Dir)), Latest().Files) {
		t.Error("wrong content")
	}
}

func TestReadScriptRoundTrip(t *testing.T) {
	t.Run("none", func(t *testing.T) {
		ws := workspace(t)
		files, err := ParseReadOutput(bash(t, ReadScript(ws)))
		if err != nil {
			t.Fatal(err)
		}
		if k, _ := Detect(files); k != KindNone {
			t.Errorf("kind = %s", k)
		}
	})
	t.Run("starter", func(t *testing.T) {
		ws := workspace(t)
		bash(t, WriteStarterScript(ws))
		files, err := ParseReadOutput(bash(t, ReadScript(ws)))
		if err != nil {
			t.Fatal(err)
		}
		if k, v := Detect(files); k != KindStarter || v != 1 {
			t.Errorf("Detect = %s %d, files %v", k, v, keys(files))
		}
	})
	t.Run("custom nested", func(t *testing.T) {
		ws := workspace(t)
		writeFiles(t, ws, map[string][]byte{
			".devcontainer/devcontainer.json":     []byte("{}"),
			".devcontainer/sub/devcontainer.json": []byte("{\"x\":1}"),
			"other.txt":                           []byte("no"),
		})
		files, err := ParseReadOutput(bash(t, ReadScript(ws)))
		if err != nil {
			t.Fatal(err)
		}
		if string(files[".devcontainer/sub/devcontainer.json"]) != `{"x":1}` || files["other.txt"] != nil {
			t.Errorf("files = %v", keys(files))
		}
		if k, _ := Detect(files); k != KindCustom {
			t.Errorf("kind = %s", k)
		}
	})
	t.Run("root json", func(t *testing.T) {
		ws := workspace(t)
		writeFiles(t, ws, map[string][]byte{".devcontainer.json": []byte("{}")})
		files, err := ParseReadOutput(bash(t, ReadScript(ws)))
		if err != nil {
			t.Fatal(err)
		}
		if k, _ := Detect(files); k != KindCustom {
			t.Errorf("kind = %s", k)
		}
	})
	t.Run("symlinked file", func(t *testing.T) {
		ws := workspace(t)
		bash(t, WriteStarterScript(ws))
		df := filepath.Join(ws, Dir, "Dockerfile")
		b, _ := os.ReadFile(df)
		writeFiles(t, ws, map[string][]byte{"Dockerfile.real": b})
		os.Remove(df)
		if err := os.Symlink("../Dockerfile.real", df); err != nil {
			t.Fatal(err)
		}
		files, err := ParseReadOutput(bash(t, ReadScript(ws)))
		if err != nil {
			t.Fatal(err)
		}
		if k, _ := Detect(files); k != KindCustom {
			t.Errorf("kind = %s", k)
		}
	})
}

func TestParseReadOutputErrors(t *testing.T) {
	if _, err := ParseReadOutput("not base64!!"); err == nil {
		t.Error("want error for bad base64")
	}
	if f, err := ParseReadOutput("\n  \n"); err != nil || len(f) != 0 {
		t.Errorf("blank output = %v, %v", f, err)
	}
}

func TestStarterWrittenErrors(t *testing.T) {
	if _, err := StarterWritten(""); err == nil {
		t.Error("want error for empty output")
	}
	if _, err := StarterWritten("garbage"); err == nil {
		t.Error("want error for unexpected output")
	}
}

func TestUntrackedStarter(t *testing.T) {
	starter := prefixed(Latest().Files)
	custom := prefixed(map[string][]byte{"devcontainer.json": []byte("{}")})
	cases := []struct {
		name      string
		untracked []string
		files     map[string][]byte
		pristine  bool
		rest      []string
	}{
		{"collapsed dir", []string{".devcontainer/"}, starter, true, nil},
		{"per file", []string{".devcontainer/Dockerfile", ".devcontainer/devcontainer.json"}, starter, true, nil},
		{"plus other work", []string{".devcontainer/", "notes.txt"}, starter, true, []string{"notes.txt"}},
		{"custom", []string{".devcontainer/"}, custom, false, []string{".devcontainer/"}},
		{"not untracked", []string{"notes.txt"}, starter, false, []string{"notes.txt"}},
		{"nothing", nil, starter, false, nil},
		{"similar name", []string{".devcontainer-old/"}, starter, false, []string{".devcontainer-old/"}},
	}
	for _, c := range cases {
		if got := IsStarterUntracked(c.untracked, c.files); got != c.pristine {
			t.Errorf("%s: IsStarterUntracked = %v", c.name, got)
		}
		if got := FilterUntracked(c.untracked, c.files); !reflect.DeepEqual(got, c.rest) {
			t.Errorf("%s: FilterUntracked = %q, want %q", c.name, got, c.rest)
		}
	}
}

func keys(m map[string][]byte) string {
	var k []string
	for p := range m {
		k = append(k, p)
	}
	return strings.Join(k, ",")
}
