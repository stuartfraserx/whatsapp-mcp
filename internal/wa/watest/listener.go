package watest

import (
	"context"
	"net"
	"sync"
)

// Listener supplies an HTTP test server with in-memory connections, without
// requiring network privileges. It can be installed on httptest's unstarted server.
type Listener struct {
	connections chan net.Conn
	done        chan struct{}
	once        sync.Once
}

func NewListener() *Listener {
	return &Listener{connections: make(chan net.Conn), done: make(chan struct{})}
}
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case c := <-l.connections:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *Listener) Close() error { l.once.Do(func() { close(l.done) }); return nil }
func (*Listener) Addr() net.Addr { return memoryAddr{} }

type memoryAddr struct{}

func (memoryAddr) Network() string { return "tcp" }
func (memoryAddr) String() string  { return "127.0.0.1:8765" }
func (l *Listener) DialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case l.connections <- server:
		return client, nil
	case <-ctx.Done():
		client.Close()
		server.Close()
		return nil, ctx.Err()
	case <-l.done:
		client.Close()
		server.Close()
		return nil, net.ErrClosed
	}
}
