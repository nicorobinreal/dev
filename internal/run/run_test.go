package run

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestExecCapturesOutputAndExitCode(t *testing.T) {
	e := &Exec{}
	res, err := e.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "echo out; echo err >&2; exit 3"}})
	var ee *ExitError
	if !errors.As(err, &ee) || ee.Result.ExitCode != 3 {
		t.Fatalf("want exit 3, got %v", err)
	}
	if strings.TrimSpace(res.Stdout) != "out" || strings.TrimSpace(res.Stderr) != "err" {
		t.Fatalf("output: %+v", res)
	}
}

func TestFakeRules(t *testing.T) {
	f := (&Fake{}).On("limactl list", `{"name":"a"}`).Fail("limactl start", 1, "boom")
	out, err := Output(context.Background(), f, "limactl", "list", "--json")
	if err != nil || out != `{"name":"a"}` {
		t.Fatalf("got %q %v", out, err)
	}
	if _, err := f.Run(context.Background(), Cmd{Name: "limactl", Args: []string{"start", "a"}}); err == nil {
		t.Fatal("expected failure")
	}
	if _, err := f.Run(context.Background(), Cmd{Name: "other", Stdin: strings.NewReader("secret")}); err != nil {
		t.Fatal(err)
	}
	if got := f.Lines(); len(got) != 3 || got[1] != "limactl start a" {
		t.Fatalf("lines: %v", got)
	}
	if f.StdinSeen[2] != "secret" {
		t.Fatal("stdin not recorded")
	}
}
