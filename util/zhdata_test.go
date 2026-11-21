package util

import (
	"encoding/json"
	"net/url"
	"testing"

	"github.com/alireza0/s-ui/database/model"
)

func zhdataInbound(t *testing.T, options map[string]interface{}) *model.Inbound {
	t.Helper()
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	return &model.Inbound{
		Type:    "zhdata",
		Tag:     "zhdata-in",
		Options: json.RawMessage(raw),
		Addrs:   json.RawMessage(`[]`),
	}
}

// BuildZhdataListen assembles the zhdata server listen semantics from the
// fields the frontend stores: listen_ports plus a listen host part.
func TestBuildZhdataListen(t *testing.T) {
	all := map[string]interface{}{
		"listen":       "",
		"listen_ports": []interface{}{49888.0, 49889.0, 49951.0},
	}
	if got := BuildZhdataListen(all); got != ":49888,:49889,:49951" {
		t.Errorf("all interfaces: got %q, want %q", got, ":49888,:49889,:49951")
	}

	bound := map[string]interface{}{
		"listen":       "127.0.0.1",
		"listen_ports": []interface{}{49888.0},
	}
	if got := BuildZhdataListen(bound); got != "127.0.0.1:49888" {
		t.Errorf("bound host: got %q, want %q", got, "127.0.0.1:49888")
	}

	empty := map[string]interface{}{"listen": ""}
	if got := BuildZhdataListen(empty); got != "" {
		t.Errorf("no ports: got %q, want empty", got)
	}
}

// A zhdata share link carries the uuid, the inbound's first port, and the
// remark (契约 §3.2). The rest of the pool stays server-side: the client dials
// one endpoint and spreads its own lanes over it.
func TestLinkGeneratorZhdata(t *testing.T) {
	inbound := zhdataInbound(t, map[string]interface{}{
		"listen":       "",
		"listen_ports": []interface{}{49888.0, 49889.0, 49951.0},
		"backend":      "direct",
	})
	config := clientConfigFor(t, "zhdata", map[string]interface{}{
		"name": "someone",
		"uuid": "b831381d-6324-4d53-ad4f-8cda48b30811",
	})

	links := LinkGenerator(config, inbound, "example.test", "someone")
	if len(links) != 1 {
		t.Fatalf("expected 1 zhdata link, got %d: %v", len(links), links)
	}

	link := links[0]
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("link does not parse: %q: %v", link, err)
	}
	if parsed.Scheme != "zhdata" {
		t.Errorf("scheme = %q, want zhdata", parsed.Scheme)
	}
	if parsed.User.Username() != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Errorf("uuid = %q, want the client's zhdata uuid", parsed.User.Username())
	}
	if parsed.Host != "example.test:49888" {
		t.Errorf("host = %q, want the pool's first port only", parsed.Host)
	}
	if parsed.Fragment != "someone-zhdata-in" {
		t.Errorf("remark = %q, want someone-zhdata-in", parsed.Fragment)
	}
}

// A client without zhdata credentials yields no link, not a half one.
func TestLinkGeneratorZhdataWithoutCredentials(t *testing.T) {
	inbound := zhdataInbound(t, map[string]interface{}{
		"listen_ports": []interface{}{49888.0},
	})
	links := LinkGenerator(json.RawMessage(`{}`), inbound, "example.test", "someone")
	if len(links) != 0 {
		t.Fatalf("expected no links, got %v", links)
	}
}

// FillOutJson for zhdata: the port pool and the zhdata server fields ride
// along; server_port points at the pool's head for anything that insists on
// one port.
func TestFillOutJsonZhdata(t *testing.T) {
	inbound := zhdataInbound(t, map[string]interface{}{
		"listen":       "",
		"listen_ports": []interface{}{49888.0, 49889.0},
		"backend":      "direct",
		"decoy":        "127.0.0.1:8080",
		"max_sessions": 4800.0,
	})
	inbound.OutJson = json.RawMessage(`{}`)

	if err := FillOutJson(inbound, "1.2.3.4"); err != nil {
		t.Fatal(err)
	}

	var out map[string]interface{}
	if err := json.Unmarshal(inbound.OutJson, &out); err != nil {
		t.Fatal(err)
	}
	if out["type"] != "zhdata" || out["tag"] != "zhdata-in" {
		t.Errorf("type/tag = %v/%v, want zhdata/zhdata-in", out["type"], out["tag"])
	}
	if out["server"] != "1.2.3.4" {
		t.Errorf("server = %v, want 1.2.3.4", out["server"])
	}
	if out["listen"] != ":49888,:49889" {
		t.Errorf("listen = %v, want the assembled server listen", out["listen"])
	}
	if port, _ := out["server_port"].(float64); port != 49888 {
		t.Errorf("server_port = %v, want the pool's first port", out["server_port"])
	}
	if out["backend"] != "direct" {
		t.Errorf("backend = %v, want direct", out["backend"])
	}
	if out["decoy"] != "127.0.0.1:8080" {
		t.Errorf("decoy = %v, want it carried through", out["decoy"])
	}
	if maxSessions, _ := out["max_sessions"].(float64); maxSessions != 4800 {
		t.Errorf("max_sessions = %v, want 4800", out["max_sessions"])
	}
	ports, ok := out["listen_ports"].([]interface{})
	if !ok || len(ports) != 2 {
		t.Errorf("listen_ports = %v, want both pool ports", out["listen_ports"])
	}
}
