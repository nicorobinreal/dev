// Package state holds global config (config.toml), per-project state, file
// locks and the invocation log.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Paths locates everything dev keeps on the Mac. Home is the user's home
// directory; tests point it at a temp dir. DEV_HOME overrides it at runtime.
type Paths struct {
	Home string
}

// DefaultPaths uses $DEV_HOME or the real home directory.
func DefaultPaths() Paths {
	if h := os.Getenv("DEV_HOME"); h != "" {
		return Paths{Home: h}
	}
	h, _ := os.UserHomeDir()
	return Paths{Home: h}
}

func (p Paths) ConfigDir() string       { return filepath.Join(p.Home, ".config", "dev") }
func (p Paths) ConfigFile() string      { return filepath.Join(p.ConfigDir(), "config.toml") }
func (p Paths) SSHConfigFile() string   { return filepath.Join(p.ConfigDir(), "ssh_config") }
func (p Paths) StateDir() string        { return filepath.Join(p.Home, ".local", "state", "dev") }
func (p Paths) ProjectsDir() string     { return filepath.Join(p.StateDir(), "projects") }
func (p Paths) LocksDir() string        { return filepath.Join(p.StateDir(), "locks") }
func (p Paths) LogsDir() string         { return filepath.Join(p.StateDir(), "logs") }
func (p Paths) UpdateFile() string      { return filepath.Join(p.StateDir(), "update.json") }
func (p Paths) BinDir() string          { return filepath.Join(p.Home, ".local", "bin") }
func (p Paths) Binary() string          { return filepath.Join(p.BinDir(), "dev") }
func (p Paths) UserSSHConfig() string   { return filepath.Join(p.Home, ".ssh", "config") }
func (p Paths) LimaDir() string         { return filepath.Join(p.Home, ".lima") }
func (p Paths) LimaKey() string         { return filepath.Join(p.LimaDir(), "_config", "user") }
func (p Paths) LaunchAgentsDir() string { return filepath.Join(p.Home, "Library", "LaunchAgents") }
func (p Paths) ProjectFile(name string) string {
	return filepath.Join(p.ProjectsDir(), name+".json")
}

// Harness IDs.
const (
	HarnessClaude   = "claude"
	HarnessCodex    = "codex"
	HarnessOpenCode = "opencode"
)

// Config is ~/.config/dev/config.toml.
type Config struct {
	Machine      string   `toml:"machine" json:"machine"`
	Harnesses    []string `toml:"harnesses" json:"harnesses"`
	CPUs         int      `toml:"cpus" json:"cpus"`
	Memory       string   `toml:"memory" json:"memory"` // e.g. "8GiB"
	Disk         string   `toml:"disk" json:"disk"`     // e.g. "60GiB"
	TailscaleTag string   `toml:"tailscale_tag" json:"tailscale_tag"`
	Editor       string   `toml:"editor" json:"editor"` // code | cursor | zed
	RAMWarnShare float64  `toml:"ram_warn_share" json:"ram_warn_share"`
	Initialized  bool     `toml:"initialized" json:"initialized"`
}

// DefaultConfig returns the built-in defaults.
func DefaultConfig() Config {
	return Config{
		Machine:      ShortHostname(),
		Harnesses:    []string{HarnessClaude, HarnessCodex, HarnessOpenCode},
		CPUs:         4,
		Memory:       "8GiB",
		Disk:         "60GiB",
		TailscaleTag: "tag:devvm",
		Editor:       "code",
		RAMWarnShare: 0.75,
	}
}

// ShortHostname returns a DNS-label-safe short hostname.
func ShortHostname() string {
	h, _ := os.Hostname()
	h = strings.ToLower(strings.SplitN(h, ".", 2)[0])
	h = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(h, "-")
	h = strings.Trim(h, "-")
	if h == "" || h[0] < 'a' || h[0] > 'z' {
		h = "mac" + h
	}
	return h
}

// LoadConfig reads config.toml, filling unset fields with defaults.
func LoadConfig(p Paths) (Config, error) {
	c := DefaultConfig()
	b, err := os.ReadFile(p.ConfigFile())
	if errors.Is(err, os.ErrNotExist) {
		if m := os.Getenv("DEV_MACHINE"); m != "" {
			c.Machine = m
		}
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if _, err := toml.Decode(string(b), &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", p.ConfigFile(), err)
	}
	d := DefaultConfig()
	if m := os.Getenv("DEV_MACHINE"); m != "" {
		c.Machine = m
	}
	if c.Machine == "" {
		c.Machine = d.Machine
	}
	if c.CPUs == 0 {
		c.CPUs = d.CPUs
	}
	if c.Memory == "" {
		c.Memory = d.Memory
	}
	if c.Disk == "" {
		c.Disk = d.Disk
	}
	if c.TailscaleTag == "" {
		c.TailscaleTag = d.TailscaleTag
	}
	if c.Editor == "" {
		c.Editor = d.Editor
	}
	if c.RAMWarnShare == 0 {
		c.RAMWarnShare = d.RAMWarnShare
	}
	return c, nil
}

// SaveConfig writes config.toml atomically.
func SaveConfig(p Paths, c Config) error {
	var sb strings.Builder
	if err := toml.NewEncoder(&sb).Encode(c); err != nil {
		return err
	}
	return WriteFileAtomic(p.ConfigFile(), []byte(sb.String()), 0o644)
}

// Project is ~/.local/state/dev/projects/<name>.json.
type Project struct {
	Name          string    `json:"name"`
	Repo          string    `json:"repo"`      // https://github.com/owner/repo
	RepoDir       string    `json:"repo_dir"`  // basename of the clone, e.g. "shop"
	Workspace     string    `json:"workspace"` // absolute path in the VM, e.g. /home/u.linux/shop
	Branch        string    `json:"branch,omitempty"`
	LimaInstance  string    `json:"lima_instance"`
	TailscaleHost string    `json:"tailscale_host"`
	BrowserPort   int       `json:"browser_port,omitempty"`
	CPUs          int       `json:"cpus"`
	Memory        string    `json:"memory"`
	Disk          string    `json:"disk"`
	Editor        string    `json:"editor,omitempty"`
	Devcontainer  string    `json:"devcontainer,omitempty"` // starter-vN | custom
	Served        []int     `json:"served,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	LastUsedAt    time.Time `json:"last_used_at"`
}

// ErrNoProject is returned when a project has no state file.
var ErrNoProject = errors.New("no such project")

func LoadProject(p Paths, name string) (Project, error) {
	var pr Project
	b, err := os.ReadFile(p.ProjectFile(name))
	if errors.Is(err, os.ErrNotExist) {
		return pr, fmt.Errorf("%w: %s", ErrNoProject, name)
	}
	if err != nil {
		return pr, err
	}
	err = json.Unmarshal(b, &pr)
	return pr, err
}

func SaveProject(p Paths, pr Project) error {
	b, err := json.MarshalIndent(pr, "", "  ")
	if err != nil {
		return err
	}
	return WriteFileAtomic(p.ProjectFile(pr.Name), append(b, '\n'), 0o644)
}

func DeleteProject(p Paths, name string) error {
	err := os.Remove(p.ProjectFile(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// ListProjects returns every project, sorted by name.
func ListProjects(p Paths) ([]Project, error) {
	ents, err := os.ReadDir(p.ProjectsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Project
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		pr, err := LoadProject(p, strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ValidateName checks a project name: lowercase letters, digits and '-',
// starting with a letter, and <machine>-<name> at most 63 characters.
func ValidateName(machine, name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid name %q: use lowercase letters, digits and '-', starting with a letter", name)
	}
	if strings.HasSuffix(name, "-") {
		return fmt.Errorf("invalid name %q: must not end with '-'", name)
	}
	if n := len(TailscaleHost(machine, name)); n > 63 {
		return fmt.Errorf("invalid name %q: %s is %d characters (max 63)", name, TailscaleHost(machine, name), n)
	}
	return nil
}

// ValidateMachine checks a machine prefix: the same characters as a project
// name, leaving room for -<name> in a 63-character DNS label.
func ValidateMachine(machine string) error {
	if !nameRE.MatchString(machine) || strings.HasSuffix(machine, "-") {
		return fmt.Errorf("invalid machine prefix %q: use lowercase letters, digits and '-', starting with a letter", machine)
	}
	if len(machine) > 40 {
		return fmt.Errorf("invalid machine prefix %q: at most 40 characters, to leave room for project names", machine)
	}
	return nil
}

// TailscaleHost is <machine>-<name>.
func TailscaleHost(machine, name string) string { return machine + "-" + name }

// WriteFileAtomic writes via temp file + rename, creating parent dirs.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
