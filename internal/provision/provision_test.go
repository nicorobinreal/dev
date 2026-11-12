package provision

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/lima"
	"github.com/nicorobinreal/dev/internal/run"
)

// fakeVM simulates the state file and records the steps whose scripts ran.
type fakeVM struct {
	recorded []string // contents of StateFile, in order
	ran      []string // step IDs whose Script ran
	scripts  []string
	stdins   []string
	failStep string // step ID whose script fails
	doneOK   bool   // Done checks succeed
}

func (f *fakeVM) Shell(_ context.Context, script string, stdin io.Reader) (run.Result, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.scripts = append(f.scripts, script)
	f.stdins = append(f.stdins, in)
	switch {
	case strings.HasPrefix(script, "cat "+StateFile):
		if len(f.recorded) == 0 {
			return run.Result{}, nil
		}
		return run.Result{Stdout: strings.Join(f.recorded, "\n") + "\n"}, nil
	case strings.Contains(script, "tee -a "+StateFile):
		for _, s := range Steps() {
			if strings.Contains(script, "echo '"+s.ID+"' |") {
				f.recorded = append(f.recorded, s.ID)
			}
		}
		return run.Result{}, nil
	case strings.HasPrefix(script, "command -v tailscale >/dev/null && tailscale status"):
		if f.doneOK {
			return run.Result{}, nil
		}
		return run.Result{ExitCode: 1}, &run.ExitError{Result: run.Result{ExitCode: 1}}
	}
	id := identify(script)
	f.ran = append(f.ran, id)
	if id == f.failStep {
		return run.Result{ExitCode: 1}, &run.ExitError{Result: run.Result{ExitCode: 1, Stderr: "boom"}}
	}
	return run.Result{}, nil
}

// identify maps a script back to its step by a distinctive fragment.
func identify(script string) string {
	marks := []struct{ id, frag string }{
		{StepBasePackages, "20auto-upgrades"},
		{StepDocker, "download.docker.com"},
		{StepGitGH, "cli.github.com"},
		{StepNode, "@devcontainers/cli"},
		{StepGHToken, "gh auth login"},
		{StepClone, "git clone"},
		{StepTailscale, "tailscale up"},
		{StepContainerSSH, "ssh_host_ed25519_key"},
	}
	for _, m := range marks {
		if strings.Contains(script, m.frag) {
			return m.id
		}
	}
	return "?"
}

const (
	ghTok  = "gho_SECRETTOKEN"
	tsKey  = "tskey-auth-SECRETKEY"
	pubKey = "ssh-ed25519 AAAAC3Nza lima"
)

func params(minted *int) Params {
	return Params{
		RepoURL:           "https://github.com/owner/repo",
		RepoDir:           "repo",
		Branch:            "main",
		GHToken:           ghTok,
		TailscaleHostname: "mbp-shop",
		TailscaleTag:      "tag:devvm",
		LimaPubKey:        pubKey,
		TailscaleAuthKey: func(context.Context) (string, error) {
			*minted++
			return tsKey, nil
		},
	}
}

func allIDs() []string {
	var ids []string
	for _, s := range Steps() {
		ids = append(ids, s.ID)
	}
	return ids
}

func TestStepIDsStableAndUnique(t *testing.T) {
	want := "base-packages docker git-gh node-devcontainer-cli gh-token clone tailscale container-ssh"
	if got := strings.Join(allIDs(), " "); got != want {
		t.Fatalf("step order/IDs changed:\n got %s\nwant %s", got, want)
	}
}

func TestRunOrderAndRecording(t *testing.T) {
	vm := &fakeVM{}
	var minted int
	var progress []string
	p := params(&minted)
	p.Progress = func(s Step) { progress = append(progress, s.ID) }
	ran, err := Run(context.Background(), vm, p, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := strings.Join(allIDs(), " ")
	for name, got := range map[string][]string{"ran": ran, "executed": vm.ran, "recorded": vm.recorded, "progress": progress} {
		if strings.Join(got, " ") != want {
			t.Errorf("%s = %v, want %s", name, got, want)
		}
	}
	if minted != 1 {
		t.Errorf("minted %d keys, want 1", minted)
	}
	if !NeedsSessionReset(ran) {
		t.Error("docker ran: session reset needed")
	}
}

func TestRunIdempotent(t *testing.T) {
	vm := &fakeVM{recorded: []string{StepBasePackages, StepDocker, StepGHToken, "future-step"}}
	var minted int
	ran, err := Run(context.Background(), vm, params(&minted), nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "git-gh node-devcontainer-cli clone tailscale container-ssh"
	if strings.Join(ran, " ") != want || strings.Join(vm.ran, " ") != want {
		t.Fatalf("ran %v / executed %v, want %s", ran, vm.ran, want)
	}
	if NeedsSessionReset(ran) {
		t.Error("docker did not run")
	}
	// Second run does nothing.
	vm.ran = nil
	ran, err = Run(context.Background(), vm, params(&minted), nil)
	if err != nil || len(ran) != 0 || len(vm.ran) != 0 {
		t.Fatalf("second run: ran=%v executed=%v err=%v", ran, vm.ran, err)
	}
}

func TestRunStopsOnFailureAndResumes(t *testing.T) {
	vm := &fakeVM{failStep: StepNode}
	var minted int
	ran, err := Run(context.Background(), vm, params(&minted), nil)
	if err == nil || !strings.Contains(err.Error(), StepNode) {
		t.Fatalf("err = %v", err)
	}
	if strings.Join(ran, " ") != "base-packages docker git-gh" || strings.Join(vm.recorded, " ") != "base-packages docker git-gh" {
		t.Fatalf("ran=%v recorded=%v", ran, vm.recorded)
	}
	if minted != 0 {
		t.Fatal("tailscale key minted although its step never ran")
	}
	vm.failStep, vm.ran = "", nil
	if _, err := Run(context.Background(), vm, params(&minted), nil); err != nil {
		t.Fatal(err)
	}
	if vm.ran[0] != StepNode || len(vm.recorded) != len(Steps()) {
		t.Fatalf("resume executed %v, recorded %v", vm.ran, vm.recorded)
	}
}

func TestRunOnly(t *testing.T) {
	vm := &fakeVM{}
	var minted int
	ran, err := Run(context.Background(), vm, params(&minted), []string{StepContainerSSH, StepGitGH})
	if err != nil || strings.Join(ran, " ") != "git-gh container-ssh" {
		t.Fatalf("ran %v, %v", ran, err)
	}
	if _, err := Run(context.Background(), vm, params(&minted), []string{"nope"}); err == nil {
		t.Fatal("unknown step accepted")
	}
}

func TestTailscaleDoneSkipsMinting(t *testing.T) {
	vm := &fakeVM{doneOK: true}
	var minted int
	ran, err := Run(context.Background(), vm, params(&minted), []string{StepTailscale})
	if err != nil || strings.Join(ran, " ") != StepTailscale {
		t.Fatalf("ran %v, %v", ran, err)
	}
	if minted != 0 || len(vm.ran) != 0 {
		t.Fatalf("minted=%d executed=%v; already-joined VM must not mint or run", minted, vm.ran)
	}
}

func TestMintErrorFailsStep(t *testing.T) {
	vm := &fakeVM{}
	p := params(new(int))
	p.TailscaleAuthKey = func(context.Context) (string, error) { return "", errors.New("api down") }
	if _, err := Run(context.Background(), vm, p, []string{StepTailscale}); err == nil || !strings.Contains(err.Error(), "api down") {
		t.Fatalf("err = %v", err)
	}
	if len(vm.recorded) != 0 {
		t.Fatal("failed step recorded")
	}
}

func TestMissing(t *testing.T) {
	vm := &fakeVM{recorded: []string{StepBasePackages, StepClone}}
	miss, err := Missing(context.Background(), vm)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range miss {
		ids = append(ids, s.ID)
	}
	if strings.Join(ids, " ") != "docker git-gh node-devcontainer-cli gh-token tailscale container-ssh" {
		t.Fatalf("missing = %v", ids)
	}
	none, _ := Missing(context.Background(), &fakeVM{recorded: allIDs()})
	if len(none) != 0 {
		t.Fatalf("fully provisioned VM missing %v", none)
	}
}

// TestSecretsNotInArgv runs the whole provisioning through lima.Client with a
// fake runner and checks that secrets only ever travel on stdin.
func TestSecretsNotInArgv(t *testing.T) {
	f := (&run.Fake{}).Fail("limactl shell --workdir / shop -- bash -c command -v tailscale", 1, "")
	vm := (&lima.Client{Runner: f}).VM("shop")
	var minted int
	if _, err := Run(context.Background(), vm, params(&minted), nil); err != nil {
		t.Fatal(err)
	}
	var sawTok, sawKey bool
	for i, c := range f.Calls {
		line := run.Line(c) + " " + c.String() + " " + strings.Join(c.Env, " ")
		if strings.Contains(line, ghTok) || strings.Contains(line, tsKey) {
			t.Fatalf("secret in argv/env: %s", line)
		}
		sawTok = sawTok || strings.Contains(f.StdinSeen[i], ghTok)
		sawKey = sawKey || strings.Contains(f.StdinSeen[i], tsKey)
	}
	if !sawTok || !sawKey {
		t.Fatalf("secrets not delivered on stdin (token %v, key %v)", sawTok, sawKey)
	}
}

func TestScriptParamsQuoted(t *testing.T) {
	p := params(new(int))
	p.Branch = "feat/x'; rm -rf /"
	s, err := cloneScript(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(s, `--branch 'feat/x'\''; rm -rf /' `) || !strings.Contains(s, "--recurse-submodules") {
		t.Fatalf("clone script:\n%s", s)
	}
	for _, bad := range []Params{{RepoURL: "git@github.com:o/r", RepoDir: "r"}, {RepoURL: "https://github.com/o/r", RepoDir: "../r"}, {}} {
		if _, err := cloneScript(bad); err == nil {
			t.Errorf("cloneScript(%+v) accepted", bad)
		}
	}
	if _, err := containerSSHScript(Params{LimaPubKey: "a\nb"}); err == nil {
		t.Error("multi-line pubkey accepted")
	}
	ts, _ := tailscaleScript(p)
	if !strings.Contains(ts, "--auth-key=\"file:$kf\"") || !strings.Contains(ts, "--hostname='mbp-shop' --advertise-tags='tag:devvm'") {
		t.Fatalf("tailscale script:\n%s", ts)
	}
}

// TestScriptsParse checks every generated script is valid bash syntax.
func TestScriptsParse(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("no bash")
	}
	p := params(new(int))
	scripts := []string{recordScript("x")}
	for _, s := range Steps() {
		sc, err := s.Script(p)
		if err != nil {
			t.Fatalf("%s: %v", s.ID, err)
		}
		scripts = append(scripts, sc)
		if s.Done != nil {
			scripts = append(scripts, s.Done(p))
		}
	}
	for _, sc := range scripts {
		if out, err := exec.Command(bash, "-n", "-c", sc).CombinedOutput(); err != nil {
			t.Errorf("bash -n: %v: %s\n%s", err, out, sc)
		}
	}
}
