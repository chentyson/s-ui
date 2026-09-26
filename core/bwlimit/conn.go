package bwlimit

import (
	"context"
	"net"
	"sync"
)

// limitedConn meters a client's connection against the buckets it shares with
// every other connection of that client.
type limitedConn struct {
	net.Conn
	caps   *caps
	ctx    context.Context
	cancel context.CancelFunc
	once   sync.Once
}

func newLimitedConn(conn net.Conn, c *caps) net.Conn {
	ctx, cancel := context.WithCancel(context.Background())
	return &limitedConn{Conn: conn, caps: c, ctx: ctx, cancel: cancel}
}

// Read takes what the client sent: its upload.
func (l *limitedConn) Read(p []byte) (int, error) {
	n, err := l.Conn.Read(p)
	if n > 0 {
		up, _ := l.caps.limiters()
		wait(l.ctx, up, n)
	}
	return n, err
}

// Write sends towards the client: its download.
func (l *limitedConn) Write(p []byte) (int, error) {
	n, err := l.Conn.Write(p)
	if n > 0 {
		_, down := l.caps.limiters()
		wait(l.ctx, down, n)
	}
	return n, err
}

// Close cancels waits a closed connection can no longer satisfy, then closes
// the connection underneath.
func (l *limitedConn) Close() error {
	l.once.Do(l.cancel)
	return l.Conn.Close()
}

// CloseWrite and CloseRead keep the half-closes the router performs working
// through the wrapper. The connection underneath is not required to support
// them -- the QUIC streams hysteria2/tuic hand us have neither -- and closing
// the whole stream instead of half-closing it cuts the other direction short,
// so a half-close the wrapped type does not have stays a no-op.
func (l *limitedConn) CloseWrite() error {
	if cw, ok := l.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func (l *limitedConn) CloseRead() error {
	if cr, ok := l.Conn.(interface{ CloseRead() error }); ok {
		return cr.CloseRead()
	}
	return nil
}

// Upstream hands the connection underneath to sing's unwrap helpers, the way
// every other wrapper in the core does, so wrapping stays invisible to them.
func (l *limitedConn) Upstream() any {
	return l.Conn
}
