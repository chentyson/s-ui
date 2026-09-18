// Package devlimit holds the per-user device limits that the core consults at
// connection time. It is an in-memory registry populated by the panel whenever
// the config is (re)built, so the hot connection path never touches the DB.
package devlimit

import "sync"

// DefaultLimit is applied when a user has no explicit limit (devs = 0) or is
// not present in the registry at all.
const DefaultLimit = 2

var (
	mu     sync.RWMutex
	limits = make(map[string]int)
)

// Reload atomically replaces the user -> device-limit map. A limit of 0 (or a
// missing user) falls back to DefaultLimit at lookup time.
func Reload(m map[string]int) {
	mu.Lock()
	defer mu.Unlock()
	if m == nil {
		m = make(map[string]int)
	}
	limits = m
}

// Get returns the device limit for user, or DefaultLimit when the user is
// unknown or has no explicit (positive) limit.
func Get(user string) int {
	mu.RLock()
	defer mu.RUnlock()
	if n, ok := limits[user]; ok && n > 0 {
		return n
	}
	return DefaultLimit
}
