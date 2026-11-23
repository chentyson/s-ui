package service

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alireza0/s-ui/core/bwlimit"
	"github.com/alireza0/s-ui/database"
)

// TestZhdataUsersCarryGroupCaps: the client's group is its bandwidth cap for the
// sing-box inbounds already (ZY2), so a zhdata user with group "2M" expects the
// same treatment. The zhdata tunnel does not pass through a sing-box inbound, so
// the only way the cap reaches that process is the PUT /v1/users payload -- the
// caps must be in it, read through the same helper, with the same semantics:
// empty group = unlimited, unreadable group = bwlimit.DefaultKbit (a typo must
// not hand out an unmetered line).
func TestZhdataUsersCarryGroupCaps(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()

	rows := []struct {
		name   string
		uuid   string
		group  string
		enable bool
	}{
		{"two-meg", "11111111-1111-1111-1111-111111111111", "2M", true},
		{"kilo", "22222222-2222-2222-2222-222222222222", "500K", true},
		{"bare-number", "33333333-3333-3333-3333-333333333333", "1.5", true},
		{"a-label", "44444444-4444-4444-4444-444444444444", "vip", true},
		{"no-group", "55555555-5555-5555-5555-555555555555", "", true},
		{"disabled", "66666666-6666-6666-6666-666666666666", "2M", false},
		{"no-credentials", "", "2M", true},
	}
	for _, r := range rows {
		config := []byte(`{}`)
		if r.uuid != "" {
			config = []byte(`{"zhdata":{"uuid":"` + r.uuid + `"}}`)
		}
		if err := db.Exec(`INSERT INTO clients (name, enable, config, "group", devs, expiry, volume, up, down) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			r.name, r.enable, config, r.group, 2, 1800000000, 107374182400, 1000, 2000).Error; err != nil {
			t.Fatalf("insert %s: %v", r.name, err)
		}
	}

	users, err := zhdataEnabledUsers(db)
	if err != nil {
		t.Fatal(err)
	}

	byID := make(map[string]zhdataAdminUsers, len(users))
	for _, u := range users {
		byID[u.UUID] = u
	}
	if len(users) != 5 {
		t.Fatalf("enabled clients with zhdata credentials: got %d, want 5 (%+v)", len(users), users)
	}
	for _, c := range []struct {
		uuid string
		want int
	}{
		{"11111111-1111-1111-1111-111111111111", 2000},
		{"22222222-2222-2222-2222-222222222222", 500},
		{"33333333-3333-3333-3333-333333333333", 1500}, // a bare number is megabits
		{"44444444-4444-4444-4444-444444444444", bwlimit.DefaultKbit},
		{"55555555-5555-5555-5555-555555555555", 0}, // no group, no cap
	} {
		u, ok := byID[c.uuid]
		if !ok {
			t.Errorf("client %s missing from the push", c.uuid)
			continue
		}
		if u.UpKbps != c.want || u.DownKbps != c.want {
			t.Errorf("client %s pushed with %+v, want %d kbit/s both ways", c.uuid, u, c.want)
		}
		// the fields the account query needs stay intact next to the caps
		if u.MaxDevices != 2 || u.Expiry != 1800000000 || u.Quota != 107374182400 || u.Used != 3000 {
			t.Errorf("client %s lost its other fields: %+v", c.uuid, u)
		}
	}

	body, err := json.Marshal(struct {
		Users []zhdataAdminUsers `json:"users"`
	}{Users: users})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"up_kbps":2000`, `"down_kbps":2000`, `"up_kbps":0`} {
		if !strings.Contains(string(body), key) {
			t.Errorf("payload is missing %s: %s", key, body)
		}
	}
}
