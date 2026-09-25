// Package devlimit holds the per-user device limits that the core consults at
// connection time. It is an in-memory registry populated by the panel whenever
// the config is (re)built, so the hot connection path never touches the DB.
package devlimit

import "sync"

// DefaultLimit is kept for backward compatibility but is no longer used as a
// fallback. devs = 0 means "no limit".
const DefaultLimit = 2

var (
	mu     sync.RWMutex
	limits = make(map[string]int)
)

// Reload atomically replaces the user -> device-limit map. A limit of 0 means
// "no limit" for that user.
func Reload(m map[string]int) {
	mu.Lock()
	defer mu.Unlock()
	if m == nil {
		m = make(map[string]int)
	}
	limits = m
}

// Get returns the device limit for user. A value of 0 means "no limit".
// Unknown users also get 0 (no limit).
func Get(user string) int {
	mu.RLock()
	defer mu.RUnlock()
	if n, ok := limits[user]; ok {
		return n
	}
	return 0
}
