package migration

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/alireza0/s-ui/database/model"

	"gorm.io/gorm"
)

func openZhdataLinkTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db := openTestDB(t)
	if err := db.AutoMigrate(&model.Client{}, &model.Inbound{}); err != nil {
		t.Fatal(err)
	}
	return db
}

// zhdataPool renders the port pool the frontend stores in listen_ports, e.g.
// [49888,49889,...] for a 64-lane inbound.
func zhdataPool(count int) string {
	ports := make([]string, 0, count)
	for i := 0; i < count; i++ {
		ports = append(ports, fmt.Sprintf("%d", 49888+i))
	}
	return "[" + strings.Join(ports, ",") + "]"
}

// zhdataPoolURI is the ZY5-era shape: every port of the pool in the host part.
func zhdataPoolURI(uuid, host string, count int) string {
	endpoints := make([]string, 0, count)
	for i := 0; i < count; i++ {
		endpoints = append(endpoints, fmt.Sprintf("%s:%d", host, 49888+i))
	}
	return fmt.Sprintf("zhdata://%s@%s#C498-ZH", uuid, strings.Join(endpoints, ","))
}

func createZhdataInbound(t *testing.T, db *gorm.DB, tag, addrs string, portCount int) *model.Inbound {
	t.Helper()
	inbound := &model.Inbound{
		Type:  "zhdata",
		Tag:   tag,
		Addrs: json.RawMessage(addrs),
		Options: json.RawMessage(
			`{"listen":"","listen_ports":` + zhdataPool(portCount) +
				`,"backend":"direct","admin_addr":"127.0.0.1:6070"}`),
	}
	if err := db.Create(inbound).Error; err != nil {
		t.Fatal(err)
	}
	return inbound
}

const oneAddr = `[{"server":"c498.example","server_port":0,"remark":""}]`

func setClientLinks(t *testing.T, db *gorm.DB, id uint, links string) {
	t.Helper()
	if err := db.Exec("UPDATE clients SET links = ? WHERE id = ?", []byte(links), id).Error; err != nil {
		t.Fatal(err)
	}
}

func clientLinks(t *testing.T, db *gorm.DB, id uint) []map[string]string {
	t.Helper()
	var stored model.Client
	if err := db.Where("id = ?", id).First(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var entries []map[string]string
	if err := json.Unmarshal(stored.Links, &entries); err != nil {
		t.Fatalf("stored links are not a list of objects: %v", err)
	}
	return entries
}

func uriOf(entries []map[string]string, remark string) string {
	for _, entry := range entries {
		if entry["remark"] == remark {
			return entry["uri"]
		}
	}
	return ""
}

const zhdataUUID = "00000000-0000-4000-8000-000000000001"

// A zhdata link generated before ZY6 named the entire port pool, because that
// is what the old builder emitted; the column is only rewritten on save, so an
// upgraded panel keeps showing the bloated URI in the modal and the QR code.
func TestRefreshZhdataLinksCollapsesPortPool(t *testing.T) {
	db := openZhdataLinkTestDB(t)
	createZhdataInbound(t, db, "zhdata-in", oneAddr, 64)

	alice := createTestClient(t, db, "alice",
		`{"zhdata":{"name":"alice","uuid":"`+zhdataUUID+`"}}`)
	setClientLinks(t, db, alice.Id, `[{"remark":"zhdata-in","type":"local","uri":"`+
		zhdataPoolURI(zhdataUUID, "c498.example", 64)+`"}]`)

	refreshed, err := refreshZhdataLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 1 {
		t.Fatalf("refreshed %d client(s), want 1", refreshed)
	}

	want := "zhdata://" + zhdataUUID + "@c498.example:49888#alice-zhdata-in"
	got := uriOf(clientLinks(t, db, alice.Id), "zhdata-in")
	if got != want {
		t.Errorf("uri = %q, want the pool's first port only: %q", got, want)
	}
	if n := strings.Count(got, "c498.example:"); n != 1 {
		t.Errorf("uri %q names %d endpoints, want 1", got, n)
	}
}

// The zhdata inbounds an operator actually creates carry no addrs of their own
// (the panel filled the address from the request's Host header when the link was
// generated), and a migration has no request to read. The host therefore comes
// back out of the stored URI -- every endpoint in it names the same machine.
func TestRefreshZhdataLinksRecoversHostFromStoredURI(t *testing.T) {
	db := openZhdataLinkTestDB(t)
	createZhdataInbound(t, db, "zhdata-in", `null`, 64)

	alice := createTestClient(t, db, "alice",
		`{"zhdata":{"name":"alice","uuid":"`+zhdataUUID+`"}}`)
	setClientLinks(t, db, alice.Id, `[{"remark":"zhdata-in","type":"local","uri":"`+
		zhdataPoolURI(zhdataUUID, "c498.uwintech.com", 64)+`"}]`)

	refreshed, err := refreshZhdataLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 1 {
		t.Fatalf("refreshed %d client(s), want 1", refreshed)
	}

	want := "zhdata://" + zhdataUUID + "@c498.uwintech.com:49888#alice-zhdata-in"
	if got := uriOf(clientLinks(t, db, alice.Id), "zhdata-in"); got != want {
		t.Errorf("uri = %q, want %q", got, want)
	}
}

// Endpoints on more than one machine cannot be rebuilt from a single inbound's
// port pool, so such a link is left exactly as the operator made it.
func TestRefreshZhdataLinksDeclinesSeveralHosts(t *testing.T) {
	db := openZhdataLinkTestDB(t)
	createZhdataInbound(t, db, "zhdata-in", `null`, 8)

	alice := createTestClient(t, db, "alice",
		`{"zhdata":{"name":"alice","uuid":"`+zhdataUUID+`"}}`)
	stale := "zhdata://" + zhdataUUID + "@a.example:49888,b.example:49889#C498-ZH"
	setClientLinks(t, db, alice.Id, `[{"remark":"zhdata-in","type":"local","uri":"`+stale+`"}]`)

	refreshed, err := refreshZhdataLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 0 {
		t.Errorf("refreshed %d client(s), want 0", refreshed)
	}
	if got := uriOf(clientLinks(t, db, alice.Id), "zhdata-in"); got != stale {
		t.Errorf("uri = %q, want it untouched", got)
	}
}

// `sui migrate` runs on upgrade and again after a database restore, and this
// fork's version row (1.6.3-ZYn) sorts below 1.6.4/1.6.5, so the chain replays:
// a link already in the current form must stay untouched and nothing may be
// reported as changed.
func TestRefreshZhdataLinksIsIdempotent(t *testing.T) {
	db := openZhdataLinkTestDB(t)
	createZhdataInbound(t, db, "zhdata-in", oneAddr, 64)

	alice := createTestClient(t, db, "alice",
		`{"zhdata":{"name":"alice","uuid":"`+zhdataUUID+`"}}`)
	setClientLinks(t, db, alice.Id, `[{"remark":"zhdata-in","type":"local","uri":"`+
		zhdataPoolURI(zhdataUUID, "c498.example", 64)+`"}]`)

	if _, err := refreshZhdataLinks(db); err != nil {
		t.Fatal(err)
	}
	first := uriOf(clientLinks(t, db, alice.Id), "zhdata-in")

	refreshed, err := refreshZhdataLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 0 {
		t.Errorf("second pass refreshed %d client(s), want 0", refreshed)
	}
	if again := uriOf(clientLinks(t, db, alice.Id), "zhdata-in"); again != first {
		t.Errorf("uri changed on replay: %q -> %q", first, again)
	}
}

// The panel links a local entry to its inbound by tag; an entry whose inbound is
// gone cannot be regenerated and must keep the value it has.
func TestRefreshZhdataLinksLeavesUnknownInbound(t *testing.T) {
	db := openZhdataLinkTestDB(t)
	createZhdataInbound(t, db, "zhdata-in", oneAddr, 8)

	alice := createTestClient(t, db, "alice",
		`{"zhdata":{"name":"alice","uuid":"`+zhdataUUID+`"}}`)
	stale := zhdataPoolURI(zhdataUUID, "c498.example", 8)
	setClientLinks(t, db, alice.Id, `[{"remark":"deleted-in","type":"local","uri":"`+stale+`"}]`)

	refreshed, err := refreshZhdataLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 0 {
		t.Errorf("refreshed %d client(s), want 0", refreshed)
	}
	if got := uriOf(clientLinks(t, db, alice.Id), "deleted-in"); got != stale {
		t.Errorf("uri = %q, want it untouched", got)
	}
}

// Only zhdata entries are touched. A vless link the operator is happy with, and
// an external subscription link, keep their exact bytes -- as does a zhdata
// inbound that names several addresses, where the stored entry could not be
// matched to one of them.
func TestRefreshZhdataLinksKeepsOtherLinks(t *testing.T) {
	db := openZhdataLinkTestDB(t)
	createZhdataInbound(t, db, "zhdata-in", oneAddr, 8)
	createZhdataInbound(t, db, "zhdata-many",
		`[{"server":"a.example","server_port":0,"remark":"a"},{"server":"b.example","server_port":0,"remark":"b"}]`, 8)

	alice := createTestClient(t, db, "alice",
		`{"zhdata":{"name":"alice","uuid":"`+zhdataUUID+`"},"vless":{"name":"alice","uuid":"`+zhdataUUID+`"}}`)
	entries := []map[string]string{
		{"remark": "vless-in", "type": "local", "uri": "vless://" + zhdataUUID + "@v.example:443#keepme"},
		{"remark": "sub", "type": "sub", "uri": "https://c498.example:62444/sub/token"},
		{"remark": "zhdata-many", "type": "local", "uri": zhdataPoolURI(zhdataUUID, "a.example", 8)},
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	setClientLinks(t, db, alice.Id, string(encoded))

	refreshed, err := refreshZhdataLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 0 {
		t.Errorf("refreshed %d client(s), want 0", refreshed)
	}

	got := clientLinks(t, db, alice.Id)
	for _, entry := range entries {
		if actual := uriOf(got, entry["remark"]); actual != entry["uri"] {
			t.Errorf("link for %q changed: %q -> %q", entry["remark"], entry["uri"], actual)
		}
	}
}

// A client without zhdata credentials gets no zhdata link invented for it.
func TestRefreshZhdataLinksSkipsClientWithoutCredentials(t *testing.T) {
	db := openZhdataLinkTestDB(t)
	createZhdataInbound(t, db, "zhdata-in", oneAddr, 8)

	alice := createTestClient(t, db, "alice", `{"trojan":{"name":"alice","password":"hunter2"}}`)
	stale := zhdataPoolURI(zhdataUUID, "c498.example", 8)
	setClientLinks(t, db, alice.Id, `[{"remark":"zhdata-in","type":"local","uri":"`+stale+`"}]`)

	refreshed, err := refreshZhdataLinks(db)
	if err != nil {
		t.Fatal(err)
	}
	if refreshed != 0 {
		t.Errorf("refreshed %d client(s), want 0", refreshed)
	}
	if got := uriOf(clientLinks(t, db, alice.Id), "zhdata-in"); got != stale {
		t.Errorf("uri = %q, want it untouched", got)
	}
}
