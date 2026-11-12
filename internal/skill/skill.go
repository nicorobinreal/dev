// Package skill installs the embedded dev agent skill into the harnesses'
// global skills folders and works out which harnesses need a reload notice.
package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nicorobinreal/dev/internal/state"
	skillfiles "github.com/nicorobinreal/dev/skill"
)

// Name is the skill's folder name inside each skills folder.
const Name = "dev"

// SharedDir is the cross-harness skills folder, relative to home. The skill is
// always written there.
const SharedDir = ".agents/skills"

// Harness is one row of the harness table.
type Harness struct {
	ID          string // state.Harness*
	Name        string // display name, used in notices
	Dir         string // global skills folder, relative to home
	ReadsShared bool   // reads ~/.agents/skills natively
}

// Harnesses is the one table mapping harnesses to skills folders.
var Harnesses = []Harness{
	{ID: state.HarnessClaude, Name: "Claude Code", Dir: ".claude/skills", ReadsShared: false},
	{ID: state.HarnessCodex, Name: "Codex", Dir: ".agents/skills", ReadsShared: true},
	{ID: state.HarnessOpenCode, Name: "OpenCode", Dir: ".config/opencode/skills", ReadsShared: true},
}

// Lookup returns the harness with id.
func Lookup(id string) (Harness, bool) {
	for _, h := range Harnesses {
		if h.ID == id {
			return h, true
		}
	}
	return Harness{}, false
}

// SkillDir is the folder the given harness loads the dev skill from.
func (h Harness) SkillDir(p state.Paths) string {
	if h.ReadsShared {
		return SharedSkillDir(p)
	}
	return filepath.Join(p.Home, filepath.FromSlash(h.Dir), Name)
}

// SharedSkillDir is ~/.agents/skills/dev.
func SharedSkillDir(p state.Paths) string {
	return filepath.Join(p.Home, filepath.FromSlash(SharedDir), Name)
}

// HashFile stores the hash of the last installed skill.
func HashFile(p state.Paths) string { return filepath.Join(p.StateDir(), "skill.hash") }

// Files returns the embedded skill files keyed by path relative to the skill
// folder (e.g. "SKILL.md").
func Files() map[string][]byte {
	files := map[string][]byte{}
	err := fs.WalkDir(skillfiles.FS, skillfiles.Root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(skillfiles.FS, p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, skillfiles.Root+"/")] = b
		return nil
	})
	if err != nil {
		panic(err) // embedded at build time
	}
	return files
}

// Hash is the skill's hash: hex SHA-256 over the files sorted by path, each
// contributing "<path>\x00<decimal length>\x00<content>" (the same scheme as
// the starter devcontainer hash).
func Hash() string { return hashFiles(Files()) }

func hashFiles(files map[string][]byte) string {
	paths := sortedKeys(files)
	h := sha256.New()
	for _, p := range paths {
		fmt.Fprintf(h, "%s\x00%d\x00", p, len(files[p]))
		h.Write(files[p])
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Result says what Install did.
type Result struct {
	Hash         string   `json:"hash"`
	PreviousHash string   `json:"previous_hash,omitempty"` // "" on first install
	Changed      bool     `json:"changed"`                 // hash differs from the stored one (or first install)
	Dirs         []string `json:"dirs"`                    // skill folders written, sorted
	// Reload lists display names of harnesses that must reload skills: any
	// harness whose skill folder is new, and, when the skill changed, every
	// harness except Claude Code (which watches existing skill files).
	Reload []string `json:"reload,omitempty"`
}

// Notices returns one "skill updated — reload skills in <harness>" line per
// harness in Reload.
func (r Result) Notices() []string {
	out := make([]string, len(r.Reload))
	for i, h := range r.Reload {
		out[i] = Notice(h)
	}
	return out
}

// Notice is the line printed for a harness that needs a reload.
func Notice(harnessName string) string {
	return "skill updated — reload skills in " + harnessName
}

// Install writes the skill to ~/.agents/skills/dev/ and to the own folder of
// each selected harness that doesn't read the shared one, then stores the
// hash. Unknown harness IDs are an error. Files are written atomically and
// stale files in the skill folders are removed.
func Install(p state.Paths, harnesses []string) (Result, error) {
	var sel []Harness
	for _, id := range harnesses {
		h, ok := Lookup(id)
		if !ok {
			return Result{}, fmt.Errorf("unknown harness %q", id)
		}
		sel = append(sel, h)
	}

	files := Files()
	res := Result{Hash: hashFiles(files)}
	prev, err := os.ReadFile(HashFile(p))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}
	res.PreviousHash = strings.TrimSpace(string(prev))
	res.Changed = res.PreviousHash != res.Hash

	dirs := map[string]bool{SharedSkillDir(p): true}
	for _, h := range sel {
		dirs[h.SkillDir(p)] = true
	}
	existed := map[string]bool{}
	for d := range dirs {
		existed[d] = isDir(d)
	}
	for _, d := range sortedKeys(dirs) {
		if err := writeDir(d, files); err != nil {
			return res, err
		}
		res.Dirs = append(res.Dirs, d)
	}

	for _, h := range sel {
		isNew := !existed[h.SkillDir(p)]
		if isNew || (res.Changed && h.ID != state.HarnessClaude) {
			res.Reload = append(res.Reload, h.Name)
		}
	}

	if res.Changed {
		if err := state.WriteFileAtomic(HashFile(p), []byte(res.Hash+"\n"), 0o644); err != nil {
			return res, err
		}
	}
	return res, nil
}

// writeDir makes dir hold exactly files: unchanged files are left alone (so
// file watchers see no churn), changed ones are replaced atomically, stale
// ones removed.
func writeDir(dir string, files map[string][]byte) error {
	for _, rel := range sortedKeys(files) {
		dst := filepath.Join(dir, filepath.FromSlash(rel))
		if cur, err := os.ReadFile(dst); err == nil && string(cur) == string(files[rel]) {
			continue
		}
		if err := state.WriteFileAtomic(dst, files[rel], 0o644); err != nil {
			return fmt.Errorf("install skill: %w", err)
		}
	}
	var stale []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if _, ok := files[filepath.ToSlash(rel)]; !ok {
			stale = append(stale, path)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("install skill: %w", err)
	}
	for _, s := range stale {
		if err := os.Remove(s); err != nil {
			return fmt.Errorf("install skill: %w", err)
		}
	}
	removeEmptyDirs(dir)
	return nil
}

// removeEmptyDirs removes empty subfolders below root (not root itself).
func removeEmptyDirs(root string) {
	var dirs []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() && path != root {
			dirs = append(dirs, path)
		}
		return nil
	})
	for i := len(dirs) - 1; i >= 0; i-- {
		_ = os.Remove(dirs[i]) // fails (and is ignored) when not empty
	}
}

// Uninstall removes the dev skill from the shared folder and every harness
// folder in the table (selected or not), and the stored hash. Missing folders
// are fine. It returns the folders removed.
func Uninstall(p state.Paths) ([]string, error) {
	dirs := map[string]bool{SharedSkillDir(p): true}
	for _, h := range Harnesses {
		dirs[h.SkillDir(p)] = true
	}
	var removed []string
	for _, d := range sortedKeys(dirs) {
		if !isDir(d) {
			continue
		}
		if err := os.RemoveAll(d); err != nil {
			return removed, err
		}
		removed = append(removed, d)
	}
	if err := os.Remove(HashFile(p)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return removed, err
	}
	return removed, nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
