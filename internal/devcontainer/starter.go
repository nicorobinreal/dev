// Package devcontainer knows the starter .devcontainer (embedded templates and
// the hash registry used to recognise them), detects which kind a clone has,
// builds the shell scripts that read and write .devcontainer/ in the VM, and
// wraps the devcontainer CLI (argv for `devcontainer up` / `exec`, parsing of
// its JSON result).
package devcontainer

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/nicorobinreal/dev/templates"
)

// Dir is the devcontainer folder, relative to the workspace.
const Dir = ".devcontainer"

// RootFile is the alternative single-file config location the devcontainer
// CLI also accepts. Its presence (without a .devcontainer/ folder) means the
// project has its own config, so dev neither writes a starter nor calls it
// "none".
const RootFile = ".devcontainer.json"

// StarterImage is the published base image of the latest starter.
const StarterImage = "ghcr.io/nicorobinreal/dev-starter:v1"

// StarterImageEnv names the environment variable that overrides the starter's
// base image. devcontainer.json passes ${localEnv:DEV_STARTER_IMAGE} as a build
// arg, so it must be set in the environment of `devcontainer up` on the VM.
const StarterImageEnv = "DEV_STARTER_IMAGE"

// registry lists every released starter and the hash of its files. Released
// entries are append-only: never edit templates/devcontainer/vN once vN has
// shipped, add vN+1 instead (and bump StarterImage).
var registry = []struct {
	Version int
	Hash    string
}{
	{1, "cba8e30868686f5fb5f969cc9c0843e92be7e90d0d03a42f90f9914ac307dccd"},
}

// Starter is one released starter version.
type Starter struct {
	Version int
	// Files maps paths relative to .devcontainer/ (e.g. "devcontainer.json")
	// to their contents.
	Files map[string][]byte
	// Hash is the registered SHA-256 (see HashFiles).
	Hash string
}

// Label is the value stored in state.Project.Devcontainer, e.g. "starter-v1".
func (s Starter) Label() string { return Label(KindStarter, s.Version) }

// Starters returns every released starter, oldest first.
func Starters() []Starter {
	out := make([]Starter, 0, len(registry))
	for _, r := range registry {
		files, err := templateFiles(r.Version)
		if err != nil {
			panic(err) // embedded files are fixed at build time
		}
		out = append(out, Starter{Version: r.Version, Files: files, Hash: r.Hash})
	}
	return out
}

// Latest returns the starter `dev new` writes.
func Latest() Starter {
	s := Starters()
	return s[len(s)-1]
}

// templateFiles reads templates/devcontainer/v<version>/ from the embed.
func templateFiles(version int) (map[string][]byte, error) {
	root := "devcontainer/v" + strconv.Itoa(version)
	files := map[string][]byte{}
	err := fs.WalkDir(templates.FS, root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(templates.FS, p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, root+"/")] = b
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("starter v%d: %w", version, err)
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("starter v%d: no embedded files", version)
	}
	return files, nil
}

// HashFiles is the starter hash: hex SHA-256 over the files sorted by path
// (relative to .devcontainer/, forward slashes), each contributing
// "<path>\x00<decimal length>\x00<content>". File modes, timestamps and
// directory entries do not count.
func HashFiles(files map[string][]byte) string {
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	h := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(files[p]))
		h.Write(files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Kind classifies a clone's devcontainer config.
type Kind string

const (
	KindNone    Kind = "none"    // no .devcontainer/ (and no .devcontainer.json)
	KindStarter Kind = "starter" // byte-identical to a released starter
	KindCustom  Kind = "custom"  // anything else
)

// Detect classifies the files of a workspace. Keys are paths relative to the
// workspace (".devcontainer/devcontainer.json", ".devcontainer.json"), as
// returned by ParseReadOutput; other keys are ignored. version is the starter
// version for KindStarter, else 0.
func Detect(files map[string][]byte) (kind Kind, version int) {
	dc := DirFiles(files)
	if len(dc) == 0 {
		for p := range files {
			// .devcontainer as a symlink or plain file, or a root config.
			if p = cleanRel(p); p == Dir || p == RootFile {
				return KindCustom, 0
			}
		}
		return KindNone, 0
	}
	h := HashFiles(dc)
	for _, r := range registry {
		if r.Hash == h {
			return KindStarter, r.Version
		}
	}
	return KindCustom, 0
}

// Label returns what to store in state.Project.Devcontainer: "starter-vN",
// "custom", or "" for KindNone.
func Label(kind Kind, version int) string {
	switch kind {
	case KindStarter:
		return fmt.Sprintf("starter-v%d", version)
	case KindCustom:
		return string(KindCustom)
	}
	return ""
}

// DirFiles returns the entries under .devcontainer/ with that prefix
// stripped (the form HashFiles expects).
func DirFiles(files map[string][]byte) map[string][]byte {
	out := map[string][]byte{}
	for p, b := range files {
		p = cleanRel(p)
		if rest, ok := strings.CutPrefix(p, Dir+"/"); ok && rest != "" {
			out[rest] = b
		}
	}
	return out
}

func cleanRel(p string) string {
	p = path.Clean(strings.TrimPrefix(p, "./"))
	return strings.TrimPrefix(p, "./")
}
