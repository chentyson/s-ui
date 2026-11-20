package service

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
	stats    string
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
	case r.Method == http.MethodGet && r.URL.Path == "/v1/stats":
		_, _ = w.Write([]byte(f.stats))
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
