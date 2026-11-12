package devcontainer

import (
	"reflect"
	"strings"
	"testing"
)

func TestUpArgs(t *testing.T) {
	ws := "/home/u.linux/shop"
	cases := []struct {
		o    UpOptions
		want string
	}{
		{UpOptions{}, "devcontainer up --workspace-folder /home/u.linux/shop"},
		{UpOptions{RemoveExisting: true}, "devcontainer up --workspace-folder /home/u.linux/shop --remove-existing-container"},
		{UpOptions{RemoveExisting: true, BuildNoCache: true}, "devcontainer up --workspace-folder /home/u.linux/shop --remove-existing-container --build-no-cache"},
		{UpOptions{StarterImage: "dev-starter:local"}, "env DEV_STARTER_IMAGE=dev-starter:local devcontainer up --workspace-folder /home/u.linux/shop"},
	}
	for _, c := range cases {
		if got := strings.Join(UpArgs(ws, c.o), " "); got != c.want {
			t.Errorf("UpArgs(%+v) = %q, want %q", c.o, got, c.want)
		}
	}
	if got := strings.Join(RebuildArgs(ws, true, ""), " "); got != cases[2].want {
		t.Errorf("RebuildArgs = %q", got)
	}
	if got := strings.Join(RebuildArgs(ws, false, ""), " "); got != cases[1].want {
		t.Errorf("RebuildArgs = %q", got)
	}
}

func TestExecArgs(t *testing.T) {
	got := ExecArgs("/w", "docker", "compose", "up", "-d")
	want := []string{"devcontainer", "exec", "--workspace-folder", "/w", "--", "docker", "compose", "up", "-d"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ExecArgs = %q", got)
	}
	if s := Script(ExecArgs("/w x", "sh", "-c", "echo $HOME")); s != `devcontainer exec --workspace-folder '/w x' -- sh -c 'echo $HOME'` {
		t.Errorf("Script = %s", s)
	}
}

func TestParseUpOutput(t *testing.T) {
	ok := "[1 ms] Start: Run\nsome log {not json}\n" +
		`{"outcome":"success","containerId":"abc123","remoteUser":"root","remoteWorkspaceFolder":"/home/u/shop"}` + "\n"
	r, err := ParseUpOutput(ok)
	if err != nil || r.ContainerID != "abc123" || r.RemoteUser != "root" || r.RemoteWorkspaceFolder != "/home/u/shop" {
		t.Errorf("success: %+v, %v", r, err)
	}

	bad := `{"outcome":"error","message":"Command failed: docker build","description":"An error occurred building the image."}`
	r, err = ParseUpOutput(bad)
	if err == nil || r.Outcome != "error" || !strings.Contains(err.Error(), "Command failed: docker build") {
		t.Errorf("error: %+v, %v", r, err)
	}

	if _, err := ParseUpOutput("just logs\n{\"other\":1}\n"); err != ErrNoUpResult {
		t.Errorf("no result: %v", err)
	}
	if _, err := ParseUpOutput(""); err != ErrNoUpResult {
		t.Errorf("empty: %v", err)
	}
}

func TestRequirements(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range Requirements {
		if r.ID == "" || r.Title == "" || r.Why == "" || len(r.Starter) == 0 {
			t.Errorf("incomplete requirement %+v", r)
		}
		if seen[r.ID] {
			t.Errorf("duplicate requirement %s", r.ID)
		}
		seen[r.ID] = true
	}
	for _, id := range []string{"host-network", "workspace-path", "gh-token", "sshd", "auth-volumes"} {
		if !seen[id] {
			t.Errorf("missing requirement %s", id)
		}
	}
}
