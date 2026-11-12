package lima

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nicorobinreal/dev/internal/run"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestGenerateYAMLGolden(t *testing.T) {
	got, err := GenerateYAML(Spec{Name: "shop", CPUs: 4, Memory: "8GiB", Disk: "60GiB"})
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "shop.yaml")
	if *update {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(want) {
		t.Errorf("YAML differs from %s (run with -update):\n%s", golden, got)
	}
	// If limactl is installed, make sure Lima itself accepts the file.
	if lc, err := exec.LookPath("limactl"); err == nil {
		if out, err := exec.Command(lc, "validate", golden).CombinedOutput(); err != nil {
			t.Errorf("limactl validate: %v\n%s", err, out)
		}
	}
}

func TestGenerateYAMLValidates(t *testing.T) {
	bad := []Spec{
		{CPUs: 0, Memory: "8GiB", Disk: "60GiB"},
		{CPUs: 2, Memory: "8 GiB", Disk: "60GiB"},
		{CPUs: 2, Memory: "8GiB", Disk: "60GiB\nmounts: [x]"},
		{CPUs: 2, Memory: "", Disk: "60GiB"},
	}
	for _, s := range bad {
		if _, err := GenerateYAML(s); err == nil {
			t.Errorf("GenerateYAML(%+v) succeeded, want error", s)
		}
	}
}

const listOut = `{"name":"shop","hostname":"lima-shop","status":"Running","dir":"/Users/u/.lima/shop","vmType":"vz","arch":"aarch64","cpus":4,"memory":8589934592,"disk":64424509440,"sshLocalPort":60022,"sshConfigFile":"/Users/u/.lima/shop/ssh.config","config":{"vmType":"vz"}}
{"name":"blog","status":"Stopped","dir":"/Users/u/.lima/blog","cpus":2,"memory":4294967296,"disk":10737418240}
`

func TestListAndGet(t *testing.T) {
	f := (&run.Fake{}).On("limactl list --json", listOut)
	c := &Client{Runner: f}
	list, err := c.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "shop" || !list[0].Running() || list[0].Memory != 8589934592 || list[1].Status != StatusStopped {
		t.Fatalf("unexpected list: %+v", list)
	}
	in, ok, err := c.Get(context.Background(), "blog")
	if err != nil || !ok || in.CPUs != 2 {
		t.Fatalf("Get blog = %+v %v %v", in, ok, err)
	}
	if _, ok, _ := c.Get(context.Background(), "nope"); ok {
		t.Fatal("Get nope found something")
	}
}

func TestListEmpty(t *testing.T) {
	c := &Client{Runner: &run.Fake{}}
	list, err := c.List(context.Background())
	if err != nil || len(list) != 0 {
		t.Fatalf("List = %v, %v", list, err)
	}
}

func TestLifecycleCommands(t *testing.T) {
	f := &run.Fake{}
	c := &Client{Runner: f}
	ctx := context.Background()
	if err := c.Create(ctx, "shop", []byte("vmType: vz\n")); err != nil {
		t.Fatal(err)
	}
	_ = c.Start(ctx, "shop")
	_ = c.Stop(ctx, "shop")
	_ = c.Delete(ctx, "shop", false)
	_ = c.Delete(ctx, "shop", true)
	want := []string{
		"limactl create --name shop --tty=false -",
		"limactl start --tty=false shop",
		"limactl stop --tty=false shop",
		"limactl delete --tty=false shop",
		"limactl delete --tty=false --force shop",
	}
	got := f.Lines()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commands:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if f.StdinSeen[0] != "vmType: vz\n" {
		t.Fatalf("create stdin = %q", f.StdinSeen[0])
	}
}

func TestShellAndVM(t *testing.T) {
	f := &run.Fake{}
	c := &Client{Runner: f}
	_, err := c.VM("shop").Shell(context.Background(), "echo hi", strings.NewReader("s3cret"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := f.Calls[0]
	want := []string{"shell", "--workdir", "/", "shop", "--", "bash", "-c", "echo hi"}
	if strings.Join(cmd.Args, "|") != strings.Join(want, "|") {
		t.Fatalf("args = %q", cmd.Args)
	}
	if f.StdinSeen[0] != "s3cret" || strings.Contains(run.Line(cmd), "s3cret") {
		t.Fatal("secret must travel on stdin only")
	}
}

func TestExecInteractive(t *testing.T) {
	f := &run.Fake{}
	c := &Client{Runner: f}
	_, _ = c.Exec(context.Background(), "shop", "/home/u.linux/shop", "ls", "-la")
	cmd := f.Calls[0]
	if !cmd.Interactive || run.Line(cmd) != "limactl shell --workdir /home/u.linux/shop shop -- ls -la" {
		t.Fatalf("got %s interactive=%v", run.Line(cmd), cmd.Interactive)
	}
}

func TestHome(t *testing.T) {
	f := (&run.Fake{}).On("limactl shell", "/home/u.linux")
	h, err := (&Client{Runner: f}).Home(context.Background(), "shop")
	if err != nil || h != "/home/u.linux" {
		t.Fatalf("Home = %q, %v", h, err)
	}
	f2 := (&run.Fake{}).On("limactl shell", "")
	if _, err := (&Client{Runner: f2}).Home(context.Background(), "shop"); err == nil {
		t.Fatal("empty HOME should fail")
	}
}

func TestShowSSHPrefersFile(t *testing.T) {
	f := (&run.Fake{}).On("limactl list --json", listOut)
	c := &Client{Runner: f, ReadFile: func(p string) ([]byte, error) {
		if p == "/Users/u/.lima/shop/ssh.config" {
			return []byte("Host lima-shop\n  Port 60022\n"), nil
		}
		return nil, os.ErrNotExist
	}}
	got, err := c.ShowSSH(context.Background(), "shop")
	if err != nil || !strings.HasPrefix(got, "Host lima-shop") {
		t.Fatalf("ShowSSH = %q, %v", got, err)
	}
	for _, l := range f.Lines() {
		if strings.Contains(l, "show-ssh") {
			t.Fatal("should not call show-ssh when the file exists")
		}
	}
	// blog has no sshConfigFile field and no file: fall back to show-ssh.
	f.On("limactl show-ssh --format config blog", "Host lima-blog\n")
	got, err = c.ShowSSH(context.Background(), "blog")
	if err != nil || got != "Host lima-blog\n" {
		t.Fatalf("fallback ShowSSH = %q, %v", got, err)
	}
}

func TestResetSSH(t *testing.T) {
	f := (&run.Fake{}).On("limactl list --json", listOut)
	f.Fail("ssh", 255, "Control socket connect: No such file or directory")
	c := &Client{Runner: f}
	if err := c.ResetSSH(context.Background(), "shop"); err != nil {
		t.Fatal(err)
	}
	last := f.Lines()[len(f.Lines())-1]
	if last != "ssh -F /Users/u/.lima/shop/ssh.config -O exit lima-shop" {
		t.Fatalf("got %q", last)
	}
}

func TestUsage(t *testing.T) {
	f := (&run.Fake{}).On("limactl shell", "cpus 4\nload 0.52 0.31 0.10\nmem 8323002368 1.2345e+09\ndisk 62725623808 5368709120\n")
	u, err := (&Client{Runner: f}).Usage(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	want := Usage{CPUs: 4, Load1: 0.52, Load5: 0.31, Load15: 0.10, MemTotal: 8323002368, MemUsed: 1234500000, DiskTotal: 62725623808, DiskUsed: 5368709120}
	if u != want {
		t.Fatalf("Usage = %+v, want %+v", u, want)
	}
}

func TestIsNotFound(t *testing.T) {
	err := &run.ExitError{Result: run.Result{Stderr: "instance `x` does not exist"}}
	if !IsNotFound(err) || IsNotFound(errors.New("boom")) {
		t.Fatal("IsNotFound misclassified")
	}
}
