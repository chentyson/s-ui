// Cross-inbound device accounting.
//
// Every inbuilt sui inbound enforces the client's device limit (devlimit)
// against the user's TOTAL set of active devices across all inbounds, not just
// the devices one inbound can see. Each Registry reports its active sources to
// the table in this file under a key prefixed with its inbound tag, so the
// same client address seen by two different inbounds (e.g. one NAT'ed source
// behind both) stays two separate entries and never lets one inbound's binding
// be released or rejected by another inbound's traffic.
//
// The panel's GetDeviceCounts reads this same table, so the count the panel
// displays and the limit TryBind enforces share one source of truth.
package usersession

import (
	"strings"
	"sync"
)

// keySeparator cannot appear in an inbound tag or in a serialised source
// address, so concatenating tag and source below is unambiguous.
const keySeparator = "\x1f"

// globalDeviceTable maps a user to the set of "tag<sep>source" keys that
// currently hold one of the user's device slots.
type globalDeviceTable struct {
	access  sync.Mutex
	devices map[string]map[string]struct{}
}

var globalDevices = &globalDeviceTable{
	devices: make(map[string]map[string]struct{}),
}

// allow grants the device slot to key unless the user already occupies limit
// distinct device slots across every inbound. Granting adds the key, so the
// caller must treat a true result as "the slot is now held by key". limit <= 0
// means no limit.
func (g *globalDeviceTable) allow(user string, key string, limit int) bool {
	g.access.Lock()
	defer g.access.Unlock()
	set, known := g.devices[user]
	if known {
		if _, held := set[key]; held {
			return true
		}
		if limit > 0 && len(set) >= limit {
			return false
		}
	} else {
		set = make(map[string]struct{})
		g.devices[user] = set
	}
	set[key] = struct{}{}
	return true
}

// add takes the slot for key without consulting the limit (used for the
// already-known-fast paths and for unlimited users).
func (g *globalDeviceTable) add(user string, key string) {
	g.access.Lock()
	defer g.access.Unlock()
	set, known := g.devices[user]
	if !known {
		set = make(map[string]struct{})
		g.devices[user] = set
	}
	set[key] = struct{}{}
}

// remove releases the slot key held for user. Releasing a key that is not held
// (or is held under another user) is a no-op.
func (g *globalDeviceTable) remove(user string, key string) {
	g.access.Lock()
	defer g.access.Unlock()
	set, known := g.devices[user]
	if !known {
		return
	}
	delete(set, key)
	if len(set) == 0 {
		delete(g.devices, user)
	}
}

// GlobalDeviceCounts snapshots the active device count per user, summed across
// all inbounds. It is the number the panel displays and it is exactly what
// TryBind checks against.
func GlobalDeviceCounts() map[string]int {
	globalDevices.access.Lock()
	defer globalDevices.access.Unlock()
	counts := make(map[string]int, len(globalDevices.devices))
	for user, set := range globalDevices.devices {
		if len(set) > 0 {
			counts[user] = len(set)
		}
	}
	return counts
}

// GlobalDeviceCountsByTag snapshots the same table grouped by inbound tag
// instead of by user. Every held key is "tag<sep>source", so counting keys by
// their prefix gives the device slots each inbound currently holds -- the
// panel's per-inbound online count, same source of truth as TryBind.
func GlobalDeviceCountsByTag() map[string]int {
	globalDevices.access.Lock()
	defer globalDevices.access.Unlock()
	counts := map[string]int{}
	for _, set := range globalDevices.devices {
		for key := range set {
			if i := strings.Index(key, keySeparator); i > 0 {
				counts[key[:i]]++
			}
		}
	}
	return counts
}
