// Package proxy is the userspace TCP proxy exposing the in-VM gateway on a
// node host port: bidirectional byte splicing, no HTTP parsing (SSE is
// transparent by construction). Chosen over DNAT rules to keep zero iptables
// ingress state — agent restarts just re-bind.
package proxy

import (
	"io"
	"net"
	"sync"
)

// TCPProxy listens on addr and dials backend per connection.
type TCPProxy struct {
	addr    string
	backend string

	mu       sync.Mutex
	listener net.Listener
	stopped  bool
	conns    map[net.Conn]struct{}
}

// New builds a proxy (not started).
func New(addr, backend string) *TCPProxy {
	return &TCPProxy{addr: addr, backend: backend, conns: map[net.Conn]struct{}{}}
}

// Start binds and serves in the background. Addr() reports the actually
// bound address afterwards (so ":0" style addresses resolve).
func (p *TCPProxy) Start() error {
	ln, err := net.Listen("tcp", p.addr)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.listener = ln
	p.addr = ln.Addr().String()
	p.mu.Unlock()
	go p.serve(ln)
	return nil
}

func (p *TCPProxy) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			p.mu.Lock()
			stopped := p.stopped
			p.mu.Unlock()
			if stopped {
				return
			}
			continue
		}
		go p.pipe(conn)
	}
}

func (p *TCPProxy) pipe(client net.Conn) {
	upstream, err := net.Dial("tcp", p.backend)
	if err != nil {
		_ = client.Close()
		return
	}
	p.mu.Lock()
	p.conns[client] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, client)
		p.mu.Unlock()
		_ = client.Close()
		_ = upstream.Close()
	}()
	// Both directions concurrently; either close tears the other.
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(upstream, client); done <- struct{}{} }()
	go func() { _, _ = io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// Stop closes the listener and all live connections.
func (p *TCPProxy) Stop() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.stopped = true
	if p.listener != nil {
		_ = p.listener.Close()
	}
	for c := range p.conns {
		_ = c.Close()
	}
}

// Addr is the listen address (for tests/status).
func (p *TCPProxy) Addr() string { return p.addr }
