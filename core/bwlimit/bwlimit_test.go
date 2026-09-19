package bwlimit

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestParseKbit(t *testing.T) {
	ok := map[string]int{
		"1M":      1000,
		"1.5M":    1500,
		"2M":      2000,
		"2m":      2000,
		"500K":    500,
		"500k":    500,
		"2":       2000,
		"1G":      1000000,
		"1.5MB":   1500,
		"1Mbps":   1000,
		"2 Mbit":  2000,
		"  1.5m ": 1500,
		"0.5M":    500,
	}
	for group, want := range ok {
		got, err := ParseKbit(group)
		if err != nil {
			t.Errorf("ParseKbit(%q) failed: %v", group, err)
			continue
		}
		if got != want {
			t.Errorf("ParseKbit(%q) = %d, want %d", group, got, want)
		}
	}

	for _, group := range []string{"", "  ", "unlimited", "vip", "M", "-1M", "1T", "0", "1.5.5M", "0M"} {
		if got, err := ParseKbit(group); err == nil {
			t.Errorf("ParseKbit(%q) = %d, want an error", group, got)
		}
	}
}

func TestGroupCapsUnreadableGroups(t *testing.T) {
	if _, ok := Group(""); ok {
		t.Fatal("an empty group must not be capped")
	}
	if _, ok := Group("   "); ok {
		t.Fatal("a blank group must not be capped")
	}

	limit, ok := Group("1.5M")
	if !ok {
		t.Fatal("1.5M must be capped")
	}
	if limit.Up != 1500 || limit.Down != 1500 {
		t.Fatalf("1.5M = %+v, want 1500 both ways", limit)
	}

	limit, ok = Group("VIP")
	if !ok {
		t.Fatal("an unreadable group must still be capped")
	}
	if limit.Up != DefaultKbit || limit.Down != DefaultKbit {
		t.Fatalf("VIP = %+v, want the default cap", limit)
	}
}

func TestCanonicalGroup(t *testing.T) {
	if group, changed := CanonicalGroup(""); changed || group != "" {
		t.Fatalf("empty group became %q (changed=%v)", group, changed)
	}
	if group, changed := CanonicalGroup("1.5M"); changed || group != "1.5M" {
		t.Fatalf("1.5M became %q (changed=%v)", group, changed)
	}
	if group, changed := CanonicalGroup("VIP"); !changed || group != DefaultGroup {
		t.Fatalf("VIP became %q (changed=%v), want %q", group, changed, DefaultGroup)
	}
	if group, changed := CanonicalGroup(""); changed {
		t.Fatalf("empty group reported a change to %q", group)
	}
}

func TestWrapSkipsClientsWithoutCap(t *testing.T) {
	Reload(map[string]Limit{"1m": {Up: 1000, Down: 1000}, "unlimited": {}})

	conn := &fakeConn{}
	if got := Wrap("unlimited", conn); got != conn {
		t.Fatal("a client with no cap must not be wrapped")
	}
	if got := Wrap("nobody", conn); got != conn {
		t.Fatal("an unknown client must not be wrapped")
	}
	if got := Wrap("", conn); got != conn {
		t.Fatal("a connection without a user must not be wrapped")
	}
	if got := Wrap("1m", conn); got == conn {
		t.Fatal("a capped client must be wrapped")
	}
}

func TestWrapSharesOneBucketPerClient(t *testing.T) {
	Reload(map[string]Limit{"alice": {Up: 1000, Down: 1000}, "bob": {Up: 1000, Down: 1000}})

	first := Wrap("alice", &fakeConn{}).(*limitedConn)
	second := Wrap("alice", &fakeConn{}).(*limitedConn)
	other := Wrap("bob", &fakeConn{}).(*limitedConn)

	if first.caps != second.caps {
		t.Fatal("two connections of one client must share a bucket")
	}
	if first.caps == other.caps {
		t.Fatal("different clients must not share a bucket")
	}
}

func TestReloadReshapesLiveBuckets(t *testing.T) {
	Reload(map[string]Limit{"alice": {Up: 8000, Down: 8000}})
	conn := Wrap("alice", &fakeConn{}).(*limitedConn)
	up, _ := conn.caps.limiters()
	if got := int(up.Limit()); got != 1000000 {
		t.Fatalf("8M = %d bytes/s, want 1000000", got)
	}

	// A tighter cap reaches connections that are already open.
	Reload(map[string]Limit{"alice": {Up: 800, Down: 800}})
	up, down := conn.caps.limiters()
	if got := int(up.Limit()); got != 100000 {
		t.Fatalf("after reload up = %d bytes/s, want 100000", got)
	}
	if got := int(down.Limit()); got != 100000 {
		t.Fatalf("after reload down = %d bytes/s, want 100000", got)
	}

	// Dropping the cap stops limiting without touching the connection.
	Reload(map[string]Limit{})
	up, down = conn.caps.limiters()
	if up.Limit() != rate.Inf || down.Limit() != rate.Inf {
		t.Fatalf("a removed cap still limits: up=%v down=%v", up.Limit(), down.Limit())
	}
	if _, ok := Get("alice"); ok {
		t.Fatal("a client without a cap must not keep a bucket")
	}
}

// TestWrapThrottlesBothDirections drives a capped connection over a pipe and
// checks that both directions are held to the bucket. The buckets are built by
// hand with a small burst so the test runs in milliseconds.
func TestWrapThrottlesBothDirections(t *testing.T) {
	client, server := net.Pipe()
	defer client.Close()

	c := &caps{up: rate.NewLimiter(rate.Limit(20000), 1000), down: rate.NewLimiter(rate.Limit(20000), 1000)}
	conn := newLimitedConn(server, c)
	defer conn.Close()

	const chunk = 1000
	go func() {
		// The peer has to take what is written, or net.Pipe blocks on the
		// receiver instead of the bucket.
		buf := make([]byte, chunk)
		for i := 0; i < 4; i++ {
			if _, err := io.ReadFull(client, buf); err != nil {
				return
			}
		}
	}()

	start := time.Now()
	payload := make([]byte, chunk)
	for i := 0; i < 4; i++ {
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	// The first 1000 bytes are the burst, the next three cost 150ms at 20kB/s.
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("download of 4kB at 20kB/s took %v, expected the bucket to hold it back", elapsed)
	}
}

func TestWaitSplitsMoreThanTheBurst(t *testing.T) {
	// A wait for more than the burst must not fail: it is split into pieces.
	l := rate.NewLimiter(rate.Limit(100000), 64)
	start := time.Now()
	wait(context.Background(), l, 1000)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waiting for 1000 bytes at 100kB/s took %v", elapsed)
	}

	// An unlimited bucket returns at once, whatever the size.
	wait(context.Background(), rate.NewLimiter(rate.Inf, 0), 1<<20)
}

func TestCloseReleasesWaits(t *testing.T) {
	conn := newLimitedConn(&fakeConn{}, &caps{
		up:   rate.NewLimiter(rate.Limit(1), 1),
		down: rate.NewLimiter(rate.Limit(1), 1),
	}).(*limitedConn)

	done := make(chan struct{})
	go func() {
		defer close(done)
		wait(conn.ctx, conn.caps.up, 1<<20)
	}()
	time.Sleep(20 * time.Millisecond)
	if err := conn.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("closing the connection left a wait pending")
	}
}

func TestReloadIsConcurrencySafe(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				Reload(map[string]Limit{"alice": {Up: 1000 + j, Down: 1000 + j}})
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				conn := Wrap("alice", &fakeConn{})
				if conn != nil {
					conn.Write([]byte("x"))
				}
			}
		}()
	}
	wg.Wait()
}

// fakeConn is a connection that discards everything, for the paths where only
// the wrapper's bookkeeping matters.
type fakeConn struct{ net.Conn }

func (c *fakeConn) Read(p []byte) (int, error)       { return 0, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *fakeConn) Close() error                     { return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }
