package usersession

import "testing"

// The device limit is enforced against the user's devices across every
// inbound: one device on inbound A plus one on inbound B fills a limit of 2,
// and a third device anywhere is rejected.
func TestGlobalLimitSharedAcrossInbounds(t *testing.T) {
	inA := NewRegistry("inA")
	inB := NewRegistry("inB")

	if !inA.TryBind("alice", "10.0.0.1:1000", 2) {
		t.Fatal("expected the first device on inbound A to be accepted")
	}
	if !inB.TryBind("alice", "10.0.0.2:1000", 2) {
		t.Fatal("expected the second device on inbound B to be accepted")
	}
	if counts := GlobalDeviceCounts(); counts["alice"] != 2 {
		t.Fatalf("expected the global table to see 2 devices for alice, got %d", counts["alice"])
	}
	if inA.TryBind("alice", "10.0.0.3:1000", 2) {
		t.Fatal("expected the third device to be rejected: limit 2 is shared across inbounds")
	}
	if inB.TryBind("alice", "10.0.0.3:1000", 2) {
		t.Fatal("expected the third device to be rejected on the other inbound too")
	}
	// A second connection from an already-counted device is a refcount bump,
	// not a new device, on either inbound.
	if !inA.TryBind("alice", "10.0.0.1:1000", 2) {
		t.Fatal("expected an extra connection from a counted device to be allowed")
	}
	if counts := GlobalDeviceCounts(); counts["alice"] != 2 {
		t.Fatalf("expected the count to stay 2, got %d", counts["alice"])
	}
}

// Closing an inbound (or the user's session on it) releases the device slots
// it held, freeing room on every other inbound.
func TestGlobalSlotsReleasedOnInboundClose(t *testing.T) {
	inA := NewRegistry("inA")
	inB := NewRegistry("inB")

	if !inA.TryBind("bob", "10.0.0.1:1000", 2) || !inB.TryBind("bob", "10.0.0.2:1000", 2) {
		t.Fatal("expected both devices to be accepted")
	}
	if inA.TryBind("bob", "10.0.0.3:1000", 2) {
		t.Fatal("expected the limit to be full before inbound A closes")
	}

	// Inbound A goes away: its slots must leave the global table.
	inA.Close()
	if counts := GlobalDeviceCounts(); counts["bob"] != 1 {
		t.Fatalf("expected 1 device left for bob after closing inbound A, got %d", counts["bob"])
	}
	if !inB.TryBind("bob", "10.0.0.3:1000", 2) {
		t.Fatal("expected a new device on inbound B to fit after inbound A closed")
	}

	// Connection close releases one device too.
	inB.Untrack("10.0.0.2:1000")
	if counts := GlobalDeviceCounts(); counts["bob"] != 1 {
		t.Fatalf("expected untrack to release one slot, got %d", counts["bob"])
	}
}

// A user removing a session through CloseUsers / KickUserSessions releases the
// slot; a muted session (no closer) keeps the per-inbound semantics it had.
func TestGlobalSlotsReleasedByCloseUsersAndKick(t *testing.T) {
	inA := NewRegistry("inA")
	inB := NewRegistry("inB")

	conn := &fakeConn{}
	inA.Track("10.0.0.1:1000", conn)
	if !inA.TryBind("carol", "10.0.0.1:1000", 1) {
		t.Fatal("expected the first device to be accepted")
	}
	if inB.TryBind("carol", "10.0.0.2:1000", 1) {
		t.Fatal("expected the second device to be rejected: limit 1 is full")
	}
	if inA.CloseUsers(map[string]struct{}{}) != 1 {
		t.Fatal("expected one session cut")
	}
	if !conn.closed {
		t.Fatal("expected the session to be closed")
	}
	if !inB.TryBind("carol", "10.0.0.2:1000", 1) {
		t.Fatal("expected the slot to be released after the user was removed")
	}
	if kicked := inB.KickUserSessions("carol"); kicked != 1 {
		t.Fatalf("expected 1 kick, got %d", kicked)
	}
	if counts := GlobalDeviceCounts(); counts["carol"] != 1 {
		t.Fatalf("expected a kick without a closer to keep muting (and counting) the session, got %v", counts)
	}
	// With a closer the kick cuts the session and releases the slot.
	inB.Track("10.0.0.2:1000", &fakeConn{})
	if kicked := inB.KickUserSessions("carol"); kicked != 1 {
		t.Fatalf("expected 1 kick, got %d", kicked)
	}
	if counts := GlobalDeviceCounts(); counts["carol"] != 0 {
		t.Fatalf("expected the kick with a closer to release the slot, got %d", counts["carol"])
	}
}

// The same source string in two inbounds must never touch each other's slots:
// bindings, releases and rejects are scoped by the inbound tag.
func TestSameSourceAcrossInboundsDoesNotCollide(t *testing.T) {
	inA := NewRegistry("inA")
	inB := NewRegistry("inB")

	// alice holds the source in A; bob holds the identical source in B.
	if !inA.TryBind("alice2", "10.0.0.1:1000", 1) {
		t.Fatal("expected alice to bind on inbound A")
	}
	if !inB.TryBind("bob2", "10.0.0.1:1000", 1) {
		t.Fatal("expected bob to bind the same source on inbound B: the tag namespaces the key")
	}
	counts := GlobalDeviceCounts()
	if counts["alice2"] != 1 || counts["bob2"] != 1 {
		t.Fatalf("expected one device each, got %v", counts)
	}
	// Releasing alice's source on A must not free or cut bob's identical
	// source on B.
	inA.Untrack("10.0.0.1:1000")
	counts = GlobalDeviceCounts()
	if counts["bob2"] != 1 {
		t.Fatalf("expected bob's slot to survive the release of the colliding source on A, got %v", counts)
	}
	inB.Untrack("10.0.0.1:1000")
	counts = GlobalDeviceCounts()
	if counts["bob2"] != 0 {
		t.Fatalf("expected bob's slot to be released by its own inbound, got %v", counts)
	}
}

// GetDeviceCounts-style reads see the sum across inbounds (展示与拦截同源).
func TestGlobalDeviceCountsAggregatesAcrossInbounds(t *testing.T) {
	inA := NewRegistry("inA")
	inB := NewRegistry("inB")
	inC := NewRegistry("inC")

	inA.TryBind("dave", "10.0.0.1:1", 0)
	inA.TryBind("dave", "10.0.0.2:1", 0)
	inB.TryBind("dave", "10.0.0.3:1", 0)
	inC.TryBind("erin", "10.0.0.1:1", 0)

	counts := GlobalDeviceCounts()
	if counts["dave"] != 3 {
		t.Fatalf("expected 3 devices for dave across inbounds, got %d", counts["dave"])
	}
	if counts["erin"] != 1 {
		t.Fatalf("expected 1 device for erin, got %d", counts["erin"])
	}
}
