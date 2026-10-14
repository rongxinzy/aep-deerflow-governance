package disk

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/rongxinzy/aep-deerflow-governance/operator/api/v1alpha1"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/hostcmd"
	"github.com/rongxinzy/aep-deerflow-governance/workspace-agent/internal/render"
)

func payloadFor(t *testing.T) *render.ConfigPayload {
	t.Helper()
	ws := &v1alpha1.PersonalWorkspace{
		ObjectMeta: metav1.ObjectMeta{Name: "ws1"},
		Spec:       v1alpha1.PersonalWorkspaceSpec{Companions: []v1alpha1.CompanionSpec{{Name: "assistant"}}},
		Status:     v1alpha1.PersonalWorkspaceStatus{VMSubnet: "192.168.77.0/30"},
	}
	p, err := render.BuildPayload(render.Inputs{
		Workspace:    ws,
		Manifest:     `{"companions":[{"name":"assistant"}]}`,
		ModelBaseURL: "http://gw/v1",
		DNSIP:        "10.43.0.10",
	})
	require.NoError(t, err)
	return p
}

func TestEnsureDataDiskCreatedOnce(t *testing.T) {
	run := &hostcmd.FakeRunner{}
	img := &Images{Run: run}
	dir := t.TempDir()
	path := filepath.Join(dir, "data.ext4")

	require.NoError(t, img.EnsureDataDisk(context.Background(), path, 5<<30))
	assert.FileExists(t, path)
	mkfsCount := strings.Count(strings.Join(run.Calls, "\n"), "mkfs.ext4")
	assert.Equal(t, 1, mkfsCount)

	// Second call is a no-op (persistence contract).
	require.NoError(t, img.EnsureDataDisk(context.Background(), path, 5<<30))
	assert.Equal(t, 1, strings.Count(strings.Join(run.Calls, "\n"), "mkfs.ext4"))
}

func TestWriteConfigDiskAtomicAndScoped(t *testing.T) {
	var envMode os.FileMode
	run := &hostcmd.FakeRunner{Response: func(name string, args []string) (string, error) {
		// Staging exists only during the mkfs call — assert the env file's
		// perms right here, at call time.
		if name == "mkfs.ext4" {
			for i, a := range args {
				if a == "-d" && i+1 < len(args) {
					if info, err := os.Stat(filepath.Join(args[i+1], "etc/workspace/env")); err == nil {
						envMode = info.Mode().Perm()
					}
				}
			}
		}
		return "", nil
	}}
	img := &Images{Run: run}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.ext4")

	require.NoError(t, img.WriteConfigDisk(context.Background(), path, payloadFor(t)))
	assert.FileExists(t, path)
	assert.NoFileExists(t, path+".tmp", "temp file renamed away")
	calls := strings.Join(run.Calls, "\n")
	assert.Contains(t, calls, "mkfs.ext4 -q -F -L cfg -d", "populated without loop mount")
	assert.Contains(t, calls, "config.ext4.tmp", "written to sibling temp first")
	assert.Equal(t, os.FileMode(0o600), envMode, "env (secrets) staged 0600")
}
