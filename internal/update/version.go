package update

import (
	"strconv"
	"strings"
)

// semver is a parsed vX.Y.Z[-pre] version.
type semver struct {
	major, minor, patch int
	pre                 string
}

// parseVersion parses "vX.Y.Z" or "vX.Y.Z-pre" (the leading "v" is
// optional). Build metadata ("+...") is ignored.
func parseVersion(s string) (semver, bool) {
	s = strings.TrimPrefix(strings.TrimSpace(s), "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}
	var v semver
	if i := strings.IndexByte(s, '-'); i >= 0 {
		v.pre = s[i+1:]
		s = s[:i]
		if v.pre == "" {
			return semver{}, false
		}
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := make([]int, 3)
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || p == "" {
			return semver{}, false
		}
		nums[i] = n
	}
	v.major, v.minor, v.patch = nums[0], nums[1], nums[2]
	return v, true
}

func (a semver) compare(b semver) int {
	for _, d := range [3]int{a.major - b.major, a.minor - b.minor, a.patch - b.patch} {
		if d != 0 {
			if d < 0 {
				return -1
			}
			return 1
		}
	}
	switch {
	case a.pre == b.pre:
		return 0
	case a.pre == "":
		return 1 // a release beats its pre-release
	case b.pre == "":
		return -1
	case a.pre < b.pre:
		return -1
	}
	return 1
}

// IsRelease reports whether v is a plain release version (vX.Y.Z, no
// pre-release suffix). Only release builds auto-update; "dev" (a local
// `go build`) and snapshot builds ("v0.1.0-SNAPSHOT-abc") never do, though an
// explicit `dev self-update` still installs the latest release over them.
func IsRelease(v string) bool {
	if !strings.HasPrefix(v, "v") {
		return false
	}
	sv, ok := parseVersion(v)
	return ok && sv.pre == ""
}

// Newer reports whether candidate is a valid version strictly newer than
// current. If current is not a parseable version, any valid candidate counts
// as newer.
func Newer(candidate, current string) bool {
	c, ok := parseVersion(candidate)
	if !ok {
		return false
	}
	cur, ok := parseVersion(current)
	if !ok {
		return true
	}
	return c.compare(cur) > 0
}
