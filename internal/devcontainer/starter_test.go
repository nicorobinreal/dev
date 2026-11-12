package devcontainer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/templates"
)

func TestRegisteredHashesMatchEmbeddedFiles(t *testing.T) {
	for _, s := range Starters() {
		if got := HashFiles(s.Files); got != s.Hash {
			t.Errorf("starter v%d: embedded files hash to %s but the registry in starter.go says %s.\n"+
				"Released starters must never change (clones on other Macs carry the old bytes). "+
				"If v%d is not released yet, update its hash in starter.go; otherwise restore "+
				"templates/devcontainer/v%d and add the change as v%d (new folder, new registry entry, bump StarterImage).",
				s.Version, got, s.Hash, s.Version, s.Version, s.Version+1)
		}
	}
}

func TestEveryTemplateVersionIsRegistered(t *testing.T) {
	ents, err := fs.ReadDir(templates.FS, "devcontainer")
	if err != nil {
		t.Fatal(err)
	}
	var dirs []int
	for _, e := range ents {
		v, err := strconv.Atoi(strings.TrimPrefix(e.Name(), "v"))
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "v") || err != nil {
			t.Errorf("unexpected entry templates/devcontainer/%s", e.Name())
			continue
		}
		dirs = append(dirs, v)
	}
	var reg []int
	for i, r := range registry {
		reg = append(reg, r.Version)
		if r.Version != i+1 {
			t.Errorf("registry versions must be 1, 2, 3, ...: entry %d is v%d", i, r.Version)
		}
	}
	if !reflect.DeepEqual(dirs, reg) {
		t.Errorf("template folders %v != registered versions %v: register the new starter's hash in starter.go", dirs, reg)
	}
}

func TestLatestStarterHeaderAndImage(t *testing.T) {
	s := Latest()
	want := fmt.Sprintf("// dev starter v%d\n", s.Version)
	if !strings.HasPrefix(string(s.Files["devcontainer.json"]), want) {
		t.Errorf("devcontainer.json must start with %q", want)
	}
	if !strings.HasSuffix(StarterImage, fmt.Sprintf(":v%d", s.Version)) {
		t.Errorf("StarterImage %s does not match latest starter v%d", StarterImage, s.Version)
	}
	df := string(s.Files["Dockerfile"])
	if !strings.Contains(df, "FROM ${"+StarterImageEnv+":-"+StarterImage+"}") {
		t.Errorf("Dockerfile must build FROM ${%s:-%s}:\n%s", StarterImageEnv, StarterImage, df)
	}
	if s.Label() != fmt.Sprintf("starter-v%d", s.Version) {
		t.Errorf("Label = %s", s.Label())
	}
}

// stripJSONC drops whole-line // comments (the only kind the starter uses).
func stripJSONC(b []byte) []byte {
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "//") {
			continue
		}
		out = append(out, l)
	}
	return []byte(strings.Join(out, "\n"))
}

func TestStarterDevcontainerJSON(t *testing.T) {
	var c struct {
		Build struct {
			Dockerfile string            `json:"dockerfile"`
			Args       map[string]string `json:"args"`
		} `json:"build"`
		ContainerUser    string            `json:"containerUser"`
		RemoteUser       string            `json:"remoteUser"`
		RunArgs          []string          `json:"runArgs"`
		WorkspaceMount   string            `json:"workspaceMount"`
		WorkspaceFolder  string            `json:"workspaceFolder"`
		Mounts           []string          `json:"mounts"`
		ContainerEnv     map[string]string `json:"containerEnv"`
		PostStartCommand string            `json:"postStartCommand"`
	}
	if err := json.Unmarshal(stripJSONC(Latest().Files["devcontainer.json"]), &c); err != nil {
		t.Fatalf("devcontainer.json is not valid JSONC: %v", err)
	}
	// The default must not live in ${localEnv:VAR:default}: the devcontainer
	// CLI splits on ':' and would drop the image tag.
	if got := c.Build.Args[StarterImageEnv]; got != "${localEnv:"+StarterImageEnv+"}" {
		t.Errorf("build.args.%s = %q", StarterImageEnv, got)
	}
	if c.Build.Dockerfile != "Dockerfile" || c.ContainerUser != "root" || c.RemoteUser != "root" {
		t.Errorf("unexpected build/user settings: %+v", c)
	}
	if !reflect.DeepEqual(c.RunArgs, []string{"--network=host"}) {
		t.Errorf("runArgs = %v", c.RunArgs)
	}
	if c.WorkspaceFolder != "${localWorkspaceFolder}" || !strings.Contains(c.WorkspaceMount, "target=${localWorkspaceFolder}") {
		t.Errorf("workspace path not preserved: %q %q", c.WorkspaceMount, c.WorkspaceFolder)
	}
	mounts := strings.Join(c.Mounts, "\n")
	for _, want := range []string{"target=/run/secrets/gh", "target=/run/secrets/ssh", "target=/root/.claude-auth", "target=/root/.codex", "target=/root/.local/share/opencode", "target=/commandhistory"} {
		if !strings.Contains(mounts, want) {
			t.Errorf("missing mount %s", want)
		}
	}
	if c.ContainerEnv["IS_SANDBOX"] != "1" || c.ContainerEnv["CLAUDE_CONFIG_DIR"] != "/root/.claude-auth" {
		t.Errorf("containerEnv = %v", c.ContainerEnv)
	}
	if !strings.Contains(c.PostStartCommand, "/usr/local/bin/dev-sshd-keys") {
		t.Errorf("postStartCommand = %q", c.PostStartCommand)
	}
}

// Golden: the embedded starter equals the files under templates/ on disk.
func TestStarterGolden(t *testing.T) {
	for _, s := range Starters() {
		dir := filepath.Join("..", "..", "templates", "devcontainer", fmt.Sprintf("v%d", s.Version))
		disk := readDir(t, dir)
		if !reflect.DeepEqual(disk, s.Files) {
			t.Errorf("starter v%d: embedded files differ from %s", s.Version, dir)
		}
	}
}

func readDir(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		files[filepath.ToSlash(rel)] = b
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestHashFiles(t *testing.T) {
	a := map[string][]byte{"a": []byte("x"), "b": []byte("y")}
	if HashFiles(a) != HashFiles(map[string][]byte{"b": []byte("y"), "a": []byte("x")}) {
		t.Error("hash depends on map order")
	}
	// Moving bytes between path and content must change the hash.
	if HashFiles(map[string][]byte{"ab": []byte("")}) == HashFiles(map[string][]byte{"a": []byte("b")}) {
		t.Error("path/content boundary not encoded")
	}
	if HashFiles(a) == HashFiles(map[string][]byte{"a": []byte("x")}) {
		t.Error("extra file did not change hash")
	}
	// Reference vector, so the scheme can be reproduced outside Go.
	sum := sha256.Sum256([]byte("a\x001\x00xb\x001\x00y"))
	if got, want := HashFiles(a), hex.EncodeToString(sum[:]); got != want {
		t.Errorf("HashFiles = %s, want %s", got, want)
	}
}

func prefixed(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for p, b := range files {
		out[Dir+"/"+p] = b
	}
	return out
}

func TestDetect(t *testing.T) {
	starter := prefixed(Latest().Files)
	modified := prefixed(Latest().Files)
	modified[Dir+"/Dockerfile"] = append(modified[Dir+"/Dockerfile"], []byte("RUN true\n")...)
	extra := prefixed(Latest().Files)
	extra[Dir+"/extra.sh"] = []byte("echo\n")
	dotSlash := map[string][]byte{}
	for p, b := range starter {
		dotSlash["./"+p] = b
	}
	withRoot := prefixed(Latest().Files)
	withRoot[RootFile] = []byte("{}")

	cases := []struct {
		name    string
		files   map[string][]byte
		kind    Kind
		version int
		label   string
	}{
		{"nil", nil, KindNone, 0, ""},
		{"empty", map[string][]byte{}, KindNone, 0, ""},
		{"unrelated", map[string][]byte{"README.md": []byte("hi")}, KindNone, 0, ""},
		{"starter", starter, KindStarter, Latest().Version, "starter-v1"},
		{"dot-slash", dotSlash, KindStarter, Latest().Version, "starter-v1"},
		{"modified", modified, KindCustom, 0, "custom"},
		{"extra file", extra, KindCustom, 0, "custom"},
		{"root json only", map[string][]byte{RootFile: []byte("{}")}, KindCustom, 0, "custom"},
		{"starter wins over root json", withRoot, KindStarter, 1, "starter-v1"},
		{"symlink", map[string][]byte{Dir: []byte("\x00symlink -> elsewhere")}, KindCustom, 0, "custom"},
	}
	for _, c := range cases {
		k, v := Detect(c.files)
		if k != c.kind || v != c.version {
			t.Errorf("%s: Detect = %s, %d; want %s, %d", c.name, k, v, c.kind, c.version)
		}
		if l := Label(k, v); l != c.label {
			t.Errorf("%s: Label = %q, want %q", c.name, l, c.label)
		}
	}
}

// The release workflow tags the starter image from starter-image/VERSION.
func TestStarterImageVersionFile(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "starter-image", "VERSION"))
	if os.IsNotExist(err) {
		t.Skip("no starter-image/VERSION")
	}
	if err != nil {
		t.Fatal(err)
	}
	if v := strings.TrimSpace(string(b)); !strings.HasSuffix(StarterImage, ":"+v) {
		t.Errorf("starter-image/VERSION is %s but StarterImage is %s", v, StarterImage)
	}
}
