// Package tap manages the per-workspace host tap device and its subnet
// address, plus the per-subnet MASQUERADE rule — all through the host
// Runner (chroot ip) so tests can fake the command stream.
package tap

import (
	"context"
	"fmt"
	"strings"

	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
)

// Device is the per-workspace tap lifecycle.
type Device struct {
	// Name is the linux interface name (tap-pw-<ws>).
	Name string
	// HostCIDR is the host-side address, e.g. 192.168.77.1/30.
	HostCIDR string
	// Subnet is the /30 (source match for the NAT rule), e.g. 192.168.77.0/30.
	Subnet string
}

// New derives the device from workspace name and assigned subnet.
func New(workspace, vmSubnet string) (*Device, error) {
	hostIP, err := hostOfSubnet(vmSubnet)
	if err != nil {
		return nil, err
	}
	return &Device{
		Name:     "tap-" + workspace,
		HostCIDR: hostIP + "/30",
		Subnet:   vmSubnet,
	}, nil
}

func hostOfSubnet(subnet string) (string, error) {
	i := strings.IndexByte(subnet, '/')
	if i < 0 || !strings.HasSuffix(subnet, "/30") {
		return "", fmt.Errorf("subnet %q is not a /30", subnet)
	}
	base := subnet[:i]
	dots := strings.Count(base, ".")
	if dots != 3 {
		return "", fmt.Errorf("subnet %q is not IPv4", subnet)
	}
	octets := strings.Split(base, ".")
	last := 0
	if _, err := fmt.Sscanf(octets[3], "%d", &last); err != nil {
		return "", fmt.Errorf("subnet %q: bad last octet", subnet)
	}
	if last+1 > 255 {
		return "", fmt.Errorf("subnet %q: host address overflow", subnet)
	}
	octets[3] = fmt.Sprintf("%d", last+1)
	return strings.Join(octets, "."), nil
}

// Ensure creates the tap and its address if missing (idempotent), brings it
// up, and installs the per-subnet MASQUERADE rule.
func (d *Device) Ensure(ctx context.Context, run hostcmd.Runner) error {
	// ip link show fails if missing → add. Detect rather than tolerating error text.
	if _, err := run.Run(ctx, "ip", "link", "show", d.Name); err != nil {
		if _, err := run.Run(ctx, "ip", "tuntap", "add", "dev", d.Name, "mode", "tap"); err != nil {
			return fmt.Errorf("tuntap add %s: %w", d.Name, err)
		}
	}
	if _, err := run.Run(ctx, "ip", "addr", "replace", d.HostCIDR, "dev", d.Name); err != nil {
		return fmt.Errorf("addr replace %s on %s: %w", d.HostCIDR, d.Name, err)
	}
	if _, err := run.Run(ctx, "ip", "link", "set", d.Name, "up"); err != nil {
		return fmt.Errorf("link set %s up: %w", d.Name, err)
	}
	// NAT: masquerade this workspace's subnet when leaving the tap.
	rule := []string{"-t", "nat", "-C", "POSTROUTING", "-s", d.Subnet, "!", "-o", d.Name, "-j", "MASQUERADE"}
	if _, err := run.Run(ctx, "iptables", rule...); err != nil {
		if _, err := run.Run(ctx, "iptables", "-t", "nat", "-A", "POSTROUTING", "-s", d.Subnet, "!", "-o", d.Name, "-j", "MASQUERADE"); err != nil {
			return fmt.Errorf("masquerade %s: %w", d.Subnet, err)
		}
	}
	return nil
}

// Teardown removes the NAT rule and the device (idempotent).
func (d *Device) Teardown(ctx context.Context, run hostcmd.Runner) error {
	if _, err := run.Run(ctx, "iptables",
		"-t", "nat", "-D", "POSTROUTING", "-s", d.Subnet, "!", "-o", d.Name, "-j", "MASQUERADE"); err != nil {
		// -D on absent rule errors; absence is the desired end state.
		_ = err
	}
	if _, err := run.Run(ctx, "ip", "link", "del", d.Name); err != nil {
		_ = err // missing device is fine
	}
	return nil
}

// TeardownByName removes just the device (orphan cleanup without a known
// subnet; stale MASQUERADE rules are cleared by node reboot or a later
// full teardown).
func TeardownByName(ctx context.Context, run hostcmd.Runner, name string) error {
	_, _ = run.Run(ctx, "ip", "link", "del", "tap-"+name)
	return nil
}
