package alloc

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocateFreeBasics(t *testing.T) {
	port, err := AllocateFree(nil, 8800, 8899, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(8800), port)

	port, err = AllocateFree([]int32{8800, 8801}, 8800, 8899, nil)
	require.NoError(t, err)
	assert.Equal(t, int32(8802), port)
}

func TestAllocateFreeRespectsBindProbe(t *testing.T) {
	// 8800 squatted by something outside the CR world: probe rejects it.
	port, err := AllocateFree(nil, 8800, 8899, func(p int32) bool { return p != 8800 })
	require.NoError(t, err)
	assert.Equal(t, int32(8801), port)
}

func TestAllocateFreeExhausted(t *testing.T) {
	_, err := AllocateFree([]int32{8800, 8801}, 8800, 8801, nil)
	assert.ErrorContains(t, err, "no free port")
}

func TestAllocateFreeInvertedRange(t *testing.T) {
	_, err := AllocateFree(nil, 8900, 8800, nil)
	assert.ErrorContains(t, err, "inverted")
}
