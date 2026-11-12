package devcontainer

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/nicorobinreal/dev/internal/shellquote"
)

// UpOptions tune `devcontainer up`.
type UpOptions struct {
	// RemoveExisting adds --remove-existing-container (rebuild).
	RemoveExisting bool
	// BuildNoCache adds --build-no-cache (rebuild --no-cache).
	BuildNoCache bool
	// StarterImage, if set, is exported as DEV_STARTER_IMAGE for the CLI so
	// the starter's Dockerfile builds FROM it (e2e before the image is
	// published). Callers pass os.Getenv(StarterImageEnv) from the Mac.
	StarterImage string
}

// UpArgs is the argv (to run inside the VM) that starts the devcontainer.
// With StarterImage set it is prefixed with `env DEV_STARTER_IMAGE=<image>`.
func UpArgs(workspace string, o UpOptions) []string {
	var args []string
	if o.StarterImage != "" {
		args = append(args, "env", StarterImageEnv+"="+o.StarterImage)
	}
	args = append(args, "devcontainer", "up", "--workspace-folder", workspace)
	if o.RemoveExisting {
		args = append(args, "--remove-existing-container")
	}
	if o.BuildNoCache {
		args = append(args, "--build-no-cache")
	}
	return args
}

// RebuildArgs is `devcontainer up` with --remove-existing-container, plus
// --build-no-cache when noCache.
func RebuildArgs(workspace string, noCache bool, starterImage string) []string {
	return UpArgs(workspace, UpOptions{RemoveExisting: true, BuildNoCache: noCache, StarterImage: starterImage})
}

// ExecArgs is the argv (to run inside the VM) that runs args in the
// devcontainer: devcontainer exec --workspace-folder <ws> -- args...
func ExecArgs(workspace string, args ...string) []string {
	return append([]string{"devcontainer", "exec", "--workspace-folder", workspace, "--"}, args...)
}

// Script joins argv into one POSIX-quoted command line, for
// `limactl shell <vm> bash -lc <script>`.
func Script(argv []string) string { return shellquote.Quote(argv...) }

// UpResult is the JSON line `devcontainer up` prints last on stdout.
type UpResult struct {
	Outcome               string `json:"outcome"` // "success" | "error"
	ContainerID           string `json:"containerId,omitempty"`
	RemoteUser            string `json:"remoteUser,omitempty"`
	RemoteWorkspaceFolder string `json:"remoteWorkspaceFolder,omitempty"`
	Message               string `json:"message,omitempty"`
	Description           string `json:"description,omitempty"`
}

// ErrNoUpResult means the output held no result line.
var ErrNoUpResult = errors.New("devcontainer up: no result in output")

// ParseUpOutput finds the last JSON object with an "outcome" field in stdout.
// A result with outcome other than "success" is returned together with an
// error carrying its message and description.
func ParseUpOutput(stdout string) (UpResult, error) {
	lines := strings.Split(stdout, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "{") {
			continue
		}
		var r UpResult
		if err := json.Unmarshal([]byte(l), &r); err != nil || r.Outcome == "" {
			continue
		}
		if r.Outcome != "success" {
			msg := strings.TrimSpace(strings.Join(nonEmpty(r.Message, r.Description), ": "))
			if msg == "" {
				msg = "outcome " + r.Outcome
			}
			return r, fmt.Errorf("devcontainer up failed: %s", msg)
		}
		return r, nil
	}
	return UpResult{}, ErrNoUpResult
}

func nonEmpty(ss ...string) []string {
	var out []string
	for _, s := range ss {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}
