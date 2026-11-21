package service

// zhdata external inbound support (docs/zhdata-admin-api.md).
//
// A zhdata inbound is NOT part of the embedded sing-box config: the zhdata
// server runs as its own process (systemd), and the panel is the single source
// of truth for clients, credentials and limits. The panel pushes the enabled
// client set to that process' admin API (default loopback), samples per-user
// traffic from it, and relays kicks to it. Every admin call is best-effort: a
// zhdata process that is not up must never fail a panel operation, so callers
// only log warnings.
//
// Client credentials live in clients.config.zhdata = {"name": ..., "uuid": ...}
// (contract §3); the device limit is the `devs` column, the same field
// loadDeviceLimits feeds to devlimit.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alireza0/s-ui/database"
	"github.com/alireza0/s-ui/database/model"
	"github.com/alireza0/s-ui/logger"

	"gorm.io/gorm"
)

// zhdataType is the external inbound type registered by the panel.
const zhdataType = "zhdata"

// isZhdataInbound marks the type as external: no embedded core config, no
// corePtr.AddInbound/RemoveInbound.
func isZhdataInbound(inboundType string) bool {
	return inboundType == zhdataType
}

// zhdataAdminTimeout bounds every admin API call. A dead zhdata process must
// not stall a save; the push happens after the DB commit and only warns.
const zhdataAdminTimeout = 5 * time.Second

// zhdataOptions are the fields a zhdata inbound row carries (contract §2).
// They mirror the zhdata server config names so the same row can one day
// generate `-c config.json`.
type zhdataOptions struct {
	Tag         string   `json:"tag"`
	Listen      string   `json:"listen"`
	Backend     string   `json:"backend"`
	MaxSessions int      `json:"max_sessions"`
	Decoy       string   `json:"decoy"`
	AdminAddr   string   `json:"admin_addr"`
	AdminToken  string   `json:"admin_token"`
	// Upstream turns this zhdata inbound into a transparent relay: a
	// comma-list free array of host:port zhdata upstreams (contract §6).
	// Empty/absent = ordinary direct backend.
	Upstream []string `json:"upstream"`
}

func parseZhdataOptions(options json.RawMessage) (*zhdataOptions, error) {
	var opts zhdataOptions
	if err := json.Unmarshal(options, &opts); err != nil {
		return nil, err
	}
	return &opts, nil
}

// zhdataInbounds lists every zhdata inbound row.
func zhdataInbounds(db *gorm.DB) ([]*model.Inbound, error) {
	var inbounds []*model.Inbound
	err := db.Model(model.Inbound{}).Where("type = ?", zhdataType).Find(&inbounds).Error
	if err != nil {
		return nil, err
	}
	return inbounds, nil
}

// zhdataAdminRequest performs one admin API call against one inbound's
// process. Empty admin_addr means the inbound opted out of the admin API.
func zhdataAdminRequest(opts *zhdataOptions, method string, path string, body any) ([]byte, error) {
	if opts.AdminAddr == "" {
		return nil, nil
	}
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		reader = bytes.NewReader(encoded)
	}
	url := "http://" + opts.AdminAddr + path
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if opts.AdminToken != "" {
		req.Header.Set("Authorization", "Bearer " + opts.AdminToken)
	}
	client := &http.Client{Timeout: zhdataAdminTimeout}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zhdata admin %s %s: status %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(responseBody)))
	}
	return responseBody, nil
}

// zhdataAdminUsers is one entry of the PUT /v1/users payload.
type zhdataAdminUsers struct {
	UUID       string `json:"uuid"`
	MaxDevices int    `json:"max_devices"`
}

// zhdataEnabledUsers reads the clients every zhdata process should serve:
// every enabled client with a config.zhdata.uuid, carrying the same device
// limit the panel feeds to devlimit (`devs`, 0 = no limit).
func zhdataEnabledUsers(db *gorm.DB) ([]zhdataAdminUsers, error) {
	type row struct {
		UUID       string
		MaxDevices int
	}
	var rows []row
	const base = "FROM clients WHERE clients.enable = true AND json_extract(clients.config, '$.zhdata.uuid') IS NOT NULL"
	err := db.Raw("SELECT json_extract(clients.config, '$.zhdata.uuid') AS uuid, COALESCE(clients.devs, 0) AS max_devices " + base).Scan(&rows).Error
	if err != nil {
		// A database that predates the devs column still has usable users.
		rows = nil
		if err2 := db.Raw("SELECT json_extract(clients.config, '$.zhdata.uuid') AS uuid, 0 AS max_devices " + base).Scan(&rows).Error; err2 != nil {
			return nil, err
		}
	}
	users := make([]zhdataAdminUsers, 0, len(rows))
	for _, r := range rows {
		if r.UUID == "" {
			continue
		}
		users = append(users, zhdataAdminUsers{UUID: r.UUID, MaxDevices: r.MaxDevices})
	}
	return users, nil
}

// PushZhdataUsers replaces the API user set of every zhdata process with the
// panel's enabled clients (contract §1.2/§2). Best-effort: a zhdata process
// that is down warns and moves on, because the file/sqlite hot-reload source
// still covers it and the next push (or restart) reconciles.
func PushZhdataUsers(db *gorm.DB) {
	if db == nil {
		return
	}
	inbounds, err := zhdataInbounds(db)
	if err != nil {
		logger.Warning("zhdata: could not list zhdata inbounds: ", err)
		return
	}
	if len(inbounds) == 0 {
		return
	}
	users, err := zhdataEnabledUsers(db)
	if err != nil {
		logger.Warning("zhdata: could not read enabled clients: ", err)
		return
	}
	for _, inbound := range inbounds {
		opts, err := parseZhdataOptions(inbound.Options)
		if err != nil {
			logger.Warning("zhdata: inbound ", inbound.Tag, " has unreadable options: ", err)
			continue
		}
		if opts.AdminAddr == "" {
			continue
		}
		body := struct {
			Users []zhdataAdminUsers `json:"users"`
		}{Users: users}
		if users == nil {
			body.Users = []zhdataAdminUsers{}
		}
		response, err := zhdataAdminRequest(opts, http.MethodPut, "/v1/users", body)
		if err != nil {
			// A failure here is expected to be loud: an unreachable admin
			// usually means the zhdata process is down, or its admin_addr
			// collides with another listener and the process could not
			// start at all (the process exits on an admin bind failure).
			logger.Warning("zhdata: push users to ", inbound.Tag, " (", opts.AdminAddr, ") failed — zhdata process down or admin_addr in use: ", err)
			continue
		}
		logger.Info("zhdata: pushed ", len(users), " user(s) to ", inbound.Tag, ": ", strings.TrimSpace(string(response)))
	}
}

// PushZhdataUsersLatest re-reads the zhdata inbounds from the live database
// and pushes. This is the entry point for the save paths and the cron jobs.
func PushZhdataUsersLatest() {
	PushZhdataUsers(database.GetDB())
}

// PushZhdataRelay sets the transparent-relay state of every zhdata process
// (contract §6): PUT /v1/relay with the inbound's upstream host:port list.
// An empty list disables relay and the process serves its direct backend
// again. Like the user push this is best-effort: an unreachable process only
// warns, and requires zhdata core >= 0.15.2 (older processes answer 404,
// which lands in the same warning).
func PushZhdataRelay(db *gorm.DB) {
	if db == nil {
		return
	}
	inbounds, err := zhdataInbounds(db)
	if err != nil {
		logger.Warning("zhdata: could not list zhdata inbounds: ", err)
		return
	}
	for _, inbound := range inbounds {
		opts, err := parseZhdataOptions(inbound.Options)
		if err != nil {
			logger.Warning("zhdata: inbound ", inbound.Tag, " has unreadable options: ", err)
			continue
		}
		if opts.AdminAddr == "" {
			continue
		}
		upstream := opts.Upstream
		if upstream == nil {
			upstream = []string{}
		}
		body := struct {
			Upstream []string `json:"upstream"`
			Enabled  bool     `json:"enabled"`
		}{Upstream: upstream, Enabled: len(upstream) > 0}
		response, err := zhdataAdminRequest(opts, http.MethodPut, "/v1/relay", body)
		if err != nil {
			logger.Warning("zhdata: push relay to ", inbound.Tag, " (", opts.AdminAddr, ") failed — zhdata process down, admin_addr in use, or core < 0.15.2: ", err)
			continue
		}
		logger.Info("zhdata: relay of ", inbound.Tag, " set to ", len(upstream), " upstream(s): ", strings.TrimSpace(string(response)))
	}
}

// PushZhdataRelayLatest is the cron/save-path entry point, mirroring
// PushZhdataUsersLatest.
func PushZhdataRelayLatest() {
	PushZhdataRelay(database.GetDB())
}

// KickZhdataUser cuts one user's sessions on every zhdata process, next to
// the embedded core kick (contract §1.4/§2). The user name is resolved to its
// config.zhdata.uuid; a client without zhdata credentials needs nothing.
func KickZhdataUser(user string) {
	if user == "" {
		return
	}
	db := database.GetDB()
	if db == nil {
		return
	}
	var uuid string
	err := db.Raw("SELECT json_extract(config, '$.zhdata.uuid') FROM clients WHERE name = ?", user).Scan(&uuid).Error
	if err != nil {
		logger.Warning("zhdata: could not resolve uuid of client ", user, ": ", err)
		return
	}
	if uuid == "" {
		return
	}
	inbounds, err := zhdataInbounds(db)
	if err != nil {
		logger.Warning("zhdata: could not list zhdata inbounds: ", err)
		return
	}
	for _, inbound := range inbounds {
		opts, err := parseZhdataOptions(inbound.Options)
		if err != nil || opts.AdminAddr == "" {
			continue
		}
		body := struct {
			UUID string `json:"uuid"`
		}{UUID: uuid}
		response, err := zhdataAdminRequest(opts, http.MethodPost, "/v1/users/kick", body)
		if err != nil {
			logger.Warning("zhdata: kick of ", user, " on ", inbound.Tag, " (", opts.AdminAddr, ") failed: ", err)
			continue
		}
		logger.Info("zhdata: kicked sessions of ", user, " on ", inbound.Tag, ": ", strings.TrimSpace(string(response)))
	}
}

// zhdataStats is one GET /v1/stats response (contract §1.3). Counters are
// cumulative since the zhdata process started, so the panel keeps a per
// inbound snapshot and credits the deltas, the same way the session tracker
// flush feeds clients.up/down.
type zhdataStats struct {
	Ts             int64 `json:"ts"`
	SessionsActive int   `json:"sessions_active"`
	Users          []struct {
		UUID    string `json:"uuid"`
		Devices int    `json:"devices"`
		Up      int64  `json:"up"`
		Down    int64  `json:"down"`
	} `json:"users"`
}

type zhdataSnapshotEntry struct {
	up   int64
	down int64
}

// zhdataSnapshots is keyed by inbound tag, then uuid. The tag key is what
// keeps two zhdata processes separate, and mapping uuid -> counters within a
// tag is the P0 dedup for a uuid reported twice by one process.
var (
	zhdataStatsMu sync.Mutex
	zhdataSnapshots = map[string]map[string]zhdataSnapshotEntry{}
)

// CollectZhdataStats samples every zhdata process and credits the per-uuid
// traffic delta into clients.up/down with gorm.Expr accumulation, matching the
// stats.go flush semantics. A counter that went down (the zhdata process
// restarted) resets the snapshot without crediting a negative.
func CollectZhdataStats() {
	db := database.GetDB()
	if db == nil {
		return
	}
	inbounds, err := zhdataInbounds(db)
	if err != nil {
		logger.Warning("zhdata stats: could not list zhdata inbounds: ", err)
		return
	}
	for _, inbound := range inbounds {
		opts, err := parseZhdataOptions(inbound.Options)
		if err != nil || opts.AdminAddr == "" {
			continue
		}
		response, err := zhdataAdminRequest(opts, http.MethodGet, "/v1/stats", nil)
		if err != nil {
			logger.Warning("zhdata stats: ", inbound.Tag, " (", opts.AdminAddr, "): ", err)
			continue
		}
		var stats zhdataStats
		if err := json.Unmarshal(response, &stats); err != nil {
			logger.Warning("zhdata stats: ", inbound.Tag, ": unreadable response: ", err)
			continue
		}

		// Dedup within one tag: one uuid can only be reported once per
		// sample; a duplicate row folds into the first.
		current := make(map[string]zhdataSnapshotEntry, len(stats.Users))
		for _, user := range stats.Users {
			if user.UUID == "" {
				continue
			}
			entry := current[user.UUID]
			entry.up += user.Up
			entry.down += user.Down
			current[user.UUID] = entry
		}

		zhdataStatsMu.Lock()
		previous, existed := zhdataSnapshots[inbound.Tag]
		zhdataSnapshots[inbound.Tag] = current
		zhdataStatsMu.Unlock()

		if !existed {
			// First sample after the panel started (or after the inbound was
			// (re)created): counters before this point are not the panel's to
			// credit, unless the process itself started after the panel.
			continue
		}

		for uuid, entry := range current {
			upDelta := entry.up - previous[uuid].up
			downDelta := entry.down - previous[uuid].down
			if upDelta < 0 {
				upDelta = 0
			}
			if downDelta < 0 {
				downDelta = 0
			}
			if upDelta == 0 && downDelta == 0 {
				continue
			}
			update := map[string]any{}
			if upDelta > 0 {
				update["up"] = gorm.Expr("up + ?", upDelta)
			}
			if downDelta > 0 {
				update["down"] = gorm.Expr("down + ?", downDelta)
			}
			if err := db.Model(model.Client{}).
				Where("json_extract(config, '$.zhdata.uuid') = ?", uuid).
				Updates(update).Error; err != nil {
				logger.Warning("zhdata stats: failed to credit traffic of ", uuid, " on ", inbound.Tag, ": ", err)
			}
		}
	}
}

// ForgetZhdataSnapshot drops the traffic snapshot of an inbound that is being
// deleted, so a recreated tag does not inherit (and suppress) the old counts.
func ForgetZhdataSnapshot(tag string) {
	zhdataStatsMu.Lock()
	delete(zhdataSnapshots, tag)
	zhdataStatsMu.Unlock()
}
