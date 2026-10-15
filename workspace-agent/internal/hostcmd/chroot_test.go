package hostcmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestHostExecutableUsesMountedHost(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "usr", "sbin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mkfs.ext4"), []byte("fixture"), 0755); err != nil {
		t.Fatal(err)
	}
	path, err := hostExecutable(root, "mkfs.ext4")
	if err != nil || path != "/usr/sbin/mkfs.ext4" {
		t.Fatalf("path=%q err=%v", path, err)
	}
	// The parent's PATH must not supply commands missing from the mounted host.
	if _, err := hostExecutable(root, "sh"); err == nil {
		t.Fatal("used container command")
	}
}

func TestHostExecutablePreservesAbsoluteHostSymlink(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "usr", "sbin")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/alternatives/iptables", filepath.Join(dir, "iptables")); err != nil {
		t.Fatal(err)
	}
	path, err := hostExecutable(root, "iptables")
	if err != nil || path != "/usr/sbin/iptables" {
		t.Fatalf("path=%q err=%v", path, err)
	}
}
