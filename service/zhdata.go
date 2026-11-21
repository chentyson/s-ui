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

	"github.com/alireza0/s-ui/core"
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
	up      int64
	down    int64
	devices int
}

// zhdataSnapshots is keyed by inbound tag, then uuid. The tag key is what
// keeps two zhdata processes separate, and mapping uuid -> counters within a
// tag is the P0 dedup for a uuid reported twice by one process.
var (
	zhdataStatsMu sync.Mutex
	zhdataSnapshots = map[string]map[string]zhdataSnapshotEntry{}
)

// zhdataOnline* is the online overlay the stats cron merges into
// onlineResources (service/stats.go): the panel's online badges (user list,
// header counts, inbound card marks) read that cache, not clients.online_at.
// Stale samples are dropped, so a zhdata process that stopped reporting
// stops marking anyone online. The 45s window is 3 collection cycles.
const zhdataOnlineStaleness = 45

var (
	zhdataOnlineMu        sync.Mutex
	zhdataOnlineSampledAt int64
	zhdataOnlineUsers     []string // client names
	zhdataOnlineInbounds  []string // inbound tags with at least one active device
)

// mergeZhdataOnline folds the overlay into a freshly built onlineResources
// (called by SaveStats right before it publishes one).
func mergeZhdataOnline(online *onlines) {
	zhdataOnlineMu.Lock()
	defer zhdataOnlineMu.Unlock()
	if zhdataOnlineSampledAt == 0 || time.Now().Unix()-zhdataOnlineSampledAt > zhdataOnlineStaleness {
		return
	}
	seenUser := make(map[string]bool, len(online.User))
	for _, name := range online.User {
		seenUser[name] = true
	}
	for _, name := range zhdataOnlineUsers {
		if !seenUser[name] {
			seenUser[name] = true
			online.User = append(online.User, name)
		}
	}
	seenInbound := make(map[string]bool, len(online.Inbound))
	for _, tag := range online.Inbound {
		seenInbound[tag] = true
	}
	for _, tag := range zhdataOnlineInbounds {
		if !seenInbound[tag] {
			seenInbound[tag] = true
			online.Inbound = append(online.Inbound, tag)
		}
	}
}

// CollectZhdataStats samples every zhdata process and folds the sample into
// the panel's books, one transaction:
//   - traffic: per-uuid deltas credited into clients.up/down with gorm.Expr
//     accumulation (stats.go flush semantics); a counter that went down (the
//     zhdata process restarted) resets the snapshot without crediting a
//     negative.
//   - online: a uuid reported with devices > 0 refreshes its client's
//     online_at, the same marker stats.go:127 sets for core sessions; the
//     uuid's client name and the inbound tag are overlaid onto
//     onlineResources so the panel's online badges see zhdata traffic too.
//     devices == 0 refreshes nothing -- offline clients keep their last
//     online_at and age out by the frontend window.
//
// A uuid bound to several zhdata inbounds is credited and refreshed exactly
// once: per-uuid deltas are summed across processes before the write.
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

	type traffic struct{ up, down int64 }
	crossTraffic := map[string]*traffic{}
	onlineUuids := map[string]bool{}
	var onlineTags []string
	sampledAny := false

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
		sampledAny = true

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
			entry.devices += user.Devices
			current[user.UUID] = entry
		}

		zhdataStatsMu.Lock()
		previous, existed := zhdataSnapshots[inbound.Tag]
		zhdataSnapshots[inbound.Tag] = current
		zhdataStatsMu.Unlock()

		tagOnline := false
		for uuid, entry := range current {
			if entry.devices > 0 {
				onlineUuids[uuid] = true
				tagOnline = true
			}
			if !existed {
				// First sample after the panel started (or after the inbound
				// was (re)created): counters before this point are not the
				// panel's to credit.
				continue
			}
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
			t, ok := crossTraffic[uuid]
			if !ok {
				t = &traffic{}
				crossTraffic[uuid] = t
			}
			t.up += upDelta
			t.down += downDelta
		}
		if tagOnline {
			onlineTags = append(onlineTags, inbound.Tag)
		}
	}

	if !sampledAny {
		// No process answered: the overlay ages out on its own rather than
		// being stamped with a sample that never happened.
		return
	}

	// An all-idle sample (nothing online, no delta) still refreshes the
	// overlay: the panel then shows nobody from zhdata, instead of clinging
	// to the previous sample until it ages out.
	namesByUuid := map[string]string{}
	onlineUsers := []string{}
	now := time.Now().Unix()

	if len(onlineUuids) > 0 || len(crossTraffic) > 0 {
		// uuid -> panel client, one query for the whole cycle.
		uuids := make([]string, 0, len(onlineUuids)+len(crossTraffic))
		for uuid := range onlineUuids {
			uuids = append(uuids, uuid)
		}
		for uuid := range crossTraffic {
			if !onlineUuids[uuid] {
				uuids = append(uuids, uuid)
			}
		}
		type uuidName struct {
			UUID string
			Name string
		}
		var resolved []uuidName
		err = db.Raw("SELECT json_extract(config, '$.zhdata.uuid') AS uuid, name FROM clients WHERE json_extract(config, '$.zhdata.uuid') IN ?", uuids).Scan(&resolved).Error
		if err != nil {
			logger.Warning("zhdata stats: could not resolve clients: ", err)
			return
		}
		for _, r := range resolved {
			namesByUuid[r.UUID] = r.Name
		}

		tx := db.Begin()
		var txErr error
		for _, uuid := range uuids {
			update := map[string]any{}
			if onlineUuids[uuid] {
				update["online_at"] = now
			}
			if t, ok := crossTraffic[uuid]; ok {
				if t.up > 0 {
					update["up"] = gorm.Expr("up + ?", t.up)
				}
				if t.down > 0 {
					update["down"] = gorm.Expr("down + ?", t.down)
				}
			}
			if len(update) == 0 {
				continue
			}
			if txErr = tx.Model(model.Client{}).
				Where("json_extract(config, '$.zhdata.uuid') = ?", uuid).
				Updates(update).Error; txErr != nil {
				break
			}
		}
		if txErr != nil {
			tx.Rollback()
			logger.Warning("zhdata stats: failed to credit traffic/online_at: ", txErr)
			return
		}
		if txErr = tx.Commit().Error; txErr != nil {
			logger.Warning("zhdata stats: failed to commit traffic/online_at: ", txErr)
			return
		}

		onlineUsers = make([]string, 0, len(onlineUuids))
		seenName := map[string]bool{}
		for uuid := range onlineUuids {
			if name, ok := namesByUuid[uuid]; ok && !seenName[name] {
				seenName[name] = true
				onlineUsers = append(onlineUsers, name)
			}
		}
	}

	zhdataOnlineMu.Lock()
	zhdataOnlineUsers = onlineUsers
	zhdataOnlineInbounds = onlineTags
	zhdataOnlineSampledAt = now
	zhdataOnlineMu.Unlock()
}

// ForgetZhdataSnapshot drops the traffic snapshot of an inbound that is being
// deleted, so a recreated tag does not inherit (and suppress) the old counts.
func ForgetZhdataSnapshot(tag string) {
	zhdataStatsMu.Lock()
	delete(zhdataSnapshots, tag)
	zhdataStatsMu.Unlock()
}

// ---------------- live sessions (contract: GET /v1/sessions) ----------------

// zhdataSession is one entry of GET /v1/sessions: a live tunnel session
// (uuid+device on a slot), not one per application stream.
type zhdataSession struct {
	UUID      string `json:"uuid"`
	Device    string `json:"device"`
	Slot      int    `json:"slot"`
	Remote    string `json:"remote"`
	StartedAt int64  `json:"started_at"`
	Relay     bool   `json:"relay"`
}

// isZhdataInboundTag reports whether a tag belongs to a zhdata inbound.
func isZhdataInboundTag(db *gorm.DB, tag string) bool {
	var count int64
	if err := db.Model(model.Inbound{}).
		Where("tag = ? AND type = ?", tag, zhdataType).
		Count(&count).Error; err != nil {
		return false
	}
	return count > 0
}

// zhdataInboundByTag loads one zhdata inbound row by tag.
func zhdataInboundByTag(db *gorm.DB, tag string) (*model.Inbound, error) {
	var inbound model.Inbound
	if err := db.Model(model.Inbound{}).
		Where("tag = ? AND type = ?", tag, zhdataType).
		First(&inbound).Error; err != nil {
		if err == gorm.ErrRecordNotFound {
			return nil, nil
		}
		return nil, err
	}
	return &inbound, nil
}

// zhdataNamesForUUIDs maps zhdata uuids to panel client names in one query.
// uuids with no client (deleted row, external keysource entry) stay absent.
func zhdataNamesForUUIDs(db *gorm.DB, uuids []string) map[string]string {
	names := map[string]string{}
	if len(uuids) == 0 {
		return names
	}
	type uuidName struct {
		UUID string
		Name string
	}
	var resolved []uuidName
	if err := db.Raw("SELECT json_extract(config, '$.zhdata.uuid') AS uuid, name FROM clients WHERE json_extract(config, '$.zhdata.uuid') IN ?", uuids).Scan(&resolved).Error; err != nil {
		logger.Warning("zhdata: could not resolve client names: ", err)
		return names
	}
	for _, r := range resolved {
		names[r.UUID] = r.Name
	}
	return names
}

// zhdataSessions fetches one zhdata process' live sessions and maps them to
// the panel's SessionInfo shape. filterUUID, when set, narrows to one client
// before mapping. A failed pull warns and counts as empty (the panel's
// sessions modal refreshes on its own; no caching here).
func zhdataSessions(db *gorm.DB, inbound *model.Inbound, filterUUID string) []core.SessionInfo {
	opts, err := parseZhdataOptions(inbound.Options)
	if err != nil || opts.AdminAddr == "" {
		return nil
	}
	response, err := zhdataAdminRequest(opts, http.MethodGet, "/v1/sessions", nil)
	if err != nil {
		logger.Warning("zhdata sessions: ", inbound.Tag, " (", opts.AdminAddr, "): ", err)
		return nil
	}
	var payload struct {
		Ts       int64           `json:"ts"`
		Sessions []zhdataSession `json:"sessions"`
	}
	if err := json.Unmarshal(response, &payload); err != nil {
		logger.Warning("zhdata sessions: ", inbound.Tag, ": unreadable response: ", err)
		return nil
	}
	if filterUUID != "" {
		kept := payload.Sessions[:0]
		for _, session := range payload.Sessions {
			if session.UUID == filterUUID {
				kept = append(kept, session)
			}
		}
		payload.Sessions = kept
	}
	if len(payload.Sessions) == 0 {
		return nil
	}

	uuids := make([]string, 0, len(payload.Sessions))
	for _, session := range payload.Sessions {
		if session.UUID != "" {
			uuids = append(uuids, session.UUID)
		}
	}
	names := zhdataNamesForUUIDs(db, uuids)

	// core.SessionInfo shape (what Sessions.vue renders): a tunnel session
	// has no destination of its own (application streams ride it and the
	// zhdata server resolves them), so destination stays "" and per-session
	// counters are 0 -- the uuid's totals come from /v1/stats. CreatedAt is
	// Unix seconds, the unit the tracker's snapshot uses.
	info := make([]core.SessionInfo, 0, len(payload.Sessions))
	for _, session := range payload.Sessions {
		user := names[session.UUID]
		if user == "" {
			user = session.UUID
		}
		entry := core.SessionInfo{
			ID:        fmt.Sprintf("%s:%s:%d", inbound.Tag, session.Device, session.Slot),
			Inbound:   inbound.Tag,
			User:      user,
			Network:   "zhdata",
			Source:    session.Remote,
			CreatedAt: session.StartedAt,
		}
		if session.Relay {
			entry.Rule = "relay"
		}
		info = append(info, entry)
	}
	return info
}

// zhdataSessionsForUser pulls the live sessions of every zhdata process for
// one panel client, merged. A client without zhdata credentials gets nothing.
func zhdataSessionsForUser(db *gorm.DB, name string) []core.SessionInfo {
	if name == "" {
		return nil
	}
	var uuid string
	if err := db.Raw("SELECT json_extract(config, '$.zhdata.uuid') FROM clients WHERE name = ?", name).Scan(&uuid).Error; err != nil {
		logger.Warning("zhdata sessions: could not resolve uuid of client ", name, ": ", err)
		return nil
	}
	if uuid == "" {
		return nil
	}
	inbounds, err := zhdataInbounds(db)
	if err != nil {
		logger.Warning("zhdata sessions: could not list zhdata inbounds: ", err)
		return nil
	}
	var sessions []core.SessionInfo
	for _, inbound := range inbounds {
		sessions = append(sessions, zhdataSessions(db, inbound, uuid)...)
	}
	return sessions
}
