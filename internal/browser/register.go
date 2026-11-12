package browser

import (
	"fmt"
	"strings"
)

// MCPName is the name the Chrome MCP server is registered under in every
// harness.
const MCPName = "chrome-devtools"

// OpenCodeConfig is OpenCode's global config file inside the container.
const OpenCodeConfig = "/root/.config/opencode/opencode.json"

// MCPURL is the bridge URL as seen from the container (host networking plus
// the reverse tunnel make it localhost).
func MCPURL(port int) string { return fmt.Sprintf("http://localhost:%d/mcp", port) }

// ClaudeAddArgs registers the server with Claude Code for every project in
// the container (user scope, ~/.claude.json).
func ClaudeAddArgs(port int) []string {
	return []string{"claude", "mcp", "add", "--transport", "http", "--scope", "user", MCPName, MCPURL(port)}
}

// ClaudeRemoveArgs undoes ClaudeAddArgs. It exits non-zero when the entry is
// missing; callers ignore that.
func ClaudeRemoveArgs() []string {
	return []string{"claude", "mcp", "remove", "--scope", "user", MCPName}
}

// CodexAddArgs registers the server with Codex (~/.codex/config.toml,
// streamable HTTP via --url).
func CodexAddArgs(port int) []string {
	return []string{"codex", "mcp", "add", MCPName, "--url", MCPURL(port)}
}

// CodexRemoveArgs undoes CodexAddArgs.
func CodexRemoveArgs() []string {
	return []string{"codex", "mcp", "remove", MCPName}
}

// openCodeJS edits OpenCode's config with node, preserving every other key.
// argv: <add|remove> <file> [url]. A missing file or entry is fine on
// remove; a file that is not valid JSON is left untouched (exit 1).
const openCodeJS = `const fs = require("fs"), path = require("path");
const [op, file, url] = process.argv.slice(1);
const name = ` + "`" + MCPName + "`" + `;
let cfg = {};
try {
  cfg = JSON.parse(fs.readFileSync(file, "utf8"));
} catch (e) {
  if (e.code !== "ENOENT") { console.error(file + ": " + e.message); process.exit(1); }
  if (op === "remove") process.exit(0);
  cfg = { "$schema": "https://opencode.ai/config.json" };
}
if (cfg === null || typeof cfg !== "object" || Array.isArray(cfg)) { console.error(file + ": not a JSON object"); process.exit(1); }
if (op === "add") {
  if (typeof cfg.mcp !== "object" || cfg.mcp === null) cfg.mcp = {};
  cfg.mcp[name] = { type: "remote", url: url, enabled: true };
} else {
  if (!cfg.mcp || !(name in cfg.mcp)) process.exit(0);
  delete cfg.mcp[name];
  if (Object.keys(cfg.mcp).length === 0) delete cfg.mcp;
}
fs.mkdirSync(path.dirname(file), { recursive: true });
const tmp = file + ".tmp-" + process.pid;
fs.writeFileSync(tmp, JSON.stringify(cfg, null, 2) + "\n");
fs.renameSync(tmp, file);
`

// OpenCodeAddArgs writes mcp["chrome-devtools"] into OpenCode's config
// (OpenCode's own `mcp add` is interactive).
func OpenCodeAddArgs(port int) []string {
	return openCodeArgs("add", OpenCodeConfig, MCPURL(port))
}

// OpenCodeRemoveArgs removes the entry; missing file or entry is fine.
func OpenCodeRemoveArgs() []string {
	return openCodeArgs("remove", OpenCodeConfig)
}

func openCodeArgs(op, file string, rest ...string) []string {
	return append([]string{"node", "-e", openCodeJS, op, file}, rest...)
}

// RegisterScript returns one argv (bash -c ...) that registers the server
// with every harness installed in the container, idempotently: each existing
// entry is removed first. Harnesses that aren't installed are skipped. It
// prints one "<harness>: ok|skipped|failed" line each and exits non-zero if
// any registration failed.
func RegisterScript(port int) []string {
	return bashScript([]step{
		{"claude", "claude", ClaudeRemoveArgs(), ClaudeAddArgs(port), false},
		{"codex", "codex", CodexRemoveArgs(), CodexAddArgs(port), false},
		{"opencode", "node", nil, OpenCodeAddArgs(port), false},
	})
}

// UnregisterScript removes the registration from every harness, tolerating
// missing harnesses and entries. It always exits zero unless OpenCode's
// config is unreadable.
func UnregisterScript() []string {
	return bashScript([]step{
		{"claude", "claude", nil, ClaudeRemoveArgs(), true},
		{"codex", "codex", nil, CodexRemoveArgs(), true},
		{"opencode", "node", nil, OpenCodeRemoveArgs(), false},
	})
}

type step struct {
	harness, binary string
	pre             []string // run first, errors ignored
	main            []string
	tolerant        bool // main's failure is fine (removing a missing entry)
}

func bashScript(steps []step) []string {
	var b strings.Builder
	b.WriteString("rc=0\n")
	for _, s := range steps {
		fmt.Fprintf(&b, "if command -v %s >/dev/null 2>&1; then\n", s.binary)
		if s.pre != nil {
			fmt.Fprintf(&b, "  %s >/dev/null 2>&1 || true\n", shellJoin(s.pre))
		}
		if s.tolerant {
			fmt.Fprintf(&b, "  %s >/dev/null 2>&1 || true\n  echo '%s: ok'\n", shellJoin(s.main), s.harness)
		} else {
			fmt.Fprintf(&b, "  if %s >/dev/null; then echo '%s: ok'; else echo '%s: failed'; rc=1; fi\n", shellJoin(s.main), s.harness, s.harness)
		}
		fmt.Fprintf(&b, "else\n  echo '%s: skipped'\nfi\n", s.harness)
	}
	b.WriteString("exit $rc\n")
	return []string{"bash", "-c", b.String()}
}

func shellJoin(args []string) string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = shellQuote(a)
	}
	return strings.Join(out, " ")
}

func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_./:=@") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
