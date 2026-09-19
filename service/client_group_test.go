package service

import (
	"testing"

	"github.com/alireza0/s-ui/core/bwlimit"
	"github.com/alireza0/s-ui/database/model"
)

// TestNormalizeGroup checks the rule a saved client group has to obey: a
// bandwidth is kept as written, an empty group stays empty (unlimited), and
// anything else becomes the default cap so a label cannot leave a client
// unmetered.
func TestNormalizeGroup(t *testing.T) {
	cases := []struct {
		group string
		want  string
	}{
		{"", ""},
		{"1M", "1M"},
		{"1.5M", "1.5M"},
		{"2m", "2m"},
		{"500K", "500K"},
		{"2", "2"},
		{"vip", bwlimit.DefaultGroup},
		{"special group", bwlimit.DefaultGroup},
		{"1.5.5M", bwlimit.DefaultGroup},
	}
	for _, c := range cases {
		client := &model.Client{Name: "c1", Group: c.group}
		normalizeGroup(client)
		if client.Group != c.want {
			t.Errorf("group %q became %q, want %q", c.group, client.Group, c.want)
		}
	}
}

// TestGroupCapsMatchStoredGroups checks that what a save stores is what the
// core then enforces: every stored group either caps the client or is empty.
func TestGroupCapsMatchStoredGroups(t *testing.T) {
	for _, group := range []string{"", "1M", "1.5M", "vip"} {
		client := &model.Client{Name: "c1", Group: group}
		normalizeGroup(client)
		limit, capped := bwlimit.Group(client.Group)
		if group == "" {
			if capped {
				t.Errorf("an empty group was capped: %+v", limit)
			}
			continue
		}
		if !capped {
			t.Errorf("group %q was stored but caps nothing", client.Group)
		}
		if limit.Up != limit.Down {
			t.Errorf("group %q caps up and down differently: %+v", client.Group, limit)
		}
	}
}
