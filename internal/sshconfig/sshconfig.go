// Package sshconfig manages dev's SSH configuration: the dev-owned file
// ~/.config/dev/ssh_config with a `lima-<name>` and a `<name>-dev` host per
// project, and the single Include line that pulls it into ~/.ssh/config.
//
// Callers must hold state.LockGlobal while calling Write, EnsureInclude,
// RemoveInclude or ForgetHostKey (this package does not lock, so it composes
// with callers that already hold the global lock).
package sshconfig

import (
	"bufio"
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/nicorobinreal/dev/internal/state"
)

// ContainerPort is where the devcontainer's sshd listens on the VM.
const ContainerPort = 2222

// Header starts the generated file.
const Header = "# managed by dev — do not edit; regenerate with `dev ssh-config`\n"

// VMHost is the SSH host alias for the project's VM.
func VMHost(name string) string { return "lima-" + name }

// ContainerHost is the SSH host alias for the project's devcontainer sshd.
func ContainerHost(name string) string { return name + "-dev" }

// KnownHostsFile is the dev-owned known_hosts for `<name>-dev` hosts.
// Container host keys are persisted across rebuilds, so accept-new is safe.
func KnownHostsFile(p state.Paths) string { return filepath.Join(p.ConfigDir(), "known_hosts") }

// LimaSSHConfigPath is the SSH config Lima writes for an instance.
func LimaSSHConfigPath(p state.Paths, instance string) string {
	return filepath.Join(p.LimaDir(), instance, "ssh.config")
}

// LoadLimaConfig reads LimaSSHConfigPath. A missing file returns "" and no
// error (Render then emits a placeholder comment for that project).
func LoadLimaConfig(p state.Paths, instance string) (string, error) {
	b, err := os.ReadFile(LimaSSHConfigPath(p, instance))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// Entry is one project's pair of hosts.
type Entry struct {
	Name string // project name (also the Lima instance name)
	// LimaConfig is Lima's generated SSH config for the instance
	// (~/.lima/<name>/ssh.config or `limactl show-ssh --format=config`).
	// Its first Host block's options are kept; the Host line is rewritten.
	LimaConfig     string
	IdentityFile   string // Lima's key, ~/.lima/_config/user
	KnownHostsFile string // dev-owned known_hosts for <name>-dev
}

// NewEntry fills IdentityFile and KnownHostsFile from p.
func NewEntry(p state.Paths, name, limaConfig string) Entry {
	return Entry{Name: name, LimaConfig: limaConfig, IdentityFile: p.LimaKey(), KnownHostsFile: KnownHostsFile(p)}
}

// LimaOptions returns the option lines (trimmed, in order) of the first Host
// block in Lima's SSH config. Comments, blank lines, and any Host/Match lines
// are dropped. If there is no Host line, every option line is taken.
func LimaOptions(raw string) []string {
	var opts []string
	inHost, sawHost := false, false
	for _, line := range strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		switch strings.ToLower(keyword(t)) {
		case "host", "match":
			if sawHost {
				return opts
			}
			sawHost, inHost = true, true
			opts = opts[:0] // drop anything that preceded the Host line
			continue
		}
		if inHost || !sawHost {
			opts = append(opts, t)
		}
	}
	return opts
}

// keyword returns the first token of an ssh_config line ("Key value" or
// "Key=value").
func keyword(line string) string {
	if i := strings.IndexAny(line, " \t="); i >= 0 {
		return line[:i]
	}
	return line
}

// quote double-quotes v if it contains whitespace.
func quote(v string) string {
	if strings.ContainsAny(v, " \t") {
		return `"` + v + `"`
	}
	return v
}

// Render returns the full ssh_config text: a header, then per project
// (sorted by name) `Host lima-<name>` and `Host <name>-dev`.
func Render(entries []Entry) []byte {
	es := append([]Entry(nil), entries...)
	sort.Slice(es, func(i, j int) bool { return es[i].Name < es[j].Name })
	var b bytes.Buffer
	b.WriteString(Header)
	for _, e := range es {
		b.WriteString("\n")
		opts := LimaOptions(e.LimaConfig)
		if len(opts) == 0 {
			fmt.Fprintf(&b, "# %s: no Lima SSH config yet (start the VM, then run `dev ssh-config`)\n", e.Name)
			continue
		}
		fmt.Fprintf(&b, "Host %s\n", VMHost(e.Name))
		for _, o := range opts {
			fmt.Fprintf(&b, "  %s\n", o)
		}
		b.WriteString("\n")
		fmt.Fprintf(&b, "Host %s\n", ContainerHost(e.Name))
		fmt.Fprintf(&b, "  HostName 127.0.0.1\n")
		fmt.Fprintf(&b, "  Port %d\n", ContainerPort)
		fmt.Fprintf(&b, "  User root\n")
		fmt.Fprintf(&b, "  ProxyJump %s\n", VMHost(e.Name))
		if e.IdentityFile != "" {
			fmt.Fprintf(&b, "  IdentityFile %s\n", quote(e.IdentityFile))
		}
		fmt.Fprintf(&b, "  IdentitiesOnly yes\n")
		// Every container is 127.0.0.1:2222 behind its own jump host, so key
		// known_hosts by alias, not by address.
		fmt.Fprintf(&b, "  HostKeyAlias %s\n", ContainerHost(e.Name))
		fmt.Fprintf(&b, "  StrictHostKeyChecking accept-new\n")
		if e.KnownHostsFile != "" {
			fmt.Fprintf(&b, "  UserKnownHostsFile %s\n", quote(e.KnownHostsFile))
		}
	}
	return b.Bytes()
}

// Write renders entries into p.SSHConfigFile() atomically (mode 0600).
func Write(p state.Paths, entries []Entry) error {
	return state.WriteFileAtomic(p.SSHConfigFile(), Render(entries), 0o600)
}

// IncludeMarker is the comment dev puts above its Include line.
const IncludeMarker = "# Added by dev: keep this Include above all Host blocks."

// IncludePath is the path dev writes in its Include line: the literal
// "~/.config/dev/ssh_config" when p.Home is the user's real home, else the
// absolute path (DEV_HOME / tests), since ssh expands ~ to the real home.
func IncludePath(p state.Paths) string {
	if h, err := os.UserHomeDir(); err == nil && filepath.Clean(h) == filepath.Clean(p.Home) {
		return "~/.config/dev/ssh_config"
	}
	return p.SSHConfigFile()
}

// IncludeLine is the full line dev owns in ~/.ssh/config.
func IncludeLine(p state.Paths) string { return "Include " + IncludePath(p) }

// isOurInclude reports whether line is an Include of exactly dev's file.
func isOurInclude(p state.Paths, line string) bool {
	t := strings.TrimSpace(line)
	kw := keyword(t)
	if !strings.EqualFold(kw, "include") {
		return false
	}
	arg := strings.TrimSpace(strings.TrimLeft(t[len(kw):], " \t="))
	arg = strings.Trim(arg, `"`)
	if arg == "" || strings.ContainsAny(arg, " \t") {
		return false
	}
	return arg == "~/.config/dev/ssh_config" || arg == p.SSHConfigFile() ||
		arg == "$HOME/.config/dev/ssh_config" || arg == "${HOME}/.config/dev/ssh_config"
}

func isBlockStart(line string) bool {
	switch strings.ToLower(keyword(strings.TrimSpace(line))) {
	case "host", "match":
		return true
	}
	return false
}

// userConfigTarget resolves ~/.ssh/config through a symlink (dotfile
// managers) so the atomic rename replaces the target, not the link.
func userConfigTarget(p state.Paths) string {
	path := p.UserSSHConfig()
	if t, err := filepath.EvalSymlinks(path); err == nil {
		return t
	}
	return path
}

func readUserConfig(p state.Paths) (text string, exists bool, mode os.FileMode, err error) {
	path := userConfigTarget(p)
	st, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, 0o600, nil
	}
	if err != nil {
		return "", false, 0, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false, 0, err
	}
	return string(b), true, st.Mode().Perm(), nil
}

// splitLines splits text into lines without terminators and reports the
// line ending in use ("\r\n" if the file uses it, else "\n").
func splitLines(text string) ([]string, string) {
	eol := "\n"
	if strings.Contains(text, "\r\n") {
		eol = "\r\n"
	}
	if text == "" {
		return nil, eol
	}
	text = strings.TrimSuffix(text, eol)
	return strings.Split(text, eol), eol
}

// HasInclude reports whether ~/.ssh/config includes dev's file at top level
// (before any Host or Match block, so it applies to every host).
func HasInclude(p state.Paths) (bool, error) {
	text, _, _, err := readUserConfig(p)
	if err != nil {
		return false, err
	}
	lines, _ := splitLines(text)
	return includeOK(p, lines), nil
}

// includeOK: exactly one of our Include lines, before any Host/Match.
func includeOK(p state.Paths, lines []string) bool {
	n, ok := 0, false
	inBlock := false
	for _, l := range lines {
		if isBlockStart(l) {
			inBlock = true
		}
		if isOurInclude(p, l) {
			n++
			ok = !inBlock
		}
	}
	return n == 1 && ok
}

// EnsureInclude makes ~/.ssh/config include dev's file, inserting the
// Include at the top (it must precede Host blocks to apply globally). It
// creates ~/.ssh (0700) and the file (0600) if needed, removes duplicate or
// misplaced copies of the line, backs the original up to
// ~/.ssh/config.dev-backup the first time it edits, and is idempotent. It
// reports whether the file changed.
func EnsureInclude(p state.Paths) (bool, error) {
	text, exists, mode, err := readUserConfig(p)
	if err != nil {
		return false, err
	}
	lines, eol := splitLines(text)
	if includeOK(p, lines) {
		return false, nil
	}
	rest := removeOurLines(p, lines)
	for len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	out := []string{IncludeMarker, IncludeLine(p)}
	if len(rest) > 0 {
		out = append(out, "")
		out = append(out, rest...)
	}
	if err := os.MkdirAll(filepath.Dir(p.UserSSHConfig()), 0o700); err != nil {
		return false, err
	}
	if exists && text != "" {
		if err := backupOnce(p, text, mode); err != nil {
			return false, err
		}
	}
	data := strings.Join(out, eol) + eol
	if err := state.WriteFileAtomic(userConfigTarget(p), []byte(data), mode); err != nil {
		return false, err
	}
	return true, nil
}

// removeOurLines drops dev's Include lines and the marker comment above them.
func removeOurLines(p state.Paths, lines []string) []string {
	var out []string
	for _, l := range lines {
		if isOurInclude(p, l) {
			if n := len(out); n > 0 && strings.TrimSpace(out[n-1]) == IncludeMarker {
				out = out[:n-1]
			}
			continue
		}
		out = append(out, l)
	}
	// A marker left without its Include.
	cleaned := out[:0]
	for _, l := range out {
		if strings.TrimSpace(l) != IncludeMarker {
			cleaned = append(cleaned, l)
		}
	}
	for len(cleaned) > 0 && strings.TrimSpace(cleaned[len(cleaned)-1]) == "" {
		cleaned = cleaned[:len(cleaned)-1]
	}
	return cleaned
}

// BackupPath is where EnsureInclude saves the original ~/.ssh/config.
func BackupPath(p state.Paths) string { return p.UserSSHConfig() + ".dev-backup" }

func backupOnce(p state.Paths, text string, mode os.FileMode) error {
	if _, err := os.Stat(BackupPath(p)); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return state.WriteFileAtomic(BackupPath(p), []byte(text), mode)
}

// RemoveInclude removes dev's Include line (and its marker comment and the
// blank line dev added after it) from ~/.ssh/config, for self-uninstall. The
// backup is left alone. It reports whether the file changed.
func RemoveInclude(p state.Paths) (bool, error) {
	text, exists, mode, err := readUserConfig(p)
	if err != nil || !exists {
		return false, err
	}
	lines, eol := splitLines(text)
	found := false
	for _, l := range lines {
		if isOurInclude(p, l) {
			found = true
			break
		}
	}
	if !found {
		return false, nil
	}
	// If the file starts with our block, drop the blank separator after it.
	topBlock := len(lines) >= 2 && strings.TrimSpace(lines[0]) == IncludeMarker && isOurInclude(p, lines[1])
	rest := removeOurLines(p, lines)
	if topBlock && len(rest) > 0 && strings.TrimSpace(rest[0]) == "" {
		rest = rest[1:]
	}
	data := ""
	if len(rest) > 0 {
		data = strings.Join(rest, eol) + eol
	}
	if err := state.WriteFileAtomic(userConfigTarget(p), []byte(data), mode); err != nil {
		return false, err
	}
	return true, nil
}

// ForgetHostKey removes `<name>-dev` from dev's known_hosts (plain or hashed
// entries), so a recreated project with new container host keys is accepted
// again. Call it from `dev rm`. A missing file is not an error.
func ForgetHostKey(p state.Paths, name string) error {
	path := KnownHostsFile(p)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	alias := ContainerHost(name)
	var out bytes.Buffer
	changed := false
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if knownHostsMatches(line, alias) {
			changed = true
			continue
		}
		out.WriteString(line)
		out.WriteByte('\n')
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if !changed {
		return nil
	}
	return state.WriteFileAtomic(path, out.Bytes(), 0o600)
}

func knownHostsMatches(line, alias string) bool {
	f := strings.Fields(line)
	if len(f) < 2 || strings.HasPrefix(f[0], "#") {
		return false
	}
	hosts := f[0]
	if strings.HasPrefix(hosts, "@") && len(f) >= 3 { // @cert-authority / @revoked
		hosts = f[1]
	}
	candidates := []string{alias, "[" + alias + "]:" + strconv.Itoa(ContainerPort)}
	for _, h := range strings.Split(hosts, ",") {
		for _, c := range candidates {
			if h == c || hashedMatch(h, c) {
				return true
			}
		}
	}
	return false
}

// hashedMatch checks an OpenSSH hashed host (|1|salt|hash, HMAC-SHA1).
func hashedMatch(h, host string) bool {
	parts := strings.Split(h, "|")
	if len(parts) != 4 || parts[0] != "" || parts[1] != "1" {
		return false
	}
	salt, err1 := base64.StdEncoding.DecodeString(parts[2])
	want, err2 := base64.StdEncoding.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	m := hmac.New(sha1.New, salt)
	m.Write([]byte(host))
	return hmac.Equal(m.Sum(nil), want)
}
