// Package testhttp runs httptest servers over in-memory connections, including
// in restricted environments that cannot bind loopback sockets.
package testhttp

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"time"
)

// NewServer starts a real HTTP server backed by net.Pipe instead of TCP.
// Use its Client method: the default transport cannot reach its virtual address.
func NewServer(handler http.Handler) *httptest.Server {
	listener := &pipeListener{connections: make(chan net.Conn), done: make(chan struct{})}
	server := &httptest.Server{Listener: listener, Config: &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second}}
	server.Start()
	// httptest.Client's transport is owned by the server and closed by Close.
	transport := server.Client().Transport.(*http.Transport)
	transport.DialContext = func(ctx context.Context, _, _ string) (net.Conn, error) {
		client, peer := net.Pipe()
		select {
		case listener.connections <- peer:
			return client, nil
		case <-ctx.Done():
			_ = client.Close()
			_ = peer.Close()
			return nil, ctx.Err()
		case <-listener.done:
			_ = client.Close()
			_ = peer.Close()
			return nil, net.ErrClosed
		}
	}
	return server
}

type pipeListener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func (l *pipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-l.connections:
		return conn, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *pipeListener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*pipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 80} }
