package shellquote

import (
	"os/exec"
	"strings"
	"testing"
)

func TestOne(t *testing.T) {
	cases := map[string]string{
		"":            "''",
		"abc":         "abc",
		"/home/u/x":   "/home/u/x",
		"a b":         "'a b'",
		"it's":        `'it'\''s'`,
		"$HOME":       "'$HOME'",
		"a;rm -rf /":  "'a;rm -rf /'",
		"--flag=v":    "--flag=v",
		"~":           "'~'",
		"*":           "'*'",
		"line\nbreak": "'line\nbreak'",
	}
	for in, want := range cases {
		if got := One(in); got != want {
			t.Errorf("One(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	args := []string{"plain", "with space", "it's", `"dq"`, "$HOME", "`x`", "a\\b", "", "new\nline", "*"}
	script := "set -- " + Quote(args...) + `; for a in "$@"; do printf '%s\0' "$a"; done`
	out, err := exec.Command(sh, "-c", script).Output()
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	if len(got) != len(args) {
		t.Fatalf("got %d args %q, want %d", len(got), got, len(args))
	}
	for i := range args {
		if got[i] != args[i] {
			t.Errorf("arg %d = %q, want %q", i, got[i], args[i])
		}
	}
}
