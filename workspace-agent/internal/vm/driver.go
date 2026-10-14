// Package vm is the Firecracker process seam. Driver keeps the concrete
// UDS/PUT protocol behind an interface so the manager state machine tests
// run without KVM.
package vm

import (
	"time"
)

// Config is everything Start needs (paths absolute, host-side).
type Config struct {
	// Workspace name (labels the process/logs).
	Workspace string
	// Binary is the firecracker binary path (template asset).
	Binary string
	// Kernel is the vmlinux path (template asset).
	Kernel string
	// Rootfs is the RO root template path.
	Rootfs string
	// ConfigDisk / DataDisk are the per-workspace disks.
	ConfigDisk string
	DataDisk   string
	// TapDevice is the host tap name for the virtio-net backend.
	TapDevice string
	// GuestMAC is the eth0 MAC (derived from the /30, stable per subnet).
	GuestMAC string
	// VCPUs / MemMib shape machine-config.
	VCPUs int32
	MemMib int32
	// RunDir holds the API socket, serial log, pid file.
	RunDir string
	// SerialLogMB rotates the serial log at 2×N MiB (0 = no rotation).
	SerialLogMB int
	// BootTimeout bounds InstanceStart + kernel boot.
	BootTimeout time.Duration
}

// Driver starts/stops one VM.
type Driver interface {
	// Start boots the VM (blocking through InstanceStart; the process runs on).
	Start(cfg Config) error
	// Stop shuts the VM down: graceful (CtrlAltDel) then SIGKILL after
	// grace; returns once the process is reaped. Safe when not running.
	Stop(grace time.Duration) error
	// Alive reports whether the firecracker process is running.
	Alive() bool
	// PID is the firecracker process id (0 when not running).
	PID() int
}
