package tailscale

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/nicorobinreal/dev/internal/run"
)

// CLI is the tailscale binary name looked up on PATH.
const CLI = "tailscale"

// Status is the subset of `tailscale status --json` dev uses.
type Status struct {
	BackendState    string // Running, Stopped, NeedsLogin, NoState, Starting, NeedsMachineAuth
	HaveNodeKey     bool
	MagicDNSSuffix  string // e.g. "taildbf6fc.ts.net"
	MagicDNSEnabled bool
	TailnetName     string   // CurrentTailnet.Name, e.g. "user.github"
	CertDomains     []string // non-empty when HTTPS certificates are enabled
	SelfHostName    string
	SelfDNSName     string // FQDN without the trailing dot
}

type rawStatus struct {
	BackendState   string   `json:"BackendState"`
	HaveNodeKey    bool     `json:"HaveNodeKey"`
	MagicDNSSuffix string   `json:"MagicDNSSuffix"`
	CertDomains    []string `json:"CertDomains"`
	CurrentTailnet *struct {
		Name            string `json:"Name"`
		MagicDNSSuffix  string `json:"MagicDNSSuffix"`
		MagicDNSEnabled bool   `json:"MagicDNSEnabled"`
	} `json:"CurrentTailnet"`
	Self *struct {
		HostName string `json:"HostName"`
		DNSName  string `json:"DNSName"`
	} `json:"Self"`
}

// ParseStatus parses `tailscale status --json` output (from the Mac, or from
// a VM via limactl shell).
func ParseStatus(b []byte) (Status, error) {
	var r rawStatus
	if err := json.Unmarshal(b, &r); err != nil {
		return Status{}, fmt.Errorf("parse tailscale status: %w", err)
	}
	s := Status{
		BackendState:   r.BackendState,
		HaveNodeKey:    r.HaveNodeKey,
		MagicDNSSuffix: r.MagicDNSSuffix,
		CertDomains:    r.CertDomains,
	}
	if t := r.CurrentTailnet; t != nil {
		s.TailnetName = t.Name
		s.MagicDNSEnabled = t.MagicDNSEnabled
		if s.MagicDNSSuffix == "" {
			s.MagicDNSSuffix = t.MagicDNSSuffix
		}
	}
	if r.Self != nil {
		s.SelfHostName = r.Self.HostName
		s.SelfDNSName = strings.TrimSuffix(r.Self.DNSName, ".")
	}
	return s, nil
}

// LoggedIn reports whether this node is logged in to a tailnet (it may be
// stopped).
func (s Status) LoggedIn() bool {
	switch s.BackendState {
	case "Running", "Stopped", "Starting":
		return s.HaveNodeKey || s.BackendState == "Running"
	}
	return false
}

// Running reports whether the node is up and connected.
func (s Status) Running() bool { return s.BackendState == "Running" }

// HTTPSEnabled reports whether HTTPS certificates are enabled on the tailnet
// (the node is offered a certificate domain).
func (s Status) HTTPSEnabled() bool { return len(s.CertDomains) > 0 }

// LocalStatus runs `tailscale status --json` on the Mac. A logged-out
// tailscale exits non-zero but still prints JSON; that JSON is parsed.
func LocalStatus(ctx context.Context, r run.Runner) (Status, error) {
	res, err := r.Run(ctx, run.Cmd{Name: CLI, Args: []string{"status", "--json"}})
	if strings.TrimSpace(res.Stdout) == "" {
		if err == nil {
			err = fmt.Errorf("tailscale status --json printed nothing")
		}
		return Status{}, err
	}
	return ParseStatus([]byte(res.Stdout))
}

// URL is the plain-HTTP tailnet URL: http://<host>:<port>, or http://<host>
// when port is 0. Short MagicDNS names resolve on tailnet devices.
func URL(host string, port int) string {
	if port <= 0 || port == 80 {
		return "http://" + host
	}
	return "http://" + host + ":" + strconv.Itoa(port)
}

// HTTPSURL is the `tailscale serve` URL: https://<host>.<suffix>[:<port>]
// (port omitted for 0 or 443). HTTPS needs the full MagicDNS name, which is
// what the certificate covers.
func HTTPSURL(host, suffix string, port int) string {
	fqdn := host
	if suffix = strings.Trim(suffix, "."); suffix != "" {
		fqdn += "." + suffix
	}
	if port <= 0 || port == 443 {
		return "https://" + fqdn
	}
	return "https://" + fqdn + ":" + strconv.Itoa(port)
}

// ServeOnArgs is the command run in the VM for `dev serve <port>`: tailscale
// terminates HTTPS on <port> on the node and proxies to 127.0.0.1:<port>.
// --bg persists the config across reboots; --yes avoids interactive prompts.
func ServeOnArgs(port int) []string {
	p := strconv.Itoa(port)
	return []string{"sudo", CLI, "serve", "--bg", "--yes", "--https=" + p, "http://127.0.0.1:" + p}
}

// ServeOffArgs is the command run in the VM for `dev serve <port> --off`.
// It fails with "handler does not exist" when nothing is served on the port;
// see IsServeNotFound.
func ServeOffArgs(port int) []string {
	return []string{"sudo", CLI, "serve", "--yes", "--https=" + strconv.Itoa(port), "off"}
}

// ServeStatusArgs prints the current serve config as JSON ("{}" when empty).
func ServeStatusArgs() []string {
	return []string{"sudo", CLI, "serve", "status", "--json"}
}

// IsServeNotFound reports whether a `serve ... off` failure only means the
// port was not being served (treat as success).
func IsServeNotFound(output string) bool {
	return strings.Contains(output, "handler does not exist") ||
		strings.Contains(output, "serve config does not exist")
}

// HTTPSDisabledHelp explains how to enable HTTPS for `dev serve`.
func HTTPSDisabledHelp(host, suffix string) string {
	fqdn := host
	if suffix != "" {
		fqdn += "." + strings.Trim(suffix, ".")
	}
	return fmt.Sprintf(`HTTPS certificates are off on this tailnet, so tailscale serve can't get a certificate.

To enable them: Tailscale admin console → DNS → HTTPS Certificates → Enable
(https://login.tailscale.com/admin/dns).

Note: once enabled, every machine that gets a certificate has its name
(e.g. %s) published permanently in public Certificate Transparency logs.
Plain http://%s:<port> already works over the tailnet (WireGuard-encrypted);
you only need HTTPS for browser secure-context features.`, fqdn, host)
}
