package devlimit

import (
	"sync"
	"time"
)

// Rejection records a single device-limit rejection event.
type Rejection struct {
	User    string    `json:"user"`
	Source  string    `json:"source"`
	Limit   int       `json:"limit"`
	At      time.Time `json:"at"`
}

const maxRejections = 200

var (
	rejMu      sync.Mutex
	rejections []Rejection
	rejIdx     int
	rejFull    bool
)

// LogRejection records a device-limit rejection event in the in-memory ring
// buffer. The buffer holds the most recent maxRejections entries across all
// users.
func LogRejection(user, source string, limit int) {
	rejMu.Lock()
	defer rejMu.Unlock()
	entry := Rejection{
		User:   user,
		Source: source,
		Limit:  limit,
		At:     time.Now(),
	}
	if !rejFull {
		rejections = append(rejections, entry)
		if len(rejections) >= maxRejections {
			rejFull = true
		}
	} else {
		rejections[rejIdx] = entry
	}
	rejIdx = (rejIdx + 1) % maxRejections
}

// GetRejections returns all recorded rejection events, optionally filtered by
// user. Results are in chronological order (oldest first).
func GetRejections(user string) []Rejection {
	rejMu.Lock()
	defer rejMu.Unlock()

	// Reconstruct chronological order from the ring buffer.
	var all []Rejection
	if !rejFull {
		all = make([]Rejection, len(rejections))
		copy(all, rejections)
	} else {
		n := len(rejections)
		all = make([]Rejection, n)
		// rejIdx points to the next write position, which is the oldest entry.
		for i := 0; i < n; i++ {
			all[i] = rejections[(rejIdx+i)%n]
		}
	}

	if user == "" {
		return all
	}
	filtered := make([]Rejection, 0, 16)
	for _, r := range all {
		if r.User == user {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

// GetRejectionUsers returns the set of users that have at least one rejection
// record in the ring buffer.
func GetRejectionUsers() []string {
	rejMu.Lock()
	defer rejMu.Unlock()
	seen := make(map[string]struct{})
	for i := range rejections {
		seen[rejections[i].User] = struct{}{}
	}
	users := make([]string, 0, len(seen))
	for u := range seen {
		users = append(users, u)
	}
	return users
}

// HasRejections reports whether any rejection has been recorded for user since
// the panel started (or since the ring buffer wrapped).
func HasRejections(user string) bool {
	rejMu.Lock()
	defer rejMu.Unlock()
	for i := range rejections {
		if rejections[i].User == user {
			return true
		}
	}
	return false
}
