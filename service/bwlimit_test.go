package service

import (
	"path/filepath"
	"testing"

	"github.com/alireza0/s-ui/core/bwlimit"
	"github.com/alireza0/s-ui/database"
)

// TestReloadGroupLimitsReadsGroups covers the whole path from the clients table
// to the caps the core enforces: the group column is a keyword (hence the
// quoting in the query), an empty group caps nothing, a bandwidth group caps
// both directions at the same rate, and a group that is neither is capped at
// the default rather than left unlimited.
func TestReloadGroupLimitsReadsGroups(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	db := database.GetDB()

	for _, row := range []struct{ name, group string }{
		{"empty-group", ""},
		{"one-and-a-half", "1.5M"},
		{"two-meg", "2M"},
		{"a-label", "vip"},
		{"kilo", "500K"},
	} {
		if err := db.Exec(`INSERT INTO clients (name, "group") VALUES (?, ?)`, row.name, row.group).Error; err != nil {
			t.Fatalf("insert %s: %v", row.name, err)
		}
	}

	(&ConfigService{}).ReloadGroupLimits()

	if limit, ok := bwlimit.Configured("empty-group"); ok {
		t.Errorf("an empty group was capped: %+v", limit)
	}
	for _, c := range []struct {
		name string
		want int
	}{
		{"one-and-a-half", 1500},
		{"two-meg", 2000},
		{"kilo", 500},
		{"a-label", bwlimit.DefaultKbit},
	} {
		limit, ok := bwlimit.Configured(c.name)
		if !ok {
			t.Errorf("client %s is not capped", c.name)
			continue
		}
		if limit.Up != c.want || limit.Down != c.want {
			t.Errorf("client %s capped at %+v, want %d both ways", c.name, limit, c.want)
		}
	}

	// An unknown client is not capped, and a client whose group is cleared
	// stops being capped on the next reload.
	if _, ok := bwlimit.Configured("nobody"); ok {
		t.Error("an unknown client must not be capped")
	}
	if err := db.Exec(`UPDATE clients SET "group" = '' WHERE name = ?`, "two-meg").Error; err != nil {
		t.Fatal(err)
	}
	(&ConfigService{}).ReloadGroupLimits()
	if limit, ok := bwlimit.Configured("two-meg"); ok {
		t.Errorf("clearing the group left the client capped: %+v", limit)
	}
}

// TestReloadGroupLimitsSurvivesMissingTable checks the failure path: a database
// without the clients table must not stop a config build, it just caps nobody.
func TestReloadGroupLimitsSurvivesMissingTable(t *testing.T) {
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Migrator().DropTable("clients"); err != nil {
		t.Fatal(err)
	}
	(&ConfigService{}).ReloadGroupLimits()
	if _, ok := bwlimit.Configured("anyone"); ok {
		t.Error("a missing clients table must not cap anyone")
	}
}
