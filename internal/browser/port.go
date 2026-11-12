package browser

import (
	"errors"
	"fmt"
	"net"
	"slices"

	"github.com/nicorobinreal/dev/internal/state"
)

// Port range for bridges. Chosen to stay clear of common dev-server ports.
var (
	PortMin = 9300
	PortMax = 9399
)

// ErrNoFreePort means every port in [PortMin, PortMax] is taken.
var ErrNoFreePort = errors.New("no free browser bridge port")

// FreePort returns the lowest port in [PortMin, PortMax] that is not in used
// (ports recorded in other projects' state) and can be bound on 127.0.0.1
// right now.
func FreePort(used []int) (int, error) {
	for p := PortMin; p <= PortMax; p++ {
		if slices.Contains(used, p) {
			continue
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p))
		if err != nil {
			continue
		}
		ln.Close()
		return p, nil
	}
	return 0, fmt.Errorf("%w in %d-%d", ErrNoFreePort, PortMin, PortMax)
}

// UsedPorts collects the browser ports of every project except the named one.
func UsedPorts(projects []state.Project, except string) []int {
	var out []int
	for _, pr := range projects {
		if pr.Name != except && pr.BrowserPort != 0 {
			out = append(out, pr.BrowserPort)
		}
	}
	return out
}
