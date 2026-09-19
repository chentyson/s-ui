// Package bwlimit holds the per-client bandwidth caps that the core applies to
// the connections it proxies.
//
// A cap is not configured here but read from the client's group: a group that
// reads as a bandwidth ("1M", "1.5M", "500K") caps that client's upload and
// download at it, and an empty group leaves the client unlimited. The panel
// pushes the resulting table into this registry whenever it rebuilds the config
// and after every save, so the hot connection path never reads the database.
//
// Enforcement is one token bucket per client that every connection the client
// opens shares, so the cap covers the client's total bandwidth rather than each
// connection separately -- a client that opens ten streams does not get ten
// times the cap. The bucket is userspace: an idle client accumulates up to one
// second of its cap and may spend it in a burst, while a client that keeps a
// queue full is held at the cap. Traffic is metered after it has moved -- a read
// from the client is its upload, a write to it is its download -- so one
// buffer's worth can slip through per connection before the first bucket
// empties.
package bwlimit

import (
	"context"
	"net"
	"sync"

	"golang.org/x/time/rate"
)

// Limit is a client's caps in kbit/s; 0 leaves that direction unlimited.
type Limit struct {
	Up   int
	Down int
}

func (l Limit) empty() bool { return l.Up <= 0 && l.Down <= 0 }

var (
	mu     sync.RWMutex
	limits = make(map[string]Limit)
	active = make(map[string]*caps)
)

// Reload replaces the table of caps. Buckets already handed to live connections
// are reshaped in place, so a new cap applies to them too and a cap that is gone
// stops limiting at once.
func Reload(m map[string]Limit) {
	mu.Lock()
	defer mu.Unlock()
	next := make(map[string]Limit, len(m))
	for user, l := range m {
		if l.empty() {
			continue
		}
		next[user] = l
	}
	limits = next
	for user, c := range active {
		l, ok := next[user]
		if !ok {
			delete(active, user)
			c.set(Limit{})
			continue
		}
		c.set(l)
	}
}

// Get returns the buckets shared by every connection of user. ok is false when
// the client has no cap, in which case the connection must not be wrapped at
// all.
func Get(user string) (*caps, bool) {
	mu.RLock()
	l, ok := limits[user]
	mu.RUnlock()
	if !ok {
		return nil, false
	}
	mu.Lock()
	defer mu.Unlock()
	if c, exists := active[user]; exists {
		return c, true
	}
	c := newCaps(l)
	active[user] = c
	return c, true
}

// Configured returns the cap a client is configured with. The core reads caps
// through Get; this is for callers that need to report one -- the panel's client
// list, tests -- without holding a connection. ok is false when the client has
// no cap.
func Configured(user string) (Limit, bool) {
	mu.RLock()
	defer mu.RUnlock()
	l, ok := limits[user]
	return l, ok
}

// Wrap returns conn metered against user's cap, or conn itself when the client
// has none. The wrapper is transparent apart from charging byte movement:
// reading from conn is the client's upload, writing to it is its download.
func Wrap(user string, conn net.Conn) net.Conn {
	if conn == nil || user == "" {
		return conn
	}
	c, ok := Get(user)
	if !ok {
		return conn
	}
	return newLimitedConn(conn, c)
}

// caps holds one client's buckets. Live connections keep this pointer, so a
// reload swaps the buckets inside it and leaves their references alone.
type caps struct {
	mu   sync.RWMutex
	up   *rate.Limiter
	down *rate.Limiter
}

func newCaps(l Limit) *caps {
	return &caps{up: newLimiter(l.Up), down: newLimiter(l.Down)}
}

// set swaps in new buckets. A changed cap starts with an empty one, so a client
// cannot spend a burst it accumulated under the old cap.
func (c *caps) set(l Limit) {
	up, down := newLimiter(l.Up), newLimiter(l.Down)
	c.mu.Lock()
	c.up, c.down = up, down
	c.mu.Unlock()
}

func (c *caps) limiters() (up *rate.Limiter, down *rate.Limiter) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.up, c.down
}

// minBurst is the smallest bucket: a single read or write is a few kilobytes,
// and a wait that asked for more than the burst would fail outright, so the
// bucket never goes below a buffer.
const minBurst = 64 * 1024

// newLimiter builds a bucket for a kbit/s cap. A megabit is 1000 kbit and a byte
// is 8 bits, which is the same conversion sing-box uses for its own bandwidth
// options.
func newLimiter(kbit int) *rate.Limiter {
	if kbit <= 0 {
		return rate.NewLimiter(rate.Inf, 0)
	}
	bps := float64(kbit) * 125
	burst := int(bps)
	if burst < minBurst {
		burst = minBurst
	}
	return rate.NewLimiter(rate.Limit(bps), burst)
}

// wait charges n bytes to l. A wait that fails means the connection is closing,
// which the caller cannot act on: the bytes have already moved.
func wait(ctx context.Context, l *rate.Limiter, n int) {
	if l.Limit() == rate.Inf {
		return
	}
	for n > 0 {
		chunk := n
		if burst := l.Burst(); chunk > burst {
			chunk = burst
		}
		if chunk <= 0 {
			return
		}
		if err := l.WaitN(ctx, chunk); err != nil {
			return
		}
		n -= chunk
	}
}
