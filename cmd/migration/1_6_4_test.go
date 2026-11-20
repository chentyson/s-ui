package migration

import (
	"testing"

	"github.com/alireza0/s-ui/database/model"

	"gorm.io/gorm"
)

func openZhdataTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openTestDB(t)
	if err := db.AutoMigrate(&model.Client{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func isUUID(s string) bool {
	// Loose shape check: 8-4-4-4-12 hex. The generator is gors/uuid v4; the
	// migration must not be stricter than what it accepts back.
	return len(s) == 36 && s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

// zhdata arrives as an external protocol, so every client created until now
// has credentials for the other protocols and nothing for zhdata -- which
// leaves it unable to use a zhdata inbound at all. The uuid reuses the
// client's vless uuid when there is one.
func TestAddZhdataClientConfigReusesVlessUUID(t *testing.T) {
	db := openZhdataTestDB(t)

	alice := createTestClient(t, db, "alice", `{"vless":{"name":"alice","uuid":"b831381d-6324-4d53-ad4f-8cda48b30811"}}`)

	if _, err := addZhdataClientConfig(db); err != nil {
		t.Fatal(err)
	}

	config := clientConfig(t, db, alice.Id)
	zhdata, ok := config["zhdata"]
	if !ok {
		t.Fatal("client got no zhdata credentials")
	}
	if zhdata["name"] != "alice" {
		t.Errorf("zhdata name = %v, want %q", zhdata["name"], "alice")
	}
	if zhdata["uuid"] != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Errorf("zhdata uuid = %v, want the client's vless uuid", zhdata["uuid"])
	}

	// Nothing else may be disturbed.
	if len(config) != 2 {
		t.Errorf("client config has %d protocols, want 2", len(config))
	}
}

// Without a vless uuid the migration mints a fresh one, and two clients get
// different uuids.
func TestAddZhdataClientConfigMintsUUID(t *testing.T) {
	db := openZhdataTestDB(t)

	alice := createTestClient(t, db, "alice", `{"trojan":{"name":"alice","password":"hunter2"}}`)
	bob := createTestClient(t, db, "bob", `{}`)

	if _, err := addZhdataClientConfig(db); err != nil {
		t.Fatal(err)
	}

	uuids := map[string]bool{}
	for _, c := range []*model.Client{alice, bob} {
		zhdata, ok := clientConfig(t, db, c.Id)["zhdata"]
		if !ok {
			t.Fatalf("client %q got no zhdata credentials", c.Name)
		}
		if zhdata["name"] != c.Name {
			t.Errorf("zhdata name = %v, want %q", zhdata["name"], c.Name)
		}
		uuid, _ := zhdata["uuid"].(string)
		if !isUUID(uuid) {
			t.Errorf("client %q uuid = %q, want a fresh uuid", c.Name, uuid)
		}
		if uuids[uuid] {
			t.Errorf("clients %q and another share uuid %q", c.Name, uuid)
		}
		uuids[uuid] = true
	}
}

// A client that already has zhdata credentials keeps them: the migration must
// not hand a working client a new uuid and cut it off.
func TestAddZhdataClientConfigKeepsExisting(t *testing.T) {
	db := openZhdataTestDB(t)

	c := createTestClient(t, db, "alice", `{"zhdata":{"name":"alice","uuid":"00000000-0000-4000-8000-000000000001"}}`)

	if _, err := addZhdataClientConfig(db); err != nil {
		t.Fatal(err)
	}

	if got := clientConfig(t, db, c.Id)["zhdata"]["uuid"]; got != "00000000-0000-4000-8000-000000000001" {
		t.Errorf("uuid = %v, want it untouched", got)
	}
}

// A replay must not replace the uuids handed out by the first pass, which
// subscribers are already using.
func TestAddZhdataClientConfigReplayed(t *testing.T) {
	db := openZhdataTestDB(t)

	c := createTestClient(t, db, "alice", `{}`)

	if _, err := addZhdataClientConfig(db); err != nil {
		t.Fatal(err)
	}
	first := clientConfig(t, db, c.Id)["zhdata"]["uuid"]

	if _, err := addZhdataClientConfig(db); err != nil {
		t.Fatal(err)
	}
	if second := clientConfig(t, db, c.Id)["zhdata"]["uuid"]; second != first {
		t.Errorf("uuid changed on the second run: %v -> %v", first, second)
	}
}

// A config no one can parse is skipped, not fatal: the other clients still get
// their credentials.
func TestAddZhdataClientConfigSkipsUnparsableConfig(t *testing.T) {
	db := openZhdataTestDB(t)

	broken := createTestClient(t, db, "broken", `not json`)
	ok := createTestClient(t, db, "ok", `{}`)

	if _, err := addZhdataClientConfig(db); err != nil {
		t.Fatal(err)
	}

	if _, exists := clientConfig(t, db, ok.Id)["zhdata"]; !exists {
		t.Error("a readable client was skipped because another one was broken")
	}
	var stored model.Client
	if err := db.Where("id = ?", broken.Id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if string(stored.Config) != "not json" {
		t.Errorf("the unreadable config was rewritten: %s", stored.Config)
	}
}

// A database old enough to have no clients table must not stop the migration:
// there is nothing to give zhdata credentials to.
func TestAddZhdataClientConfigWithoutClientsTable(t *testing.T) {
	db := openTestDB(t)
	if err := to1_6_4(db); err != nil {
		t.Fatal(err)
	}
}

// The chain gate: a database stamped with an old ZY build of the same base
// version still runs the step -- parseVersion ignores the -ZYx suffix, so the
// 1.6.4 gate covers "1.6.3-ZY4".
func TestZhdataMigrationGateCoversZYSuffix(t *testing.T) {
	if compareVersions("1.6.3-ZY4", "1.6.4") >= 0 {
		t.Fatal("the 1.6.4 gate must fire for a 1.6.3-ZY4 database")
	}
	if compareVersions("1.6.3", "1.6.4") >= 0 {
		t.Fatal("the 1.6.4 gate must fire for a 1.6.3 database")
	}
	if compareVersions("1.6.4", "1.6.4") < 0 {
		t.Fatal("a 1.6.4 database must not replay the step")
	}
}
