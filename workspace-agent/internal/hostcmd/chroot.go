// Package hostcmd runs node-side commands (ip/iptables/mkfs.ext4) through
// the host's own binaries via chroot into the mounted host root — the
// ko-built static agent image carries no userland, and using the node's
// iptables keeps nft/legacy backend parity with kube-proxy.
package hostcmd

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Runner executes one host command with args and returns combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) (string, error)
}

// ChrootRunner runs commands inside the host root mounted at hostRoot.
type ChrootRunner struct {
	// HostRoot is the mount point of the node filesystem (e.g. /host).
	HostRoot string
	// Timeout bounds each command.
	Timeout time.Duration
}

// NewChrootRunner builds a runner with a sane default timeout.
func NewChrootRunner(hostRoot string) *ChrootRunner {
	return &ChrootRunner{HostRoot: hostRoot, Timeout: 30 * time.Second}
}

// Run executes `chroot <hostRoot> <name> <args...>`.
func (r *ChrootRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	full := append([]string{r.HostRoot, name}, args...)
	cmd := exec.CommandContext(ctx, "chroot", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("chroot %s %s: %w: %s", r.HostRoot, name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// FakeRunner records commands for assertions in tests.
type FakeRunner struct {
	Calls    []string
	Response func(name string, args []string) (string, error)
}

// Run implements Runner.
func (f *FakeRunner) Run(_ context.Context, name string, args ...string) (string, error) {
	call := name + " " + strings.Join(args, " ")
	f.Calls = append(f.Calls, call)
	if f.Response != nil {
		return f.Response(name, args)
	}
	return "", nil
}
