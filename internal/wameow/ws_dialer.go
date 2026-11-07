package wameow

import (
	"context"
	"net"
	"time"

	"go.mau.fi/whatsmeow"
)

// Track every live dial, including an in-flight replacement, so recovery cannot
// miss a socket while whatsmeow is reconnecting in the background.
func (c *Client) installWSDialer() {
	c.cli.SetSOCKSProxy(&trackedWSDialer{owner: c, dialer: net.Dialer{
		Timeout: 15 * time.Second, KeepAliveConfig: net.KeepAliveConfig{
			Enable: true, Idle: 15 * time.Second, Interval: 10 * time.Second, Count: 3,
		}, Control: tcpUserTimeout,
	}}, whatsmeow.SetProxyOptions{NoMedia: true})
}

// SetSOCKSProxy accepts a generic context dialer; no SOCKS protocol is added.
// whatsmeow retains its default HTTP transport and environment proxy behavior.
type trackedWSDialer struct {
	owner  *Client
	dialer net.Dialer
}

func (d *trackedWSDialer) Dial(network, address string) (net.Conn, error) {
	return d.DialContext(context.Background(), network, address)
}
func (d *trackedWSDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	c := d.owner

	conn, err := d.dialer.DialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	tracked := &trackedWSConn{Conn: conn, owner: c}
	c.rawMu.Lock()
	if c.rawConns == nil {
		c.rawConns = make(map[net.Conn]struct{})
	}
	c.rawConns[tracked] = struct{}{}
	c.rawMu.Unlock()
	return tracked, nil
}

type trackedWSConn struct {
	net.Conn
	owner *Client
}

func (conn *trackedWSConn) Close() error {
	conn.owner.rawMu.Lock()
	delete(conn.owner.rawConns, conn)
	conn.owner.rawMu.Unlock()
	return conn.Conn.Close()
}
func (c *Client) forceCloseRaw() {
	c.rawMu.Lock()
	conns := c.rawConns
	c.rawConns = nil
	c.rawMu.Unlock()
	for conn := range conns {
		_ = conn.Close()
	}
}
