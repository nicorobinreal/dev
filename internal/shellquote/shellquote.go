// Package shellquote quotes arguments for POSIX shells (bash, sh), so argv
// built on the Mac survives `limactl shell <vm> bash -c <script>` intact.
package shellquote

import "strings"

// One quotes a single argument. Strings made only of safe characters are
// returned as is; everything else is wrapped in single quotes, with embedded
// single quotes written as '\”.
func One(s string) string {
	if s == "" {
		return "''"
	}
	if isSafe(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Quote quotes each argument and joins them with spaces.
func Quote(args ...string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = One(a)
	}
	return strings.Join(q, " ")
}

func isSafe(s string) bool {
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("-_./:,+@%=", r):
		default:
			return false
		}
	}
	return true
}
