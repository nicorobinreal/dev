package lima

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/nicorobinreal/dev/internal/run"
)

// Instance statuses reported by `limactl list`.
const (
	StatusRunning = "Running"
	StatusStopped = "Stopped"
	StatusBroken  = "Broken"
)

// Instance is one entry of `limactl list --json`. Memory and Disk are bytes.
type Instance struct {
	Name          string `json:"name"`
	Hostname      string `json:"hostname"`
	Status        string `json:"status"`
	Dir           string `json:"dir"`
	VMType        string `json:"vmType"`
	Arch          string `json:"arch"`
	CPUs          int    `json:"cpus"`
	Memory        int64  `json:"memory"`
	Disk          int64  `json:"disk"`
	Message       string `json:"message,omitempty"`
	SSHLocalPort  int    `json:"sshLocalPort,omitempty"`
	SSHConfigFile string `json:"sshConfigFile,omitempty"`
	HostAgentPID  int    `json:"hostAgentPID,omitempty"`
	DriverPID     int    `json:"driverPID,omitempty"`
	LimaVersion   string `json:"limaVersion,omitempty"`
	Protected     bool   `json:"protected,omitempty"`
}

// Running reports whether the instance is up.
func (i Instance) Running() bool { return i.Status == StatusRunning }

// Client runs limactl through a run.Runner.
type Client struct {
	Runner run.Runner
	// Limactl is the binary to run; empty means "limactl" from PATH.
	Limactl string
	// ReadFile reads host files (the instance's ssh.config); nil means
	// os.ReadFile. Tests replace it.
	ReadFile func(string) ([]byte, error)
}

func (c *Client) bin() string {
	if c.Limactl != "" {
		return c.Limactl
	}
	return "limactl"
}

func (c *Client) run(ctx context.Context, cmd run.Cmd) (run.Result, error) {
	if cmd.Name == "" {
		cmd.Name = c.bin()
	}
	return c.Runner.Run(ctx, cmd)
}

func (c *Client) limactl(ctx context.Context, args ...string) (run.Result, error) {
	return c.run(ctx, run.Cmd{Args: args})
}

// Create creates (but does not start) instance name from a generated config.
// The YAML is passed on stdin (`limactl create ... -`), which limactl supports
// when --name is given, so there is no temp file to clean up.
func (c *Client) Create(ctx context.Context, name string, yaml []byte) error {
	_, err := c.run(ctx, run.Cmd{
		Args:  []string{"create", "--name", name, "--tty=false", "-"},
		Stdin: bytes.NewReader(yaml),
	})
	return err
}

// Start boots the instance and waits until it is running.
func (c *Client) Start(ctx context.Context, name string) error {
	_, err := c.limactl(ctx, "start", "--tty=false", name)
	return err
}

// Stop shuts the instance down.
func (c *Client) Stop(ctx context.Context, name string) error {
	_, err := c.limactl(ctx, "stop", "--tty=false", name)
	return err
}

// Delete removes the instance. force also kills a running instance.
func (c *Client) Delete(ctx context.Context, name string, force bool) error {
	args := []string{"delete", "--tty=false"}
	if force {
		args = append(args, "--force")
	}
	_, err := c.limactl(ctx, append(args, name)...)
	return err
}

// List returns every Lima instance (not only dev's).
func (c *Client) List(ctx context.Context) ([]Instance, error) {
	res, err := c.limactl(ctx, "list", "--json")
	if err != nil {
		return nil, err
	}
	return parseList(res.Stdout)
}

// parseList parses `limactl list --json`: one JSON object per line. With no
// instances limactl prints nothing on stdout (only a warning on stderr).
func parseList(out string) ([]Instance, error) {
	var list []Instance
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024) // lines include the full config
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var in Instance
		if err := json.Unmarshal([]byte(line), &in); err != nil {
			return nil, fmt.Errorf("parse limactl list output: %w", err)
		}
		list = append(list, in)
	}
	return list, sc.Err()
}

// Get returns the named instance; ok is false if it does not exist.
func (c *Client) Get(ctx context.Context, name string) (Instance, bool, error) {
	list, err := c.List(ctx)
	if err != nil {
		return Instance{}, false, err
	}
	for _, in := range list {
		if in.Name == name {
			return in, true, nil
		}
	}
	return Instance{}, false, nil
}

// ShellOpt adjusts a Shell call.
type ShellOpt func(*shellOpts)

type shellOpts struct {
	stdin   io.Reader
	workdir string
}

// WithStdin feeds r to the script's stdin. Use it for secrets: stdin is never
// logged, argv is.
func WithStdin(r io.Reader) ShellOpt { return func(o *shellOpts) { o.stdin = r } }

// WithWorkdir sets the working directory in the VM (default "/").
func WithWorkdir(dir string) ShellOpt { return func(o *shellOpts) { o.workdir = dir } }

// Shell runs a bash script in the VM:
//
//	limactl shell --workdir / <name> -- bash -c <script>
//
// The script travels in argv (limactl shell-quotes it for ssh), so it shows up
// in dev's log; never embed secrets in it, pass them with WithStdin instead.
// A non-zero exit returns *run.ExitError along with the Result.
func (c *Client) Shell(ctx context.Context, name, script string, opts ...ShellOpt) (run.Result, error) {
	o := shellOpts{workdir: "/"}
	for _, f := range opts {
		f(&o)
	}
	return c.run(ctx, run.Cmd{
		Args:  []string{"shell", "--workdir", o.workdir, name, "--", "bash", "-c", script},
		Stdin: o.stdin,
	})
}

// Exec runs args interactively in the VM, attached to the terminal (for
// `dev exec --vm`). An empty workdir lets Lima pick (falls back to $HOME).
func (c *Client) Exec(ctx context.Context, name, workdir string, args ...string) (run.Result, error) {
	a := []string{"shell"}
	if workdir != "" {
		a = append(a, "--workdir", workdir)
	}
	a = append(a, name)
	if len(args) > 0 {
		a = append(a, "--")
		a = append(a, args...)
	}
	return c.run(ctx, run.Cmd{Args: a, Interactive: true})
}

// Home returns $HOME of the Lima user inside the VM (typically
// /home/<user>.linux). It is read on every call, never cached.
func (c *Client) Home(ctx context.Context, name string) (string, error) {
	res, err := c.Shell(ctx, name, `printf '%s' "$HOME"`)
	if err != nil {
		return "", err
	}
	h := strings.TrimSpace(res.Stdout)
	if !strings.HasPrefix(h, "/") {
		return "", fmt.Errorf("lima: unexpected $HOME %q in %s", h, name)
	}
	return h, nil
}

// ShowSSH returns the instance's SSH client config (a `Host lima-<name>`
// block). `limactl show-ssh` is deprecated in Lima 2.x in favour of the
// ssh.config file Lima keeps in the instance directory, so that file is read
// first; `limactl show-ssh --format config` is the fallback.
func (c *Client) ShowSSH(ctx context.Context, name string) (string, error) {
	if in, ok, err := c.Get(ctx, name); err == nil && ok {
		path := sshConfigPath(in)
		read := c.ReadFile
		if read == nil {
			read = os.ReadFile
		}
		if b, err := read(path); err == nil && len(bytes.TrimSpace(b)) > 0 {
			return string(b), nil
		}
	}
	res, err := c.limactl(ctx, "show-ssh", "--format", "config", name)
	if err != nil {
		return "", err
	}
	return res.Stdout, nil
}

func sshConfigPath(in Instance) string {
	if in.SSHConfigFile != "" {
		return in.SSHConfigFile
	}
	return filepath.Join(in.Dir, "ssh.config")
}

// ResetSSH closes Lima's shared SSH master connection (ControlMaster) for the
// instance, so the next `limactl shell` opens a fresh login session. Needed
// after group changes such as adding the user to "docker": sessions
// multiplexed over the old master keep the old supplementary groups. Errors
// from ssh (typically "no master running") are ignored.
func (c *Client) ResetSSH(ctx context.Context, name string) error {
	in, ok, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("lima: no instance %q", name)
	}
	_, _ = c.run(ctx, run.Cmd{Name: "ssh", Args: []string{"-F", sshConfigPath(in), "-O", "exit", "lima-" + name}})
	return nil
}

// Usage is the resource use inside a running VM, in bytes. Best effort.
type Usage struct {
	CPUs      int     `json:"cpus"`
	Load1     float64 `json:"load1"`
	Load5     float64 `json:"load5"`
	Load15    float64 `json:"load15"`
	MemTotal  int64   `json:"mem_total"`
	MemUsed   int64   `json:"mem_used"` // MemTotal - MemAvailable
	DiskTotal int64   `json:"disk_total"`
	DiskUsed  int64   `json:"disk_used"` // root filesystem
}

const usageScript = `echo "cpus $(nproc)"
read -r l1 l5 l15 _ < /proc/loadavg; echo "load $l1 $l5 $l15"
awk '/^MemTotal:/{t=$2} /^MemAvailable:/{a=$2} END{printf "mem %.0f %.0f\n", t*1024, (t-a)*1024}' /proc/meminfo
df -B1 --output=size,used / | awk 'NR==2{print "disk", $1, $2}'
`

// Usage reads CPU count, load average, memory and root disk use from the VM.
// Fields it cannot read stay zero; an error is returned only if the shell
// itself failed.
func (c *Client) Usage(ctx context.Context, name string) (Usage, error) {
	res, err := c.Shell(ctx, name, usageScript)
	if err != nil && res.Stdout == "" {
		return Usage{}, err
	}
	return parseUsage(res.Stdout), nil
}

func parseUsage(out string) Usage {
	var u Usage
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		i := func(k int) int64 {
			if k >= len(f) {
				return 0
			}
			n, _ := strconv.ParseFloat(f[k], 64) // awk may print 1.6e+10
			return int64(n)
		}
		fl := func(k int) float64 {
			if k >= len(f) {
				return 0
			}
			n, _ := strconv.ParseFloat(f[k], 64)
			return n
		}
		switch f[0] {
		case "cpus":
			u.CPUs = int(i(1))
		case "load":
			u.Load1, u.Load5, u.Load15 = fl(1), fl(2), fl(3)
		case "mem":
			u.MemTotal, u.MemUsed = i(1), i(2)
		case "disk":
			u.DiskTotal, u.DiskUsed = i(1), i(2)
		}
	}
	return u
}

// VM binds a Client to one instance. It satisfies the small Shell interfaces
// of internal/provision and internal/gh.
type VM struct {
	Client *Client
	Name   string
}

// VM returns a handle for instance name.
func (c *Client) VM(name string) VM { return VM{Client: c, Name: name} }

// Shell runs script in the VM with stdin (may be nil).
func (v VM) Shell(ctx context.Context, script string, stdin io.Reader) (run.Result, error) {
	var opts []ShellOpt
	if stdin != nil {
		opts = append(opts, WithStdin(stdin))
	}
	return v.Client.Shell(ctx, v.Name, script, opts...)
}

// IsNotFound reports whether err looks like limactl complaining about an
// unknown instance (useful for idempotent delete/stop).
func IsNotFound(err error) bool {
	var ee *run.ExitError
	if !errors.As(err, &ee) {
		return false
	}
	msg := ee.Result.Stderr + ee.Result.Stdout
	return strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "No instance matching") ||
		strings.Contains(msg, "no such file or directory") // `limactl stop` on a missing instance
}
