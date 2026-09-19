package bwlimit

import (
	"fmt"
	"math"
	"strconv"
	"strings"
)

// A client's group is its bandwidth setting: "1M", "1.5M", "500K", case
// insensitive, with an empty group meaning no cap. The group is also the label
// the panel groups clients by, so a value that reads as neither is not treated
// as "unlimited" -- a typo must not hand out an unmetered line. It is capped at
// DefaultKbit instead, and a save rewrites the stored group to DefaultGroup so
// the operator can see what it became.
const (
	// DefaultGroup is what an unreadable group is stored as.
	DefaultGroup = "1M"
	// DefaultKbit is the cap that group stands for.
	DefaultKbit = 1000
)

// Group returns the cap a stored group stands for. ok is false for an empty
// group, which caps nothing; every other group is capped, readable or not.
func Group(group string) (Limit, bool) {
	value := strings.TrimSpace(group)
	if value == "" {
		return Limit{}, false
	}
	kbit, err := ParseKbit(value)
	if err != nil {
		return Limit{Up: DefaultKbit, Down: DefaultKbit}, true
	}
	return Limit{Up: kbit, Down: kbit}, true
}

// CanonicalGroup returns the group to store and whether it had to change. An
// empty group stays empty, a group that reads as a bandwidth is left exactly as
// the operator spelled it, and anything else becomes DefaultGroup.
func CanonicalGroup(group string) (string, bool) {
	value := strings.TrimSpace(group)
	if value == "" {
		return group, false
	}
	if _, err := ParseKbit(value); err != nil {
		return DefaultGroup, true
	}
	return group, false
}

// ParseKbit reads a bandwidth as kbit/s. A bare number is megabits, and the
// suffixes K, M and G scale it; a trailing unit word ("Mbps", "MBit", "Kbit")
// is accepted as well. "1.5M" is a megabit and a half, "500K" half a megabit,
// "2" two megabits. The result is rounded to the kbit.
func ParseKbit(s string) (int, error) {
	s = strings.TrimSpace(s)
	numberEnd := 0
	for numberEnd < len(s) {
		c := s[numberEnd]
		if (c >= '0' && c <= '9') || c == '.' {
			numberEnd++
			continue
		}
		break
	}
	if numberEnd == 0 {
		return 0, fmt.Errorf("bwlimit: no number in group %q", s)
	}
	value, err := strconv.ParseFloat(s[:numberEnd], 64)
	if err != nil {
		return 0, fmt.Errorf("bwlimit: group %q: %w", s, err)
	}
	unit := strings.ToUpper(strings.TrimSpace(s[numberEnd:]))
	switch {
	case unit == "":
		// A bare number is megabits: the panel's bandwidth options are in
		// megabits and "2" reads as "2M" to everyone who uses it.
		value *= 1000
	case strings.HasPrefix(unit, "M"):
		value *= 1000
	case strings.HasPrefix(unit, "K"):
		// Kilo as written.
	case strings.HasPrefix(unit, "G"):
		value *= 1000 * 1000
	default:
		return 0, fmt.Errorf("bwlimit: unknown unit %q in group %q", unit, s)
	}
	if value <= 0 || math.IsInf(value, 0) || math.IsNaN(value) {
		return 0, fmt.Errorf("bwlimit: group %q is not a positive bandwidth", s)
	}
	return int(math.Round(value)), nil
}
