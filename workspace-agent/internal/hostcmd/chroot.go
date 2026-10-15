// Package hostcmd runs node-side commands (ip/iptables/mkfs.ext4) through
// the host's own binaries via chroot into the mounted host root — the
// ko-built static agent image carries no userland, and using the node's
// iptables keeps nft/legacy backend parity with kube-proxy.
package hostcmd

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
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

// Run changes root only in the child process, then executes a host binary.
// Static ko images do not contain a chroot executable or host userland.
func (r *ChrootRunner) Run(ctx context.Context, name string, args ...string) (string, error) {
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	path, err := hostExecutable(r.HostRoot, name)
	if err != nil {
		return "", err
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Chroot: r.HostRoot}
	cmd.Dir = "/"
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("chroot %s %s: %w: %s", r.HostRoot, name, err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// Resolve against the mounted host, not the static container's PATH. Lstat
// preserves absolute host symlinks (e.g. /etc/alternatives/iptables): the child
// resolves their targets after chroot, where the host paths are meaningful.
func hostExecutable(root, name string) (string, error) {
	for _, dir := range []string{"/usr/sbin", "/usr/bin", "/sbin", "/bin"} {
		path := filepath.Join(dir, name)
		info, err := os.Lstat(filepath.Join(root, path))
		if err == nil && (info.Mode()&os.ModeSymlink != 0 || (info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0)) {
			return path, nil
		}
	}
	return "", fmt.Errorf("host executable %q not found under %s", name, root)
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
