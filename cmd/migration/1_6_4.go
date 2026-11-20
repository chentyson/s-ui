package migration

// zhdata client credentials (docs/zhdata-admin-api.md §3).
//
// zhdata arrives as an external protocol: a client authenticates to the zhdata
// server with clients.config.zhdata = {"name": ..., "uuid": ...}. Clients
// created before this have credentials for every other protocol and none for
// zhdata, so they cannot be pushed to a zhdata process at all.
//
// The uuid reuses the client's existing vless uuid when it has one — the two
// are the same kind of identifier and operators expect a client to carry one
// uuid across uuid-based protocols — and mints a fresh one otherwise. A client
// that already has zhdata credentials is left exactly as it is, so a replay
// never rotates a uuid subscribers are already using.

import (
	"encoding/json"
	"fmt"

	"github.com/gofrs/uuid/v5"

	"gorm.io/gorm"
)

func to1_6_4(tx *gorm.DB) error {
	changed, err := addZhdataClientConfig(tx)
	if err != nil {
		return err
	}
	if changed > 0 {
		fmt.Printf("zhdata: added zhdata credentials to %d client(s)\n", changed)
	}
	return nil
}

func addZhdataClientConfig(tx *gorm.DB) (int, error) {
	if !tx.Migrator().HasTable("clients") {
		return 0, nil
	}

	var clients []clientRow
	if err := tx.Raw("SELECT id, name, config FROM clients").Scan(&clients).Error; err != nil {
		return 0, err
	}

	changed := 0
	for _, client := range clients {
		config := map[string]json.RawMessage{}
		if len(client.Config) > 0 {
			if err := json.Unmarshal(client.Config, &config); err != nil {
				// One unreadable config must not stop the rest. The client
				// keeps working on the protocols it already has.
				fmt.Printf("zhdata: skipping client %q, cannot parse config: %v\n", client.Name, err)
				continue
			}
		}
		if _, exists := config["zhdata"]; exists {
			continue
		}

		entry := map[string]string{
			"name": client.Name,
			"uuid": zhdataClientUUID(config),
		}
		encoded, err := json.Marshal(entry)
		if err != nil {
			return 0, err
		}
		config["zhdata"] = encoded

		encoded, err = json.Marshal(config)
		if err != nil {
			return 0, err
		}
		// Written as raw bytes, not a string: the column holds a BLOB
		// everywhere else, and a TEXT value lands there as something GORM
		// cannot scan back into json.RawMessage.
		if err := tx.Exec("UPDATE clients SET config = ? WHERE id = ?", encoded, client.Id).Error; err != nil {
			return 0, err
		}
		changed++
	}
	return changed, nil
}

// zhdataClientUUID reuses the client's vless uuid when present, and mints a
// v4 otherwise.
func zhdataClientUUID(config map[string]json.RawMessage) string {
	var vless struct {
		UUID string `json:"uuid"`
	}
	if raw, ok := config["vless"]; ok {
		if err := json.Unmarshal(raw, &vless); err == nil && vless.UUID != "" {
			return vless.UUID
		}
	}
	return uuid.Must(uuid.NewV4()).String()
}
