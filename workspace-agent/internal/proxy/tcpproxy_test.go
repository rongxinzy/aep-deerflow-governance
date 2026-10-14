package proxy

import (
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProxySplicesBothDirectionsAndStops(t *testing.T) {
	// backend echoes everything uppercased.
	backend, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer backend.Close()
	go func() {
		for {
			conn, err := backend.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 64)
				for {
					n, err := c.Read(buf)
					if err != nil {
						return
					}
					out := make([]byte, n)
					for i := 0; i < n; i++ {
						if buf[i] >= 'a' && buf[i] <= 'z' {
							out[i] = buf[i] - 32
						} else {
							out[i] = buf[i]
						}
					}
					if _, err := c.Write(out); err != nil {
						return
					}
				}
			}(conn)
		}
	}()

	p := New("127.0.0.1:0", backend.Addr().String())
	require.NoError(t, p.Start())
	defer p.Stop()

	conn, err := net.Dial("tcp", p.Addr())
	require.NoError(t, err)
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Write([]byte("stream-through"))
	require.NoError(t, err)
	buf := make([]byte, 64)
	n, err := io.ReadFull(conn, buf[:len("stream-through")])
	require.NoError(t, err)
	assert.Equal(t, "STREAM-THROUGH", string(buf[:n]))

	p.Stop()
	// After Stop, new dials fail (listener closed).
	_, err = net.DialTimeout("tcp", p.Addr(), 300*time.Millisecond)
	assert.Error(t, err)
}
