package vm

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Firecracker drives one firecracker process over its UDS API (v1.17).
type Firecracker struct {
	apiSock string
	pidFile string
	serial  string
	cmd     *exec.Cmd
}

// NewFirecracker builds a driver bound to a run dir (api.sock / fc.pid /
// serial.log inside).
func NewFirecracker(runDir string) *Firecracker {
	return &Firecracker{
		apiSock: filepath.Join(runDir, "fc.sock"),
		pidFile: filepath.Join(runDir, "fc.pid"),
		serial:  filepath.Join(runDir, "serial.log"),
	}
}

// bootArgs is the golden kernel cmdline. Root is RO (long-term contract);
// virtio_mmio devices at fixed slots: net=5 root=6 config=7 data=8.
func bootArgs() string {
	return "console=ttyS0 reboot=k panic=1 pci=off root=/dev/vda ro init=/init " +
		"random.trust_cpu=on " +
		"virtio_mmio.device=4K@0xc0001000:{5} " +
		"virtio_mmio.device=4K@0xc0002000:{6} " +
		"virtio_mmio.device=4K@0xc0003000:{7} " +
		"virtio_mmio.device=4K@0xc0004000:{8}"
}

// Start boots the VM: spawn process, PUT the device model, InstanceStart.
func (f *Firecracker) Start(cfg Config) error {
	_ = os.Remove(f.apiSock)
	if err := os.MkdirAll(filepath.Dir(f.apiSock), 0o755); err != nil {
		return err
	}
	if err := rotateIfLarge(f.serial, cfg.SerialLogMB); err != nil {
		return err
	}
	serialOut, err := os.OpenFile(f.serial, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer serialOut.Close()

	cmd := exec.Command(cfg.Binary, "--api-sock", f.apiSock)
	cmd.Stdout = serialOut
	cmd.Stderr = serialOut
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("spawn firecracker: %w", err)
	}
	f.cmd = cmd
	if err := os.WriteFile(f.pidFile, []byte(strconv.Itoa(cmd.Process.Pid)), 0o644); err != nil {
		return err
	}

	// Reap the process when it exits; Stop() kills it explicitly.
	go func() { _ = cmd.Wait() }()

	ctx, cancel := context.WithTimeout(context.Background(), cfg.BootTimeout)
	defer cancel()
	if err := waitForSock(ctx, f.apiSock); err != nil {
		_ = f.Stop(2 * time.Second)
		return fmt.Errorf("api sock: %w", err)
	}

	type drive struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	}
	puts := []struct {
		path string
		body any
	}{
		{"boot-source", map[string]any{
			"kernel_image_path": cfg.Kernel,
			"boot_args":         bootArgs(),
		}},
		{"machine-config", map[string]any{
			"vcpu_count": cfg.VCPUs,
			"mem_size_mib": cfg.MemMib,
			"smt": false,
		}},
		{"drives/rootfs", drive{DriveID: "rootfs", PathOnHost: cfg.Rootfs, IsRootDevice: true, IsReadOnly: true}},
		{"drives/config", drive{DriveID: "config", PathOnHost: cfg.ConfigDisk, IsReadOnly: true}},
		{"drives/data", drive{DriveID: "data", PathOnHost: cfg.DataDisk}},
		{"network-interfaces/eth0", map[string]any{
			"iface_id":      "eth0",
			"guest_mac":     cfg.GuestMAC,
			"host_dev_name": cfg.TapDevice,
		}},
		{"serial", map[string]any{"serial_out_path": f.serial}},
	}
	client := f.httpClient(serialOut)
	for _, p := range puts {
		body, _ := json.Marshal(p.body)
		req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://fc/"+p.path, strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			_ = f.Stop(2 * time.Second)
			return fmt.Errorf("PUT /%s: %w", p.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode >= 300 {
			_ = f.Stop(2 * time.Second)
			return fmt.Errorf("PUT /%s: status %d", p.path, resp.StatusCode)
		}
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodPut, "http://fc/actions", strings.NewReader(`{"action_type":"InstanceStart"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		_ = f.Stop(2 * time.Second)
		return fmt.Errorf("InstanceStart: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode >= 300 {
		_ = f.Stop(2 * time.Second)
		return fmt.Errorf("InstanceStart: status %d", resp.StatusCode)
	}
	return nil
}

// Stop: CtrlAltDel (clean shutdown lets the guest init sync), then SIGKILL.
func (f *Firecracker) Stop(grace time.Duration) error {
	pid := f.PID()
	if pid == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://fc/actions", strings.NewReader(`{"action_type":"SendCtrlAltDel"}`)); err == nil {
		req.Header.Set("Content-Type", "application/json")
		if resp, err := f.httpClient(nil).Do(req); err == nil {
			_ = resp.Body.Close()
		}
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) && syscall.Kill(pid, 0) == nil {
		time.Sleep(100 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	// Wait for reaping (the goroutine from Start owns Wait; poll liveness).
	for i := 0; i < 50 && syscall.Kill(pid, 0) == nil; i++ {
		time.Sleep(100 * time.Millisecond)
	}
	_ = os.Remove(f.pidFile)
	return nil
}

// Alive reports process liveness.
func (f *Firecracker) Alive() bool {
	return syscall.Kill(f.PID(), 0) == nil
}

// PID reads the recorded pid (0 when absent or dead-unknown).
func (f *Firecracker) PID() int {
	if f.cmd != nil && f.cmd.Process != nil {
		return f.cmd.Process.Pid
	}
	data, err := os.ReadFile(f.pidFile)
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0
	}
	return pid
}

func (f *Firecracker) httpClient(serialOut *os.File) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", f.apiSock)
			},
		},
	}
}

func waitForSock(ctx context.Context, path string) error {
	deadline, _ := ctx.Deadline()
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("timeout waiting for %s", path)
}

// rotateIfLarge keeps serial logs bounded: at 2×N MiB rotate to .1 (one
// generation back).
func rotateIfLarge(path string, mb int) error {
	if mb <= 0 {
		return nil
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() < int64(mb)*2<<20 {
		return nil
	}
	_ = os.Remove(path + ".1")
	return os.Rename(path, path+".1")
}
