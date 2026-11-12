package state

import (
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestValidateName(t *testing.T) {
	ok := []string{"shop", "a", "my-app2", "x1-y"}
	for _, n := range ok {
		if err := ValidateName("mbp", n); err != nil {
			t.Errorf("%q: unexpected error %v", n, err)
		}
	}
	bad := []string{"", "Shop", "1shop", "-shop", "shop-", "sh_op", "sh.op", "shop!"}
	for _, n := range bad {
		if err := ValidateName("mbp", n); err == nil {
			t.Errorf("%q: expected error", n)
		}
	}
	// <machine>-<name> at most 63 characters.
	if err := ValidateName("mbp", strings.Repeat("a", 59)); err != nil {
		t.Errorf("63 chars should pass: %v", err)
	}
	if err := ValidateName("mbp", strings.Repeat("a", 60)); err == nil {
		t.Error("64 chars should fail")
	}
}

func TestValidateMachine(t *testing.T) {
	for _, m := range []string{"black", "mbp-2", "e2e"} {
		if err := ValidateMachine(m); err != nil {
			t.Errorf("%q: %v", m, err)
		}
	}
	for _, m := range []string{"", "Black", "2mbp", "mbp-", "a_b", strings.Repeat("a", 41)} {
		if ValidateMachine(m) == nil {
			t.Errorf("%q: expected error", m)
		}
	}
}

func TestConfigRoundTrip(t *testing.T) {
	p := Paths{Home: t.TempDir()}
	c, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.CPUs != 4 || c.TailscaleTag != "tag:devvm" {
		t.Fatalf("defaults not applied: %+v", c)
	}
	c.Machine = "black"
	c.CPUs = 6
	if err := SaveConfig(p, c); err != nil {
		t.Fatal(err)
	}
	c2, err := LoadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if c2.Machine != "black" || c2.CPUs != 6 || c2.Memory != "8GiB" {
		t.Fatalf("round trip: %+v", c2)
	}
}

func TestProjectsCRUD(t *testing.T) {
	p := Paths{Home: t.TempDir()}
	if ps, err := ListProjects(p); err != nil || len(ps) != 0 {
		t.Fatalf("empty list: %v %v", ps, err)
	}
	if _, err := LoadProject(p, "nope"); !errors.Is(err, ErrNoProject) {
		t.Fatalf("want ErrNoProject, got %v", err)
	}
	for _, n := range []string{"b", "a"} {
		if err := SaveProject(p, Project{Name: n, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	ps, err := ListProjects(p)
	if err != nil || len(ps) != 2 || ps[0].Name != "a" {
		t.Fatalf("list: %v %v", ps, err)
	}
	if err := DeleteProject(p, "a"); err != nil {
		t.Fatal(err)
	}
	if err := DeleteProject(p, "a"); err != nil {
		t.Fatal("delete should be idempotent")
	}
}

func TestLockTimeout(t *testing.T) {
	p := Paths{Home: t.TempDir()}
	old := LockWait
	LockWait = 300 * time.Millisecond
	defer func() { LockWait = old }()
	l, err := LockProject(p, "shop")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockProject(p, "shop"); !errors.Is(err, ErrLockHeld) {
		t.Fatalf("want ErrLockHeld, got %v", err)
	}
	l.Unlock()
	l2, err := LockProject(p, "shop")
	if err != nil {
		t.Fatalf("after unlock: %v", err)
	}
	l2.Unlock()
}

func TestWriteFileAtomicMode(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/x/y.txt"
	if err := WriteFileAtomic(f, []byte("hi"), 0o600); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(f)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v %v", st.Mode(), err)
	}
}
