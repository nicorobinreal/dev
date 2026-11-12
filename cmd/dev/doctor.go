package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/nicorobinreal/dev/internal/run"
	"github.com/nicorobinreal/dev/internal/sshconfig"
	"github.com/nicorobinreal/dev/internal/state"
	"github.com/nicorobinreal/dev/internal/tailscale"
)

func init() { register(newDoctorCmd) }

// Check is one doctor result.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Warn   bool   `json:"warn,omitempty"` // failure is only a warning
	Detail string `json:"detail,omitempty"`
	Fix    string `json:"fix,omitempty"` // human hint
	Brew   string `json:"-"`             // brew install args for --fix
}

func newDoctorCmd(a *App) *cobra.Command {
	var fix, setupTS bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check host prerequisites, offer to install missing ones, guide the Tailscale setup",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			checks := a.doctorChecks(ctx)
			if fix {
				for _, c := range checks {
					if c.OK || c.Brew == "" {
						continue
					}
					a.Notef("installing %s with Homebrew…\n", c.Name)
					if _, err := a.Runner.Run(ctx, run.Cmd{Name: "brew", Args: strings.Fields(c.Brew), Interactive: !a.JSON}); err != nil {
						a.Notef("  failed: %v\n", err)
					}
				}
				if !a.hasInclude() {
					if err := a.ensureSSHInclude(); err != nil {
						a.Notef("  ssh include: %v\n", err)
					}
				}
				checks = a.doctorChecks(ctx)
			}
			tsMissing := false
			for _, c := range checks {
				if c.Name == "tailscale-oauth" && !c.OK {
					tsMissing = true
				}
			}
			// Guided Tailscale setup is the one interactive flow (a person runs it).
			if tsMissing && !a.JSON && (setupTS || isTTY()) {
				if err := a.tailscaleSetup(ctx); err != nil {
					return err
				}
				checks = a.doctorChecks(ctx)
			}
			failed := failedChecks(checks)
			err := a.Emit(map[string]any{"checks": checks, "ok": failed == 0}, func() {
				for _, c := range checks {
					mark := "✓"
					if !c.OK {
						mark = "✗"
						if c.Warn {
							mark = "!"
						}
					}
					line := fmt.Sprintf("%s %-20s %s", mark, c.Name, c.Detail)
					a.Printf("%s\n", strings.TrimRight(line, " "))
					if !c.OK && c.Fix != "" {
						a.Printf("    → %s\n", c.Fix)
					}
				}
			})
			if err != nil {
				return err
			}
			if failed > 0 {
				if a.JSON {
					return &exitStatusError{code: ExitError} // details are in the JSON
				}
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "install what Homebrew can install and add the SSH Include")
	cmd.Flags().BoolVar(&setupTS, "setup-tailscale", false, "run the guided Tailscale setup even without a TTY check")
	_ = cmd.Flags().MarkHidden("setup-tailscale")
	return cmd
}

func failedChecks(checks []Check) int {
	n := 0
	for _, c := range checks {
		if !c.OK && !c.Warn {
			n++
		}
	}
	return n
}

func isTTY() bool {
	st, err := os.Stdin.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (a *App) hasInclude() bool {
	ok, _ := sshconfig.HasInclude(a.Paths)
	return ok
}

func (a *App) ensureSSHInclude() error {
	l, err := state.LockGlobal(a.Paths)
	if err != nil {
		return err
	}
	defer l.Unlock()
	_, err = sshconfig.EnsureInclude(a.Paths)
	return err
}

var verRE = regexp.MustCompile(`(\d+)\.(\d+)(?:\.(\d+))?`)

func majorMinor(s string) (int, int) {
	m := verRE.FindStringSubmatch(s)
	if m == nil {
		return 0, 0
	}
	maj, _ := strconv.Atoi(m[1])
	mi, _ := strconv.Atoi(m[2])
	return maj, mi
}

func (a *App) doctorChecks(ctx context.Context) []Check {
	var out []Check
	have := func(bin string) (string, bool) {
		p, err := run.Output(ctx, a.Runner, "which", bin)
		return p, err == nil && p != ""
	}

	// Lima, recent (vz + virtiofs need ≥ 1.0; we test against 2.x).
	c := Check{Name: "lima", Brew: "install lima", Fix: "brew install lima"}
	if _, ok := have("limactl"); ok {
		v, _ := run.Output(ctx, a.Runner, "limactl", "--version")
		maj, _ := majorMinor(v)
		c.Detail = v
		c.OK = maj >= 1
		if !c.OK {
			c.Fix = "brew upgrade lima (need ≥ 1.0)"
			c.Brew = "upgrade lima"
		}
	} else {
		c.Detail = "limactl not found"
	}
	out = append(out, c)

	// Rosetta (warn only).
	c = Check{Name: "rosetta", Warn: true, Fix: "softwareupdate --install-rosetta --agree-to-license (lets VMs run amd64 images fast)"}
	if _, err := os.Stat("/Library/Apple/usr/libexec/oah/libRosettaRuntime"); err == nil {
		c.OK, c.Detail = true, "installed"
	} else {
		c.Detail = "not installed"
	}
	out = append(out, c)

	// Tailscale CLI and login.
	c = Check{Name: "tailscale", Fix: "install Tailscale (brew install --cask tailscale-app) and log in; the tailscale CLI must be on PATH"}
	if _, ok := have("tailscale"); ok {
		if st, err := tailscale.LocalStatus(ctx, a.Runner); err == nil && st.LoggedIn() {
			c.OK = true
			c.Detail = "logged in, tailnet " + st.MagicDNSSuffix
			if !st.MagicDNSEnabled {
				c.OK = false
				c.Detail = "MagicDNS is off"
				c.Fix = "turn on MagicDNS in the Tailscale admin console (DNS page)"
			}
		} else {
			c.Detail = "not logged in"
		}
	} else {
		c.Detail = "tailscale CLI not found"
	}
	out = append(out, c)

	// Tailscale OAuth client in Keychain and able to mint a token.
	c = Check{Name: "tailscale-oauth", Fix: "run `dev doctor` in a terminal for the guided setup"}
	kc := tailscale.Keychain{R: a.Runner}
	if ok, _ := kc.Has(ctx); ok {
		if err := a.tsClient().CheckToken(ctx); err == nil {
			c.OK, c.Detail = true, "Keychain credentials mint an API token"
		} else {
			c.Detail = "cannot mint API token: " + err.Error()
		}
	} else {
		c.Detail = "no OAuth client in Keychain (service dev-tailscale)"
	}
	out = append(out, c)

	// gh logged in.
	c = Check{Name: "gh", Brew: "install gh", Fix: "brew install gh && gh auth login"}
	if _, ok := have("gh"); ok {
		if _, err := run.Output(ctx, a.Runner, "gh", "auth", "token"); err == nil {
			c.OK, c.Detail = true, "logged in"
		} else {
			c.Detail, c.Fix, c.Brew = "not logged in", "gh auth login", ""
		}
	} else {
		c.Detail = "gh not found"
	}
	out = append(out, c)

	// Node on the Mac (npx chrome-devtools-mcp).
	c = Check{Name: "node", Brew: "install node", Fix: "brew install node (needed for browser access)"}
	if v, err := run.Output(ctx, a.Runner, "node", "--version"); err == nil {
		c.OK, c.Detail = true, v
	} else {
		c.Detail = "node not found"
	}
	out = append(out, c)

	// VS Code + Remote-SSH + Dev Containers.
	c = Check{Name: "vscode", Warn: true, Brew: "install --cask visual-studio-code", Fix: "install VS Code with the Remote-SSH and Dev Containers extensions"}
	if _, ok := have("code"); ok {
		exts, _ := run.Output(ctx, a.Runner, "code", "--list-extensions")
		var missing []string
		for _, e := range []string{"ms-vscode-remote.remote-ssh", "ms-vscode-remote.remote-containers"} {
			if !strings.Contains(strings.ToLower(exts), e) {
				missing = append(missing, e)
			}
		}
		c.Brew = ""
		if len(missing) == 0 {
			c.OK, c.Detail = true, "Remote-SSH and Dev Containers installed"
		} else {
			c.Detail = "missing extensions: " + strings.Join(missing, ", ")
			c.Fix = "code --install-extension " + strings.Join(missing, " --install-extension ")
		}
	} else {
		c.Detail = "code CLI not found"
	}
	out = append(out, c)

	// Chrome 144+ (warn only).
	c = Check{Name: "chrome", Warn: true, Fix: "install Chrome 144+ and enable remote debugging at chrome://inspect/#remote-debugging"}
	if v := chromeVersion(); v != "" {
		maj, _ := majorMinor(v)
		c.Detail = "version " + v
		c.OK = maj >= 144
	} else {
		c.Detail = "not found"
	}
	out = append(out, c)

	// ~/.local/bin on PATH.
	c = Check{Name: "path", Fix: "add ~/.local/bin to PATH in your shell profile"}
	bin := a.Paths.BinDir()
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if filepath.Clean(d) == bin {
			c.OK = true
		}
	}
	c.Detail = bin
	out = append(out, c)

	// SSH Include.
	c = Check{Name: "ssh-include", Fix: "dev doctor --fix (adds Include ~/.config/dev/ssh_config to ~/.ssh/config)"}
	c.OK = a.hasInclude()
	c.Detail = a.Paths.UserSSHConfig()
	out = append(out, c)
	return out
}

func chromeVersion() string {
	b, err := os.ReadFile("/Applications/Google Chrome.app/Contents/Info.plist")
	if err != nil {
		return ""
	}
	m := regexp.MustCompile(`<key>CFBundleShortVersionString</key>\s*<string>([^<]+)</string>`).FindSubmatch(b)
	if m == nil {
		return ""
	}
	return string(m[1])
}

var tsClientCache *tailscale.Client

// tsClient returns the process-wide Tailscale API client (token cached).
func (a *App) tsClient() *tailscale.Client {
	if tsClientCache == nil {
		tsClientCache = tailscale.NewClient(tailscale.Keychain{R: a.Runner}.Credentials)
	}
	return tsClientCache
}

// tailscaleSetup prints the one-time guide and stores the OAuth client.
func (a *App) tailscaleSetup(ctx context.Context) error {
	fmt.Fprintln(a.Out, tailscale.SetupGuideTag(a.Config.Machine, a.Config.TailscaleTag))
	in := bufio.NewReader(os.Stdin)
	fmt.Fprint(a.Out, "Press Enter when the OAuth client is created (or type 'skip'): ")
	line, _ := in.ReadString('\n')
	if strings.TrimSpace(line) == "skip" {
		return nil
	}
	fmt.Fprint(a.Out, "OAuth client ID: ")
	id, _ := in.ReadString('\n')
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("no client ID entered")
	}
	kc := tailscale.Keychain{R: a.Runner}
	if err := kc.Set(ctx, tailscale.AccountClientID, id); err != nil {
		return err
	}
	fmt.Fprintln(a.Out, "Now enter the client secret (security prompts twice; it never passes through dev):")
	if err := kc.SetInteractive(ctx, tailscale.AccountClientSecret); err != nil {
		return err
	}
	tsClientCache = nil
	if err := a.tsClient().CheckToken(ctx); err != nil {
		return fmt.Errorf("stored, but minting an API token failed: %w", err)
	}
	fmt.Fprintln(a.Out, "✓ Tailscale OAuth client stored and verified.")
	return nil
}
