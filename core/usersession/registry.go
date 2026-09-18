// Package usersession tracks the client sessions of an inbound, so that the
// sessions of a user who was just removed can actually be cut.
//
// Protocols that authenticate once per session (the QUIC ones and anytls) keep
// serving an already authenticated client after its user is gone from the
// inbound: swapping the auth map only affects new sessions, and closing the
// routed connections is not enough because the client simply opens another
// stream on the same session. The panel therefore has to reach the session
// itself, which is what this registry is for.
package usersession

import (
	"io"
	"sync"
	"time"

	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	N "github.com/sagernet/sing/common/network"
)

const (
	// A source that has not opened a connection for this long is forgotten;
	// its session is either gone or idle enough to be re-learned on use.
	idleTimeout = 10 * time.Minute
	// Backstop for blocks: a client whose session is really dead stops being
	// blocked, so the address is reusable even if the user is never re-added.
	blockTimeout = 10 * time.Minute
	// A kicked session is muted only while it keeps trying. Once it has been
	// quiet this long, the next attempt from the address is a new session and
	// is let through, so a disconnect does not turn into a lockout.
	kickQuietWindow = 30 * time.Second
)

// Closer is implemented by the inbounds that own a Registry.
type Closer interface {
	// CloseUserSessions cuts the sessions of users that are gone from the
	// inbound, and lets back in the ones that are still there.
	CloseUserSessions(keep map[string]struct{}) int
	// KickUserSessions cuts the sessions of one user who is still enabled, for
	// a disconnect asked for from the panel.
	KickUserSessions(user string) int
}

type entry struct {
	user     string
	lastSeen time.Time
	closer   io.Closer
	// refs is the number of live sub-connections bound to this source. A
	// source counts as an active device while refs > 0; it is released back to
	// the pool (and out of the per-user index) once the last one closes.
	refs int
}

// block mutes one client address. A removal keeps it muted until the backstop;
// a kick, which must not lock a still-enabled user out, lifts as soon as the
// muted session stops trying.
type block struct {
	at          time.Time
	lastAttempt time.Time
	kick        bool
}

// Registry maps a client address to the user it authenticated as. One instance
// per inbound. userSources is the reverse index (user -> active sources) that
// backs per-user device counting.
type Registry struct {
	access  sync.Mutex
	sources map[string]*entry
	blocked map[string]*block
	// userSources maps a user to the set of sources currently active for that
	// user. It is maintained alongside sources and is what CountForUser reads.
	userSources map[string]map[string]struct{}
}

func NewRegistry() *Registry {
	return &Registry{
		sources:     make(map[string]*entry),
		blocked:     make(map[string]*block),
		userSources: make(map[string]map[string]struct{}),
	}
}

func (r *Registry) load(source string) *entry {
	e, loaded := r.sources[source]
	if !loaded {
		e = &entry{}
		r.sources[source] = e
	}
	e.lastSeen = time.Now()
	return e
}

// Bind records which user the session at source authenticated as. Called for
// every connection the session opens, which doubles as a liveness ping.
func (r *Registry) Bind(user string, source string) {
	if source == "" {
		return
	}
	r.access.Lock()
	defer r.access.Unlock()
	e := r.load(source)
	if user != "" {
		e.user = user
	}
}

// Track stores the session transport, for protocols whose session has a closer
// of its own. Without one the session can only be muted, not closed.
func (r *Registry) Track(source string, closer io.Closer) {
	if source == "" {
		return
	}
	r.access.Lock()
	defer r.access.Unlock()
	r.load(source).closer = closer
}

func (r *Registry) Untrack(source string) {
	if source == "" {
		return
	}
	r.access.Lock()
	defer r.access.Unlock()
	if e, ok := r.sources[source]; ok && e.refs > 0 {
		// Refcounted sub-connection (QUIC protocols): release one reference and
		// only drop the source once the last one has closed.
		e.refs--
		if e.refs == 0 {
			r.removeSourceLocked(source)
		}
		return
	}
	r.removeSourceLocked(source)
}

// TryBind records which user the session at source authenticated as and, if the
// user is within their device limit, marks the source as an active device. It
// returns false (without binding) when the user already has limit distinct
// active sources, so the caller can reject the connection before it is routed.
// A source that is already active for the same user just bumps its refcount and
// is always allowed. limit <= 0 means "no limit".
func (r *Registry) TryBind(user string, source string, limit int) bool {
	if source == "" || user == "" {
		return true
	}
	r.access.Lock()
	defer r.access.Unlock()
	e := r.load(source)
	if e.refs > 0 && e.user == user {
		e.refs++
		return true
	}
	if limit > 0 {
		if set, ok := r.userSources[user]; ok && len(set) >= limit {
			return false
		}
	}
	// The source was active for a different user; release it from that user's
	// index before reassigning it.
	if e.user != "" && e.user != user {
		if set, ok := r.userSources[e.user]; ok {
			delete(set, source)
			if len(set) == 0 {
				delete(r.userSources, e.user)
			}
		}
	}
	e.user = user
	e.refs = 1
	if r.userSources[user] == nil {
		r.userSources[user] = make(map[string]struct{})
	}
	r.userSources[user][source] = struct{}{}
	return true
}

// CountForUser returns the number of distinct sources currently active for
// user. It is the live device count the panel's limit is compared against.
func (r *Registry) CountForUser(user string) int {
	if user == "" {
		return 0
	}
	r.access.Lock()
	defer r.access.Unlock()
	return len(r.userSources[user])
}

// TrackClose wraps a close handler so the source's refcount is released when
// the connection closes. Protocols that refcount sub-connections (the QUIC
// ones) use this to keep the per-user device count accurate.
func (r *Registry) TrackClose(source string, onClose N.CloseHandlerFunc) N.CloseHandlerFunc {
	return func(err error) {
		r.Untrack(source)
		if onClose != nil {
			onClose(err)
		}
	}
}

// removeSourceLocked drops a source from the registry and the per-user index.
// Callers must hold r.access.
func (r *Registry) removeSourceLocked(source string) {
	if e, ok := r.sources[source]; ok && e.user != "" {
		if set, ok := r.userSources[e.user]; ok {
			delete(set, source)
			if len(set) == 0 {
				delete(r.userSources, e.user)
			}
		}
	}
	delete(r.sources, source)
	delete(r.blocked, source)
}

// Allowed reports whether connections from source may still be routed. A
// session that cannot be closed is muted here instead: nothing it opens is
// routed any more, so the removed user's traffic stops.
func (r *Registry) Allowed(source string) bool {
	if source == "" {
		return true
	}
	r.access.Lock()
	defer r.access.Unlock()
	b, blocked := r.blocked[source]
	if !blocked {
		return true
	}
	now := time.Now()
	if now.Sub(b.at) > blockTimeout {
		delete(r.blocked, source)
		return true
	}
	// A gap this long means the muted session gave up; whatever is connecting
	// now is a new one.
	if b.kick && now.Sub(b.lastAttempt) > kickQuietWindow {
		delete(r.blocked, source)
		return true
	}
	b.lastAttempt = now
	return false
}

// CloseUsers cuts the sessions of every user not in keep and lifts the block on
// the sessions of users that are in keep, so re-enabling a user takes effect
// without waiting for their session to die. Returns the number of sessions cut.
func (r *Registry) CloseUsers(keep map[string]struct{}) int {
	now := time.Now()

	r.access.Lock()
	var closers []io.Closer
	cut := 0
	for source, e := range r.sources {
		if now.Sub(e.lastSeen) > idleTimeout {
			r.removeSourceLocked(source)
			continue
		}
		if e.user == "" {
			continue
		}
		if _, ok := keep[e.user]; ok {
			delete(r.blocked, source)
			continue
		}
		cut++
		if e.closer != nil {
			closers = append(closers, e.closer)
			r.removeSourceLocked(source)
			continue
		}
		r.blocked[source] = &block{at: now, lastAttempt: now}
	}
	for source, b := range r.blocked {
		if now.Sub(b.at) > blockTimeout {
			delete(r.blocked, source)
		}
	}
	r.access.Unlock()

	// Outside the lock: closing a tracked session runs the inbound's own close
	// handler, which comes back through Untrack.
	for _, closer := range closers {
		_ = closer.Close()
	}
	return cut
}

// ErrRemoved is reported to the close handler of a connection that a muted
// session tried to open.
var ErrRemoved = E.New("user removed from inbound")

// Reject drops a connection opened by a session whose user is gone.
func Reject(conn io.Closer, onClose N.CloseHandlerFunc) {
	common.Close(conn)
	if onClose != nil {
		onClose(ErrRemoved)
	}
}

// KickUserSessions disconnects a user who is still enabled. A session with a
// closer is cut outright; one without is muted, which is the only way to stop a
// QUIC session that the protocol gives us no handle on. The mute lifts as soon
// as that session stops trying, so the client reconnects on its own.
func (r *Registry) KickUserSessions(user string) int {
	if user == "" {
		return 0
	}
	now := time.Now()

	r.access.Lock()
	var closers []io.Closer
	kicked := 0
	for source, e := range r.sources {
		if e.user != user {
			continue
		}
		kicked++
		if e.closer != nil {
			r.removeSourceLocked(source)
			closers = append(closers, e.closer)
			continue
		}
		r.blocked[source] = &block{at: now, lastAttempt: now, kick: true}
	}
	r.access.Unlock()

	for _, closer := range closers {
		_ = closer.Close()
	}
	return kicked
}
