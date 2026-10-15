package template

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeTemplate(t *testing.T, dir, tag string, corrupt string) string {
	t.Helper()
	tagDir := filepath.Join(dir, tag)
	require.NoError(t, os.MkdirAll(tagDir, 0o755))
	var sums string
	for _, name := range []string{rootfsName, kernelName, fcName} {
		content := "payload-of-" + name + "-" + tag
		if name == corrupt {
			content = "tampered"
		}
		require.NoError(t, os.WriteFile(filepath.Join(tagDir, name), []byte(content), 0o644))
		sum := sha256.Sum256([]byte("payload-of-" + name + "-" + tag))
		sums += hex.EncodeToString(sum[:]) + "  " + name + "\n"
	}
	require.NoError(t, os.WriteFile(filepath.Join(tagDir, sumsName), []byte(sums), 0o644))
	return tagDir
}

func TestResolveOK(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "0.5.0", "")
	a, err := Resolve(dir, "0.5.0")
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "0.5.0", rootfsName), a.RootfsPath)
	assert.FileExists(t, a.KernelPath)
	assert.FileExists(t, a.FcPath)
}

func TestResolveDetectsTamper(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "0.5.0", kernelName)
	_, err := Resolve(dir, "0.5.0")
	assert.ErrorContains(t, err, "sha256 mismatch")
}

func TestResolveMissingSums(t *testing.T) {
	dir := t.TempDir()
	_, err := Resolve(dir, "absent")
	assert.ErrorContains(t, err, "SHA256SUMS")
}

func TestGCKeepNewest(t *testing.T) {
	dir := t.TempDir()
	for _, tag := range []string{"0.4.0", "0.4.9", "0.5.0"} {
		writeTemplate(t, dir, tag, "")
	}
	require.NoError(t, GC(dir, 2))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	assert.Equal(t, []string{"0.4.9", "0.5.0"}, names, "oldest dropped, keep-2")
}

func TestResolveRejectsPathAndMissingMetadataChecksum(t *testing.T) {
	_, err := Resolve(t.TempDir(), "../outside")
	require.Error(t, err)
}

func TestDesktopMetadataIsChecksumBound(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "desktop-v1", "")
	tagDir := filepath.Join(dir, "desktop-v1")
	assets, err := Resolve(dir, "desktop-v1")
	require.NoError(t, err)
	require.False(t, assets.DesktopAvailable)
	metadata := []byte(`{"tag":"desktop-v1","desktop":true}`)
	require.NoError(t, os.WriteFile(filepath.Join(tagDir, "template.json"), metadata, 0644))
	sum := sha256.Sum256(metadata)
	sums, err := os.ReadFile(filepath.Join(tagDir, "SHA256SUMS"))
	require.NoError(t, err)
	sums = append(sums, []byte(hex.EncodeToString(sum[:])+"  template.json\n")...)
	require.NoError(t, os.WriteFile(filepath.Join(tagDir, "SHA256SUMS"), sums, 0644))
	assets, err = Resolve(dir, "desktop-v1")
	require.NoError(t, err)
	require.True(t, assets.DesktopAvailable)
	require.NoError(t, os.WriteFile(filepath.Join(tagDir, "template.json"), []byte(`{}`), 0644))
	_, err = Resolve(dir, "desktop-v1")
	require.Error(t, err)
}
