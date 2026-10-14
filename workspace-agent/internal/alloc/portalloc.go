// Package alloc picks the host port for a workspace's TCP proxy: lowest
// free port in the configured range, "used" = recorded in CR status plus a
// live bind probe (rules out anything squatting outside the CR world).
package alloc

import (
	"fmt"
	"net"
)

// ProbePort reports whether a TCP port is free to bind on this host.
func ProbePort(port int32) bool {
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
}

// AllocateFree returns the lowest port in [lo, hi] that is not in used and
// passes the bind probe. Empty probe (nil) skips the bind check.
func AllocateFree(used []int32, lo, hi int32, probe func(int32) bool) (int32, error) {
	if lo > hi {
		return 0, fmt.Errorf("port range [%d,%d] is inverted", lo, hi)
	}
	taken := make(map[int32]bool, len(used))
	for _, p := range used {
		taken[p] = true
	}
	for port := lo; port <= hi; port++ {
		if taken[port] {
			continue
		}
		if probe != nil && !probe(port) {
			continue
		}
		return port, nil
	}
	return 0, fmt.Errorf("no free port in [%d,%d] (%d recorded in use)", lo, hi, len(used))
}
