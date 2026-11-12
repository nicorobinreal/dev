package tailscale

import (
	"fmt"
	"strings"
)

// DefaultTag is the tag every dev VM joins with.
const DefaultTag = "tag:devvm"

// PolicySnippet is the minimal tailnet policy dev needs: tag:devvm owned by
// admins, your own devices may reach tag:devvm (and each other), and nothing
// lets tag:devvm reach anything. It replaces the default allow-all.
func PolicySnippet(tag string) string {
	if tag == "" {
		tag = DefaultTag
	}
	return strings.ReplaceAll(`{
  "tagOwners": {
    "TAG": ["autogroup:admin"]
  },
  "grants": [
    // Your devices can reach dev VMs on any port.
    {"src": ["autogroup:member"], "dst": ["TAG"], "ip": ["*"]},
    // Your devices can still reach each other (replaces the default allow-all).
    {"src": ["autogroup:member"], "dst": ["autogroup:self"], "ip": ["*"]}
    // No grant has "src": ["TAG"]: dev VMs cannot reach other devices.
  ]
}`, "TAG", tag)
}

// SetupGuide is the one-time Tailscale setup that `dev doctor` prints when
// the OAuth client is missing from the Keychain.
func SetupGuide(machine string) string {
	return SetupGuideTag(machine, DefaultTag)
}

// SetupGuideTag is SetupGuide with a custom tag (config tailscale_tag).
func SetupGuideTag(machine, tag string) string {
	if tag == "" {
		tag = DefaultTag
	}
	if machine == "" {
		machine = "<machine>"
	}
	indent := func(s string) string {
		return "     " + strings.ReplaceAll(s, "\n", "\n     ")
	}
	return fmt.Sprintf(`One-time Tailscale setup for dev
================================

Each dev VM joins your tailnet as %[1]s-<name>, tagged %[2]s, with a
single-use auth key that dev mints through an OAuth client. Do these steps
once in the Tailscale admin console (https://login.tailscale.com/admin).

1. Access controls → Policy file (https://login.tailscale.com/admin/acls/file)
   - tagOwners: give %[2]s to autogroup:admin.
   - Add a grant from your devices (autogroup:member) to %[2]s, all ports.
   - Do NOT add any grant or ACL whose src is %[2]s: VMs must not reach
     your other devices.
   - REMOVE the default allow-all rule ({"src": ["*"], "dst": ["*"], ...} or
     the "acls" accept "*" → "*:*" entry), or %[2]s can reach everything.
   Merge this into your policy file:

%[3]s

2. DNS (https://login.tailscale.com/admin/dns)
   - MagicDNS: on (so http://%[1]s-<name>:<port> resolves).
   - HTTPS Certificates: leave OFF. Turn it on only when a project needs
     HTTPS via `+"`dev serve`"+`; it publishes machine names in public
     Certificate Transparency logs.

3. Settings → OAuth clients / Trust credentials
   (https://login.tailscale.com/admin/settings/oauth) → Generate
   - Scopes: Auth Keys (write) and Devices Core (read and write).
   - Tag both scopes with %[2]s.
   - Copy the client ID and the client secret (shown only once).

4. Store them in the macOS Keychain (service dev-tailscale). Run
   `+"`dev doctor`"+` again and paste them when asked, or by hand:

     /usr/bin/security add-generic-password -U -s dev-tailscale -a client-id -w <client-id>
     /usr/bin/security add-generic-password -U -s dev-tailscale -a client-secret -w

   (with -w last, security prompts for the secret so it never appears in
   your shell history). The secret never leaves this Mac; VMs only get a
   short-lived single-use auth key.

Then run `+"`dev doctor`"+` to verify that dev can mint an API token.
`, machine, tag, indent(PolicySnippet(tag)))
}
