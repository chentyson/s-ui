package zhdata

import (
	"context"
	"encoding/json"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

// The options JSON of contract §7 decodes with the sing-box conventions:
// ServerOptions for the host half, badoption.Duration for the timeout.
func TestZhdataOutboundOptionsDecode(t *testing.T) {
	raw := `{
		"type": "zhdata",
		"tag": "zhdata-out",
		"server": "zh.example.test",
		"server_port": 49888,
		"uuid": "b831381d-6324-4d53-ad4f-8cda48b30811",
		"conns": 16,
		"spread": 8,
		"connect_timeout": "5s"
	}`

	var options ZhdataOutboundOptions
	if err := json.Unmarshal([]byte(raw), &options); err != nil {
		t.Fatal(err)
	}
	if options.Server != "zh.example.test" || options.ServerPort != 49888 {
		t.Errorf("server = %q:%d, want zh.example.test:49888", options.Server, options.ServerPort)
	}
	if options.UUID != "b831381d-6324-4d53-ad4f-8cda48b30811" {
		t.Errorf("uuid = %q", options.UUID)
	}
	if options.Conns != 16 || options.Spread != 8 {
		t.Errorf("conns/spread = %d/%d, want 16/8", options.Conns, options.Spread)
	}
	if time.Duration(options.ConnectTimeout) != 5*time.Second {
		t.Errorf("connect_timeout = %v, want 5s", time.Duration(options.ConnectTimeout))
	}
}

// A registry lookup by type must find the options type -- that is what makes
// a zhdata outbound entry legal inside the panel config.
func TestZhdataOutboundRegistered(t *testing.T) {
	registry := outbound.NewRegistry()
	RegisterOutbound(registry)
	if !slices.Contains(registry.OptionTypes(), TypeZhdata) {
		t.Fatalf("registry option types: %v", registry.OptionTypes())
	}
	if _, ok := registry.CreateOptions(TypeZhdata); !ok {
		t.Fatal("no options type for zhdata")
	}
}

// Construction validates the mandatory fields; a complete outbound is TCP-only.
func TestZhdataOutboundConstruction(t *testing.T) {
	for name, options := range map[string]ZhdataOutboundOptions{
		"missing server": {ServerOptions: option.ServerOptions{ServerPort: 49888}, UUID: "u"},
		"missing port":   {ServerOptions: zhHostOnly("zh.example.test"), UUID: "u"},
		"missing uuid":   {ServerOptions: zhServer(49888)},
	} {
		if _, err := NewOutbound(context.Background(), nil, logger.NOP(), "zhdata-out", options); err == nil {
			t.Errorf("%s: expected a construction error", name)
		}
	}

	ob, err := NewOutbound(context.Background(), nil, logger.NOP(), "zhdata-out", ZhdataOutboundOptions{
		ServerOptions: zhServer(49888),
		UUID:          "b831381d-6324-4d53-ad4f-8cda48b30811",
	})
	if err != nil {
		t.Fatal(err)
	}
	var _ adapter.Outbound = ob
	if !slices.Contains(ob.Network(), N.NetworkTCP) {
		t.Errorf("network = %v, want tcp", ob.Network())
	}
	if slices.Contains(ob.Network(), N.NetworkUDP) {
		t.Errorf("network = %v, want no udp", ob.Network())
	}

	// UDP asks for a PacketConn the protocol does not have: a clear error,
	// not a panic or a hang.
	if _, err := ob.ListenPacket(context.Background(), M.ParseSocksaddr("example.test:443")); err == nil {
		t.Error("ListenPacket succeeded; zhdata has no packet transport")
	}

	// Close before any dial: the lazy client was never built, and closing
	// the outbound must be safe (and must not build one afterwards). The
	// outbound manager closes outbounds through io.Closer, exactly like
	// this concrete type.
	closer, ok := ob.(io.Closer)
	if !ok {
		t.Fatal("zhdata outbound is not an io.Closer; the manager would never release it")
	}
	if err := closer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ob.DialContext(context.Background(), N.NetworkTCP, M.ParseSocksaddr("example.test:443")); err == nil {
		t.Error("dial on a closed outbound succeeded")
	}
}

func zhServer(port uint16) option.ServerOptions {
	return option.ServerOptions{Server: "zh.example.test", ServerPort: port}
}

func zhHostOnly(host string) option.ServerOptions {
	return option.ServerOptions{Server: host}
}
