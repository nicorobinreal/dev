package devcontainer

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/nicorobinreal/dev/internal/shellquote"
)

// ReadScript returns a bash script that prints, base64-encoded, a tar of the
// workspace's .devcontainer/ folder and .devcontainer.json (whichever exist).
// It prints nothing when neither exists. Run it in the VM, e.g.
// `limactl shell <vm> bash -c <script>`, and pass stdout to ParseReadOutput.
// base64 keeps the bytes intact through limactl/ssh.
func ReadScript(workspace string) string {
	return `set -eu
cd ` + shellquote.One(workspace) + `
set --
if [ -e ` + Dir + ` ] || [ -L ` + Dir + ` ]; then set -- "$@" ` + Dir + `; fi
if [ -e ` + RootFile + ` ] || [ -L ` + RootFile + ` ]; then set -- "$@" ` + RootFile + `; fi
if [ "$#" -gt 0 ]; then COPYFILE_DISABLE=1 tar -cf - -- "$@" | base64; fi
`
}

// ParseReadOutput decodes ReadScript's stdout into files keyed by path
// relative to the workspace (".devcontainer/devcontainer.json"). Empty output
// means no devcontainer config at all.
func ParseReadOutput(stdout string) (map[string][]byte, error) {
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, stdout)
	if clean == "" {
		return map[string][]byte{}, nil
	}
	raw, err := base64.StdEncoding.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("decode .devcontainer listing: %w", err)
	}
	return ReadTar(bytes.NewReader(raw))
}

// ReadTar reads a tar stream into files keyed by cleaned relative path.
// Directories are skipped. Symlinks and other non-regular entries are kept as
// a marker ("\x00symlink -> target"), so a starter that has been replaced by a
// link never hashes like the real starter.
func ReadTar(r io.Reader) (map[string][]byte, error) {
	files := map[string][]byte{}
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read .devcontainer tar: %w", err)
		}
		name := cleanRel(h.Name)
		if name == "." || name == "" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg:
			b, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("read %s: %w", name, err)
			}
			files[name] = b
		case tar.TypeSymlink, tar.TypeLink:
			files[name] = []byte("\x00symlink -> " + h.Linkname)
		default:
			files[name] = []byte(fmt.Sprintf("\x00type %c", h.Typeflag))
		}
	}
}

// Output markers printed last by WriteStarterScript.
const (
	WriteResultWritten = "written"
	WriteResultExists  = "exists"
)

// WriteStarterScript returns a bash script that writes the latest starter into
// <workspace>/.devcontainer/ in the VM, unless the workspace already has a
// devcontainer config (a non-empty .devcontainer/, a .devcontainer symlink or
// file, or a .devcontainer.json).
// The files are staged in a temp folder and moved into place in one rename.
// Its last stdout line is WriteResultWritten or WriteResultExists (see
// StarterWritten).
func WriteStarterScript(workspace string) string {
	s := Latest()
	paths := make([]string, 0, len(s.Files))
	for p := range s.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var b strings.Builder
	b.WriteString("set -eu\n")
	b.WriteString("cd " + shellquote.One(workspace) + "\n")
	b.WriteString(`if [ -e ` + RootFile + ` ] || [ -L ` + Dir + ` ] || { [ -e ` + Dir + ` ] && [ -n "$(ls -A ` + Dir + ` 2>/dev/null || echo x)" ]; }; then echo ` + WriteResultExists + `; exit 0; fi` + "\n")
	b.WriteString(`tmp=$(mktemp -d .devcontainer.tmp.XXXXXX)` + "\n")
	b.WriteString(`trap 'rm -rf "$tmp"' EXIT` + "\n")
	b.WriteString(`chmod 755 "$tmp"` + "\n")
	for _, p := range paths {
		dst := `"$tmp"/` + shellquote.One(p)
		if i := strings.LastIndexByte(p, '/'); i >= 0 {
			b.WriteString(`mkdir -p "$tmp"/` + shellquote.One(p[:i]) + "\n")
		}
		b.WriteString("printf '%s' " + shellquote.One(base64.StdEncoding.EncodeToString(s.Files[p])) +
			" | base64 -d > " + dst + "\n")
		b.WriteString("chmod 644 " + dst + "\n")
	}
	b.WriteString(`if [ -d ` + Dir + ` ]; then rmdir ` + Dir + `; fi` + "\n")
	b.WriteString(`mv "$tmp" ` + Dir + "\n")
	b.WriteString(`trap - EXIT` + "\n")
	b.WriteString("echo " + WriteResultWritten + "\n")
	return b.String()
}

// StarterWritten reports whether WriteStarterScript's stdout says it wrote the
// starter (false: the workspace already had a devcontainer config).
func StarterWritten(stdout string) (bool, error) {
	lines := strings.Fields(stdout)
	if len(lines) == 0 {
		return false, errors.New("starter write: no output")
	}
	switch last := lines[len(lines)-1]; last {
	case WriteResultWritten:
		return true, nil
	case WriteResultExists:
		return false, nil
	default:
		return false, fmt.Errorf("starter write: unexpected output %q", last)
	}
}

// IsStarterUntracked reports whether .devcontainer/ shows up among the
// untracked paths and is a pristine starter, i.e. it does not count as
// uncommitted work for `dev rm` / `dev prune`. untracked are paths relative
// to the repo root as git prints them (git collapses an untracked folder to
// ".devcontainer/"); files is ParseReadOutput's result for the workspace.
func IsStarterUntracked(untracked []string, files map[string][]byte) bool {
	found := false
	for _, u := range untracked {
		if inDir(u) {
			found = true
		}
	}
	if !found {
		return false
	}
	kind, _ := Detect(files)
	return kind == KindStarter
}

// FilterUntracked returns untracked without the .devcontainer/ entries when
// they are a pristine starter (see IsStarterUntracked); otherwise it returns
// untracked unchanged. A non-empty result means real uncommitted work.
func FilterUntracked(untracked []string, files map[string][]byte) []string {
	if !IsStarterUntracked(untracked, files) {
		return untracked
	}
	var rest []string
	for _, u := range untracked {
		if !inDir(u) {
			rest = append(rest, u)
		}
	}
	return rest
}

func inDir(p string) bool {
	p = cleanRel(p)
	return p == Dir || strings.HasPrefix(p, Dir+"/")
}
