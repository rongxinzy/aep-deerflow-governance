package manager

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGuestLifecycleTokenPersistsAcrossRebuilds(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "guest")
	first, err := guestLifecycleToken(dir)
	require.NoError(t, err)
	require.Len(t, first, 64)
	again, err := guestLifecycleToken(dir)
	require.NoError(t, err)
	require.Equal(t, first, again)
	info, err := os.Stat(filepath.Join(dir, "lifecycle.token"))
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	other, err := guestLifecycleToken(filepath.Join(t.TempDir(), "other"))
	require.NoError(t, err)
	require.NotEqual(t, first, other)
}
