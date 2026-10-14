package vm

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Regression: pid 0 (missing pid file) must NOT report alive —
// syscall.Kill(0, 0) checks the caller's own process group and succeeds
// unconditionally, which made the manager skip booting forever.
func TestAliveWithoutPidFileIsFalse(t *testing.T) {
	f := NewFirecracker(t.TempDir())
	assert.Equal(t, 0, f.PID())
	assert.False(t, f.Alive(), "no pid file => not alive (pid 0 kills the process group probe)")
}

func TestAliveWithSelfPid(t *testing.T) {
	dir := t.TempDir()
	// Use our own pid: signal 0 to a live process succeeds (we are alive).
	f := NewFirecracker(dir)
	require.NoError(t, writeFile(filepath.Join(dir, "fc.pid"), itoa(osGetpid())))
	assert.True(t, f.Alive())
}

func TestAliveWithDeadPid(t *testing.T) {
	dir := t.TempDir()
	f := NewFirecracker(dir)
	// pid 300000000: beyond any realistic pid, kill(2) must fail (ESRCH).
	require.NoError(t, writeFile(filepath.Join(dir, "fc.pid"), "300000000"))
	assert.False(t, f.Alive())
}
