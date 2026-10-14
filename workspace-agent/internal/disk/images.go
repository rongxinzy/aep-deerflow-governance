// Package disk builds the per-workspace disks: the persistent data disk
// (created once, never rebuilt) and the config disk (16 MiB ext4, written
// only while the VM is stopped — hot-writing an attached disk corrupted
// ext4 in the PoC, the state machine guards this with DiskIO and tests
// lock it).
package disk

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/render"
)

// configDiskBytes is the config disk size (16 MiB): companions.json, env,
// resolv.conf — a few KiB with generous headroom.
const configDiskBytes = 16 << 20

// DiskIO is the disk operations seam (mock in tests).
//
//nolint:revive // interface name mirrors domain
type DiskIO interface {
	// EnsureDataDisk creates path as an ext4 image of sizeBytes if absent.
	// An existing disk is NEVER touched (persistence contract).
	EnsureDataDisk(ctx context.Context, path string, sizeBytes int64) error
	// WriteConfigDisk atomically writes the config disk from a payload.
	// Callers must only invoke it with the VM stopped.
	WriteConfigDisk(ctx context.Context, path string, payload *render.ConfigPayload) error
}

// Images implements DiskIO through the host Runner (mkfs.ext4 -d staging:
// populate without loop mounts).
type Images struct {
	Run hostcmd.Runner
	// StagingRoot holds config-disk staging trees. It MUST live under the
	// shared workdir (not /tmp): mkfs runs chrooted into the host view,
	// where the container's /tmp does not exist.
	StagingRoot string
}

// stagingDir builds the payload directory tree then lets mkfs -d copy it.
func (m *Images) stageConfig(ctx context.Context, payload *render.ConfigPayload) (string, func(), error) {
	root := m.StagingRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", nil, err
	}
	staging, err := os.MkdirTemp(root, "pw-cfg-")
	if err != nil {
		return "", nil, err
	}
	cleanup := func() { _ = os.RemoveAll(staging) }
	if err := os.MkdirAll(filepath.Join(staging, "etc/workspace"), 0o755); err != nil {
		cleanup()
		return "", nil, err
	}
	files := []struct {
		path string
		data []byte
		mode os.FileMode
	}{
		{filepath.Join("etc", "workspace", "companions.json"), payload.CompanionsJSON, 0o644},
		{filepath.Join("etc", "workspace", "env"), payload.EnvFile, 0o600},
		{filepath.Join("etc", "resolv.conf"), payload.ResolvConf, 0o644},
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(staging, f.path), f.data, f.mode); err != nil {
			cleanup()
			return "", nil, err
		}
	}
	return staging, cleanup, nil
}

// EnsureDataDisk implements DiskIO.
func (m *Images) EnsureDataDisk(ctx context.Context, path string, sizeBytes int64) error {
	if _, err := os.Stat(path); err == nil {
		return nil // existing disk is the persistence contract
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path+".tmp", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(sizeBytes); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := m.Run.Run(ctx, "mkfs.ext4", "-q", "-F", "-L", "data", path+".tmp"); err != nil {
		_ = os.Remove(path + ".tmp")
		return fmt.Errorf("mkfs data disk: %w", err)
	}
	return os.Rename(path+".tmp", path)
}

// WriteConfigDisk implements DiskIO: build into a sibling temp file, mkfs
// from staging, atomic rename over the previous config disk.
func (m *Images) WriteConfigDisk(ctx context.Context, path string, payload *render.ConfigPayload) error {
	staging, cleanup, err := m.stageConfig(ctx, payload)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if err := f.Truncate(configDiskBytes); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if _, err := m.Run.Run(ctx, "mkfs.ext4", "-q", "-F", "-L", "cfg", "-d", staging, tmp); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("mkfs config disk: %w", err)
	}
	return os.Rename(tmp, path)
}

// HasConfig returns whether the config disk file exists (boot-vs-first-boot).
func HasConfig(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
