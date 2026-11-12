package gh

import (
	"fmt"
	"regexp"
	"strings"
)

// Repo is a GitHub repository.
type Repo struct {
	Owner string
	Name  string
}

// URL is the canonical HTTPS clone URL, https://github.com/<owner>/<name>.
func (r Repo) URL() string { return "https://github.com/" + r.Owner + "/" + r.Name }

// Dir is the clone directory name in the VM ($HOME/<Dir>).
func (r Repo) Dir() string { return r.Name }

// String is "owner/name".
func (r Repo) String() string { return r.Owner + "/" + r.Name }

var (
	ownerRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)
	nameRE  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ParseRepo accepts the usual ways to name a GitHub repo and normalises them:
//
//	owner/repo
//	github.com/owner/repo
//	https://github.com/owner/repo(.git)(/)
//	git@github.com:owner/repo(.git)
//	ssh://git@github.com/owner/repo(.git)
//
// Anything not on github.com is rejected (v1 is GitHub only).
func ParseRepo(arg string) (Repo, error) {
	s := strings.TrimSpace(arg)
	orig := s
	switch {
	case strings.HasPrefix(s, "git@"):
		host, path, ok := strings.Cut(strings.TrimPrefix(s, "git@"), ":")
		if !ok || !isGitHub(host) {
			return Repo{}, notGitHub(orig)
		}
		s = path
	case strings.Contains(s, "://"):
		scheme, rest, _ := strings.Cut(s, "://")
		switch strings.ToLower(scheme) {
		case "https", "http", "ssh", "git":
		default:
			return Repo{}, notGitHub(orig)
		}
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			rest = rest[at+1:] // drop user info (git@, token@)
		}
		host, path, _ := strings.Cut(rest, "/")
		if !isGitHub(host) {
			return Repo{}, notGitHub(orig)
		}
		s = path
	default:
		if host, path, ok := strings.Cut(s, "/"); ok && strings.Contains(host, ".") {
			if !isGitHub(host) {
				return Repo{}, notGitHub(orig)
			}
			s = path
		}
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
	parts := strings.Split(s, "/")
	if len(parts) != 2 || !ownerRE.MatchString(parts[0]) || !nameRE.MatchString(parts[1]) ||
		parts[1] == "." || parts[1] == ".." {
		return Repo{}, fmt.Errorf("invalid GitHub repo %q: use owner/repo or https://github.com/owner/repo", orig)
	}
	return Repo{Owner: parts[0], Name: parts[1]}, nil
}

// NormalizeRepo returns the HTTPS clone URL and clone directory for arg.
func NormalizeRepo(arg string) (url, dir string, err error) {
	r, err := ParseRepo(arg)
	if err != nil {
		return "", "", err
	}
	return r.URL(), r.Dir(), nil
}

func isGitHub(host string) bool {
	host = strings.ToLower(host)
	if h, _, ok := strings.Cut(host, ":"); ok { // port
		host = h
	}
	return host == "github.com" || host == "www.github.com"
}

func notGitHub(arg string) error {
	return fmt.Errorf("unsupported repo %q: only github.com repositories are supported", arg)
}
