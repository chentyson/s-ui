package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alireza0/s-ui/core"
	"github.com/alireza0/s-ui/database"
	"github.com/alireza0/s-ui/database/model"

	"gorm.io/gorm"
)

func zhdataTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	if err := database.InitDB(filepath.Join(t.TempDir(), "test.db")); err != nil {
		t.Fatal(err)
	}
	return database.GetDB()
}

func seedZhdataInbound(t *testing.T, db *gorm.DB, tag string, options string) *model.Inbound {
	t.Helper()
	inbound := &model.Inbound{
		Type:    zhdataType,
		Tag:     tag,
		Addrs:   json.RawMessage(`[]`),
		OutJson: json.RawMessage(`{}`),
		Options: json.RawMessage(options),
	}
	if err := db.Create(inbound).Error; err != nil {
		t.Fatal(err)
	}
	return inbound
}

func seedZhdataClient(t *testing.T, db *gorm.DB, name string, devs int, uuid string, inbounds []uint) *model.Client {
	t.Helper()
	inboundsJson, _ := json.Marshal(inbounds)
	client := &model.Client{
		Name:     name,
		Enable:   true,
		Config:   json.RawMessage(`{"zhdata":{"name":"` + name + `","uuid":"` + uuid + `"}}`),
		Inbounds: json.RawMessage(inboundsJson),
		Links:    json.RawMessage(`[]`),
	}
	if err := db.Create(client).Error; err != nil {
		t.Fatal(err)
	}
	if devs >= 0 {
		if err := db.Model(model.Client{}).Where("id = ?", client.Id).Update("devs", devs).Error; err != nil {
			t.Fatal(err)
		}
	}
	return client
}

// zhdata is external: it must not appear in the sing-box config the panel
// hands the embedded core (contract §2).
func TestGetAllConfigFiltersZhdata(t *testing.T) {
	db := zhdataTestDB(t)

	vless := &model.Inbound{
		Type:    "vless",
		Tag:     "vless-in",
		Addrs:   json.RawMessage(`[]`),
		OutJson: json.RawMessage(`{}`),
		Options: json.RawMessage(`{"listen":"::","listen_port":2096}`),
	}
	if err := db.Create(vless).Error; err != nil {
		t.Fatal(err)
	}
	seedZhdataInbound(t, db, "zhdata-in", `{"listen":"","listen_ports":[49888,49889],"backend":"direct","admin_addr":"127.0.0.1:6070"}`)
	seedZhdataClient(t, db, "someone", 2, "aaaaaaaa-0000-4000-8000-000000000001", []uint{vless.Id})

	s := &InboundService{}
	configs, err := s.GetAllConfig(db)
	if err != nil {
		t.Fatal(err)
	}
	if len(configs) != 1 {
		t.Fatalf("expected only the vless inbound in the core config, got %d: %v", len(configs), configs)
	}
	var decoded map[string]any
	if err := json.Unmarshal(configs[0], &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["type"] != "vless" || decoded["tag"] != "vless-in" {
		t.Errorf("unexpected inbound in the core config: %v", decoded)
	}
}

// hasUser("zhdata") is true and the user objects injected for it are the
// clients.config.zhdata shape {name, uuid}.
func TestAddUsersZhdataShape(t *testing.T) {
	db := zhdataTestDB(t)
	inbound := seedZhdataInbound(t, db, "zhdata-in", `{"listen":"","listen_ports":[49888]}`)
	seedZhdataClient(t, db, "someone", 2, "aaaaaaaa-0000-4000-8000-000000000001", []uint{inbound.Id})

	s := &InboundService{}
	out, err := s.addUsers(db, []byte(`{"type":"zhdata","tag":"zhdata-in"}`), inbound.Id, zhdataType)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Users []map[string]string `json:"users"`
	}
	if err := json.Unmarshal(out, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Users) != 1 {
		t.Fatalf("expected one zhdata user, got %s", out)
	}
	if decoded.Users[0]["name"] != "someone" || decoded.Users[0]["uuid"] != "aaaaaaaa-0000-4000-8000-000000000001" {
		t.Errorf("user shape = %v, want {name: someone, uuid: aaaa...}", decoded.Users[0])
	}
}

// Saving a zhdata inbound with a RUNNING core must not reach AddInbound --
// the embedded core has no zhdata type, so before the external-type skip this
// failed with an unknown inbound type error.
func TestZhdataInboundSaveSkipsEmbeddedCore(t *testing.T) {
	db := zhdataTestDB(t)

	prev := corePtr
	c := core.NewCore()
	if err := c.Start([]byte(`{"log":{"level":"error"}}`)); err != nil {
		t.Fatal(err)
	}
	corePtr = c
	t.Cleanup(func() {
		_ = c.Stop()
		corePtr = prev
	})

	if !c.IsRunning() {
		t.Fatal("expected the core to be running for this test")
	}

	s := &InboundService{}
	tx := db.Begin()
	data := json.RawMessage(`{"type":"zhdata","tag":"zhdata-1","listen":"","listen_ports":[49888,49889],"backend":"direct","admin_addr":"127.0.0.1:6070"}`)
	if err := s.Save(tx, "new", data, "", "127.0.0.1"); err != nil {
		tx.Rollback()
		t.Fatalf("saving a zhdata inbound failed (it must not reach the embedded core): %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}

	var count int64
	if err := db.Model(model.Inbound{}).Where("tag = ?", "zhdata-1").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("zhdata inbound not stored, count=%d", count)
	}
}

// Deleting a zhdata inbound likewise must not reach RemoveInbound of the
// embedded core.
func TestZhdataInboundDeleteSkipsEmbeddedCore(t *testing.T) {
	db := zhdataTestDB(t)

	prev := corePtr
	c := core.NewCore()
	if err := c.Start([]byte(`{"log":{"level":"error"}}`)); err != nil {
		t.Fatal(err)
	}
	corePtr = c
	t.Cleanup(func() {
		_ = c.Stop()
		corePtr = prev
	})

	seedZhdataInbound(t, db, "zhdata-1", `{"listen":"","listen_ports":[49888],"admin_addr":"127.0.0.1:6070"}`)

	s := &InboundService{}
	tx := db.Begin()
	if err := s.Save(tx, "del", json.RawMessage(`"zhdata-1"`), "", "127.0.0.1"); err != nil {
		tx.Rollback()
		t.Fatalf("deleting a zhdata inbound failed: %v", err)
	}
	if err := tx.Commit().Error; err != nil {
		t.Fatal(err)
	}

	var count int64
	if err := db.Model(model.Inbound{}).Where("tag = ?", "zhdata-1").Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("zhdata inbound still present, count=%d", count)
	}
}

// fakeZhdataAdmin stands in for a zhdata process' admin API on loopback.
type fakeZhdataAdmin struct {
	mu       sync.Mutex
	users    []byte
	kicks    []string
	relays   []string
	stats    string
	sessions string
	queries  []string
	requests int
}

func (f *fakeZhdataAdmin) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	switch {
	case r.Method == http.MethodPut && r.URL.Path == "/v1/users":
		body, _ := io.ReadAll(r.Body)
		f.users = body
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"keys":1}`))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/users/kick":
		body, _ := io.ReadAll(r.Body)
		f.kicks = append(f.kicks, string(body))
		_, _ = w.Write([]byte(`{"kicked":1}`))
	case r.Method == http.MethodPut && r.URL.Path == "/v1/relay":
		body, _ := io.ReadAll(r.Body)
		f.relays = append(f.relays, string(body))
		_, _ = w.Write([]byte(`{"ok":true}`))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/stats":
		_, _ = w.Write([]byte(f.stats))
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sessions":
		f.queries = append(f.queries, r.URL.RawQuery)
		_, _ = w.Write([]byte(f.sessions))
	default:
		http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
	}
}

// The admin push sends every enabled client's zhdata uuid with the same
// device limit (devs) the panel feeds to devlimit.
func TestPushZhdataUsers(t *testing.T) {
	db := zhdataTestDB(t)

	admin := &fakeZhdataAdmin{}
	server := httptest.NewServer(admin)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	seedZhdataInbound(t, db, "zhdata-in", `{"listen":"","listen_ports":[49888],"admin_addr":"`+addr+`","admin_token":"sekret"}`)
	seedZhdataClient(t, db, "alice", 2, "aaaaaaaa-0000-4000-8000-000000000001", []uint{})
	seedZhdataClient(t, db, "bob", 0, "bbbbbbbb-0000-4000-8000-000000000002", []uint{})
	// No zhdata credentials: not pushable.
	db.Create(&model.Client{Name: "plain", Enable: true, Config: json.RawMessage(`{"trojan":{"name":"plain","password":"x"}}`),
		Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(`[]`)})

	PushZhdataUsers(db)

	admin.mu.Lock()
	defer admin.mu.Unlock()
	if admin.requests != 1 {
		t.Fatalf("expected exactly one PUT, got %d", admin.requests)
	}
	var payload struct {
		Users []struct {
			UUID       string `json:"uuid"`
			MaxDevices int    `json:"max_devices"`
		} `json:"users"`
		Authorization string
	}
	if err := json.Unmarshal(admin.users, &payload); err != nil {
		t.Fatalf("users payload is not the contract shape: %v: %s", err, admin.users)
	}
	if len(payload.Users) != 2 {
		t.Fatalf("expected the two zhdata clients, got %s", admin.users)
	}
	for _, u := range payload.Users {
		switch u.UUID {
		case "aaaaaaaa-0000-4000-8000-000000000001":
			if u.MaxDevices != 2 {
				t.Errorf("alice max_devices = %d, want 2 (her devs column)", u.MaxDevices)
			}
		case "bbbbbbbb-0000-4000-8000-000000000002":
			if u.MaxDevices != 0 {
				t.Errorf("bob max_devices = %d, want 0 (no limit)", u.MaxDevices)
			}
		default:
			t.Errorf("unexpected uuid pushed: %q", u.UUID)
		}
	}
}

// Stats deltas land in clients.up/down (the panel column is core + zhdata).
func TestCollectZhdataStatsCreditsDeltas(t *testing.T) {
	db := zhdataTestDB(t)

	admin := &fakeZhdataAdmin{}
	server := httptest.NewServer(admin)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	inbound := seedZhdataInbound(t, db, "zhdata-stats", `{"listen":"","listen_ports":[49888],"admin_addr":"`+addr+`"}`)
	_ = inbound
	client := seedZhdataClient(t, db, "carol", 0, "cccccccc-0000-4000-8000-000000000003", []uint{})

	admin.mu.Lock()
	admin.stats = `{"ts":1,"sessions_active":1,"users":[{"uuid":"cccccccc-0000-4000-8000-000000000003","devices":1,"up":100,"down":200}]}`
	admin.mu.Unlock()

	// First sample: the baseline. Counters already at 100/200 when the panel
	// started watching are not the panel's to credit.
	ForgetZhdataSnapshot("zhdata-stats")
	CollectZhdataStats()
	var stored model.Client
	if err := db.Where("id = ?", client.Id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Up != 0 || stored.Down != 0 {
		t.Fatalf("first sample credited %d/%d, want 0/0", stored.Up, stored.Down)
	}

	admin.mu.Lock()
	admin.stats = `{"ts":2,"sessions_active":1,"users":[{"uuid":"cccccccc-0000-4000-8000-000000000003","devices":1,"up":350,"down":900},{"uuid":"cccccccc-0000-4000-8000-000000000003","devices":1,"up":50,"down":0}]}`
	admin.mu.Unlock()

	CollectZhdataStats()

	if err := db.Where("id = ?", client.Id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	// The duplicate uuid in one tag folds into one entry (P0 dedup): the
	// second row pushes the reported total to 400/900 -> delta 300/700.
	if stored.Up != 300 || stored.Down != 700 {
		t.Fatalf("credited %d/%d, want 300/700", stored.Up, stored.Down)
	}

	// A zhdata process restart (counters go backwards) credits nothing and
	// only resets the baseline.
	admin.mu.Lock()
	admin.stats = `{"ts":3,"sessions_active":0,"users":[{"uuid":"cccccccc-0000-4000-8000-000000000003","devices":0,"up":10,"down":0}]}`
	admin.mu.Unlock()

	CollectZhdataStats()

	if err := db.Where("id = ?", client.Id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Up != 300 || stored.Down != 700 {
		t.Fatalf("restart credited %d/%d, want 300/700 unchanged", stored.Up, stored.Down)
	}
}

// The kick reaches every zhdata process with the client's uuid.
func TestKickZhdataUser(t *testing.T) {
	db := zhdataTestDB(t)

	admin := &fakeZhdataAdmin{}
	server := httptest.NewServer(admin)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	seedZhdataInbound(t, db, "zhdata-in", `{"listen":"","listen_ports":[49888],"admin_addr":"`+addr+`"}`)
	seedZhdataClient(t, db, "dave", 0, "dddddddd-0000-4000-8000-000000000004", []uint{})

	KickZhdataUser("dave")

	admin.mu.Lock()
	defer admin.mu.Unlock()
	if len(admin.kicks) != 1 {
		t.Fatalf("expected one kick, got %v", admin.kicks)
	}
	if !strings.Contains(admin.kicks[0], "dddddddd-0000-4000-8000-000000000004") {
		t.Errorf("kick payload = %s, want the client's zhdata uuid", admin.kicks[0])
	}

	// A client without zhdata credentials must not hit the API at all.
	db.Create(&model.Client{Name: "plain", Enable: true, Config: json.RawMessage(`{"trojan":{"name":"plain","password":"x"}}`),
		Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(`[]`)})
	KickZhdataUser("plain")

	admin.mu.Lock()
	defer admin.mu.Unlock()
	if len(admin.kicks) != 1 {
		t.Fatalf("plain client kick reached the API: %v", admin.kicks)
	}
}

// Relay state is pure inbound options pushed to every zhdata process
// (contract §6): upstream list -> enabled, empty list -> disabled.
func TestPushZhdataRelay(t *testing.T) {
	db := zhdataTestDB(t)

	admin := &fakeZhdataAdmin{}
	server := httptest.NewServer(admin)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	seedZhdataInbound(t, db, "zhdata-relay", `{"listen":"","listen_ports":[49888],"admin_addr":"`+addr+`","upstream":["10.0.0.1:49888","10.0.0.2:49889"]}`)
	seedZhdataInbound(t, db, "zhdata-plain", `{"listen":"","listen_ports":[49890],"admin_addr":"`+addr+`"}`)

	PushZhdataRelay(db)

	admin.mu.Lock()
	defer admin.mu.Unlock()
	if len(admin.relays) != 2 {
		t.Fatalf("expected one PUT /v1/relay per zhdata inbound, got %v", admin.relays)
	}

	var on, off struct {
		Upstream []string `json:"upstream"`
		Enabled  *bool    `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(admin.relays[0]), &on); err != nil {
		t.Fatal(err)
	}
	if on.Enabled == nil || !*on.Enabled {
		t.Errorf("relay inbound: enabled = %v, want true", on.Enabled)
	}
	if len(on.Upstream) != 2 || on.Upstream[0] != "10.0.0.1:49888" || on.Upstream[1] != "10.0.0.2:49889" {
		t.Errorf("relay inbound: upstream = %v, want both hosts", on.Upstream)
	}

	if err := json.Unmarshal([]byte(admin.relays[1]), &off); err != nil {
		t.Fatal(err)
	}
	if off.Enabled == nil || *off.Enabled {
		t.Errorf("plain inbound: enabled = %v, want false", off.Enabled)
	}
	if off.Upstream == nil || len(off.Upstream) != 0 {
		t.Errorf("plain inbound: upstream = %v, want an explicit empty list", off.Upstream)
	}
}

// A uuid reported with devices > 0 refreshes its client's online_at (the same
// online marker stats.go sets for core sessions); devices == 0 refreshes
// nothing. A uuid bound to two zhdata inbounds is refreshed exactly once.
func TestCollectZhdataStatsMarksOnline(t *testing.T) {
	db := zhdataTestDB(t)

	admin := &fakeZhdataAdmin{}
	server := httptest.NewServer(admin)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	// Two zhdata inbounds, one admin process shape, both reporting erin.
	seedZhdataInbound(t, db, "zhdata-online-a", `{"listen":"","listen_ports":[49888],"admin_addr":"`+addr+`"}`)
	seedZhdataInbound(t, db, "zhdata-online-b", `{"listen":"","listen_ports":[49889],"admin_addr":"`+addr+`"}`)
	erin := seedZhdataClient(t, db, "erin", 0, "eeeeeeee-0000-4000-8000-000000000005", []uint{})
	frank := seedZhdataClient(t, db, "frank", 0, "ffffffff-0000-4000-8000-000000000006", []uint{})

	ForgetZhdataSnapshot("zhdata-online-a")
	ForgetZhdataSnapshot("zhdata-online-b")

	admin.mu.Lock()
	admin.stats = `{"ts":1,"sessions_active":1,"users":[` +
		`{"uuid":"eeeeeeee-0000-4000-8000-000000000005","devices":1,"up":0,"down":0},` +
		`{"uuid":"ffffffff-0000-4000-8000-000000000006","devices":0,"up":0,"down":0}]}`
	admin.mu.Unlock()

	updates := 0
	if err := db.Callback().Update().Before("gorm:update").
		Register("test:count_updates", func(tx *gorm.DB) { updates++ }); err != nil {
		t.Fatal(err)
	}

	CollectZhdataStats()

	// One client row per uuid at most: erin appears on two inbounds but must
	// be written once; frank (devices == 0, no delta) must not be written.
	if updates != 1 {
		t.Errorf("clients written %d times, want exactly 1 (erin only, once)", updates)
	}

	var stored model.Client
	if err := db.Where("id = ?", erin.Id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.OnlineAt == 0 {
		t.Error("erin online_at not refreshed although devices > 0")
	}
	if err := db.Where("id = ?", frank.Id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	if stored.OnlineAt != 0 {
		t.Errorf("frank online_at = %d, want 0: devices == 0 must not mark online", stored.OnlineAt)
	}

	// The same sample feeds the panel's online cache (onlines.user /
	// onlines.inbound -- what the online badges actually read).
	online := &onlines{}
	mergeZhdataOnline(online)
	if !slices.Contains(online.User, "erin") {
		t.Errorf("online users = %v, want erin listed", online.User)
	}
	if slices.Contains(online.User, "frank") {
		t.Errorf("online users = %v, frank must not be listed", online.User)
	}
	for _, tag := range []string{"zhdata-online-a", "zhdata-online-b"} {
		if !slices.Contains(online.Inbound, tag) {
			t.Errorf("online inbounds = %v, want %s", online.Inbound, tag)
		}
	}

	// onlines.inboundCounts: zhdata tag -> summed uuid devices (erin 1 +
	// frank 0 per process). GetOnlines with an unstarted core carries the
	// zhdata half only.
	if counts := zhdataInboundCounts(); counts["zhdata-online-a"] != 1 || counts["zhdata-online-b"] != 1 {
		t.Errorf("zhdataInboundCounts = %v, want both tags at 1 (devices summed per uuid)", counts)
	}
	res, err := (&StatsService{}).GetOnlines()
	if err != nil {
		t.Fatal(err)
	}
	if res.InboundCounts["zhdata-online-a"] != 1 || res.InboundCounts["zhdata-online-b"] != 1 {
		t.Errorf("GetOnlines inboundCounts = %v, want zhdata tags at 1", res.InboundCounts)
	}

	// A stale overlay (process stopped reporting) marks nobody.
	zhdataOnlineMu.Lock()
	zhdataOnlineSampledAt = time.Now().Unix() - zhdataOnlineStaleness - 1
	zhdataOnlineMu.Unlock()
	online = &onlines{}
	mergeZhdataOnline(online)
	if len(online.User) != 0 || len(online.Inbound) != 0 {
		t.Errorf("stale overlay merged: users=%v inbounds=%v, want empty", online.User, online.Inbound)
	}
	if counts := zhdataInboundCounts(); counts != nil {
		t.Errorf("stale zhdataInboundCounts = %v, want nil", counts)
	}
	res, err = (&StatsService{}).GetOnlines()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := res.InboundCounts["zhdata-online-a"]; ok {
		t.Errorf("stale zhdata tag still in inboundCounts: %v", res.InboundCounts)
	}
}

// The sessions modal for a zhdata inbound reads that process'
// GET /v1/sessions mapped into core.SessionInfo; a user query folds zhdata
// sessions in across every zhdata process (contract: uuid/device/slot/remote/
// started_at/relay).
func TestZhdataSessionsBridge(t *testing.T) {
	db := zhdataTestDB(t)

	admin := &fakeZhdataAdmin{}
	server := httptest.NewServer(admin)
	defer server.Close()
	addr := strings.TrimPrefix(server.URL, "http://")

	seedZhdataInbound(t, db, "zhdata-sess-a", `{"listen":"","listen_ports":[49888],"admin_addr":"`+addr+`"}`)
	seedZhdataInbound(t, db, "zhdata-sess-b", `{"listen":"","listen_ports":[49889],"admin_addr":"`+addr+`"}`)
	vless := &model.Inbound{
		Type: "vless", Tag: "vless-sess",
		Addrs: json.RawMessage(`[]`), OutJson: json.RawMessage(`{}`),
		Options: json.RawMessage(`{"listen":"::","listen_port":2096}`),
	}
	if err := db.Create(vless).Error; err != nil {
		t.Fatal(err)
	}
	seedZhdataClient(t, db, "gina", 0, "gggggggg-0000-4000-8000-000000000007", []uint{})

	admin.mu.Lock()
	admin.sessions = `{"ts":9,"total":4,"sessions":[` +
		`{"uuid":"gggggggg-0000-4000-8000-000000000007","device":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","slot":3,"remote":"1.2.3.4:5555","started_at":1732000000,"relay":false},` +
		`{"uuid":"gggggggg-0000-4000-8000-000000000007","device":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","slot":9,"remote":"5.6.7.8:1","started_at":1732000100,"relay":true},` +
		`{"uuid":"hhhhhhhh-0000-4000-8000-000000000008","device":"cccccccccccccccccccccccccccccccc","slot":0,"remote":"9.9.9.9:2","started_at":1732000200,"relay":false},` +
		`{"uuid":"iiiiiiii-0000-4000-8000-000000000009","device":"","slot":0,"remote":"7.7.7.7:7","started_at":1731999000,"relay":false}]}`
	admin.mu.Unlock()

	s := &StatsService{}

	// --- inbound query on a zhdata tag ---
	list, err := s.GetSessions("inbound", "zhdata-sess-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 4 {
		t.Fatalf("zhdata inbound sessions = %d, want 4: %+v", len(list), list)
	}
	admin.mu.Lock()
	queries := append([]string(nil), admin.queries...)
	admin.mu.Unlock()
	for _, q := range queries {
		if !strings.Contains(q, "limit=100") {
			t.Errorf("sessions query = %q, want limit=100", q)
		}
	}
	var relay *core.SessionInfo
	for i := range list {
		if list[i].Rule == "relay" {
			relay = &list[i]
		}
	}
	if relay == nil {
		t.Fatal("relay flag not carried into the mapping")
	}
	if relay.ID != "zhdata-sess-a:gggggggg-0000-4000-8000-000000000007:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb:9" {
		t.Errorf("id = %q, want tag:uuid:device:slot", relay.ID)
	}
	if relay.Inbound != "zhdata-sess-a" || relay.User != "gina" {
		t.Errorf("inbound/user = %q/%q, want zhdata-sess-a/gina", relay.Inbound, relay.User)
	}
	if relay.Network != "zhdata" {
		t.Errorf("network = %q, want zhdata", relay.Network)
	}
	if relay.Source != "5.6.7.8:1" {
		t.Errorf("source = %q, want the remote address", relay.Source)
	}
	if relay.Destination != "" {
		t.Errorf("destination = %q, want empty (tunnel sessions have none)", relay.Destination)
	}
	// Unit check: createdAt is Unix SECONDS, the unit Sessions.vue's
	// duration() and the tracker's snapshot use (the task note said
	// milliseconds; the core is seconds -- zhdata started_at passes as-is).
	if relay.CreatedAt != 1732000100 {
		t.Errorf("createdAt = %d, want 1732000100 (seconds)", relay.CreatedAt)
	}
	if relay.Upload != 0 || relay.Download != 0 {
		t.Errorf("up/down = %d/%d, want 0/0 (no per-session counters)", relay.Upload, relay.Download)
	}
	// A uuid with no panel client shows the uuid itself.
	found := false
	emptyDevice := false
	for _, session := range list {
		if session.User == "hhhhhhhh-0000-4000-8000-000000000008" {
			found = true
		}
		// Legacy zhv4/5/6 handshakes report an empty device; the uuid in
		// the id keeps such rows unique and renderable.
		if session.ID == "zhdata-sess-a:iiiiiiii-0000-4000-8000-000000000009::0" &&
			session.User == "iiiiiiii-0000-4000-8000-000000000009" {
			emptyDevice = true
		}
	}
	if !found {
		t.Error("unresolved uuid should be displayed as-is")
	}
	if !emptyDevice {
		t.Error("empty-device session missing or id not unique via uuid")
	}

	// --- user query across both zhdata inbounds, merged and filtered ---
	user, err := s.GetSessions("user", "gina")
	if err != nil {
		t.Fatal(err)
	}
	// gina has two sessions per process, two processes; the other uuid is
	// filtered out.
	if len(user) != 4 {
		t.Fatalf("gina sessions = %d, want 4 (2 per inbound): %+v", len(user), user)
	}
	seen := map[string]bool{}
	for _, session := range user {
		if session.User != "gina" {
			t.Errorf("foreign user in gina's list: %+v", session)
		}
		if seen[session.ID] {
			t.Errorf("duplicate session id %q", session.ID)
		}
		seen[session.ID] = true
	}
	for _, tag := range []string{"zhdata-sess-a", "zhdata-sess-b"} {
		count := 0
		for _, session := range user {
			if session.Inbound == tag {
				count++
			}
		}
		if count != 2 {
			t.Errorf("inbound %s contributed %d sessions, want 2", tag, count)
		}
	}

	// --- non-zhdata inbound and unknown resource behave as before ---
	plain, err := s.GetSessions("inbound", "vless-sess")
	if err != nil {
		t.Fatal(err)
	}
	if plain == nil || len(plain) != 0 {
		t.Errorf("vless inbound with an unstarted core = %v, want empty list", plain)
	}
	if _, err := s.GetSessions("bogus", "x"); err == nil {
		t.Error("unknown resource must still error")
	}

	// --- the modal's Close All kicks zhdata even with the core stopped ---
	admin.mu.Lock()
	admin.kicks = nil
	admin.mu.Unlock()

	if err := s.CloseUserSessions("gina"); err == nil {
		t.Error("CloseUserSessions with an unstarted core must report the core error")
	}
	admin.mu.Lock()
	kicks := append([]string(nil), admin.kicks...)
	admin.mu.Unlock()
	if len(kicks) != 2 {
		t.Fatalf("gina kicks = %v, want one per zhdata inbound (2)", kicks)
	}
	for _, body := range kicks {
		if !strings.Contains(body, "gggggggg-0000-4000-8000-000000000007") {
			t.Errorf("kick body = %s, want gina's zhdata uuid", body)
		}
	}

	// A client without zhdata credentials kicks nobody.
	db.Create(&model.Client{Name: "plain", Enable: true, Config: json.RawMessage(`{"trojan":{"name":"plain","password":"x"}}`),
		Inbounds: json.RawMessage(`[]`), Links: json.RawMessage(`[]`)})
	_ = s.CloseUserSessions("plain")
	admin.mu.Lock()
	kicksAfter := len(admin.kicks)
	admin.mu.Unlock()
	if kicksAfter != 2 {
		t.Fatalf("plain client reached the zhdata API: %v", kicksAfter)
	}
}
