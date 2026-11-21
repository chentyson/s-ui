package zhdata

// zhdata outbound for the embedded sing-box box (docs/zhdata-admin-api.md §7).
//
// The panel already runs zhdata *inbounds* as external processes (§2). This
// outbound is the other direction: traffic from any embedded inbound
// (vless/hysteria2/...) leaves through a zhdata tunnel. It is built on the
// zhdata client library (module github.com/chentyson/zhdata, v0.15.2+):
// a Client dials its zhdata server(s) and returns plain net.Conn tunnels.
//
// The outbound authenticates with the uuid written in its own options -- not
// with a panel client's credentials -- and carries a random per-process
// device_id (DeviceID nil => the library generates one), exactly like a
// standalone zhdata client app on this machine.

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	zhdatacore "github.com/chentyson/zhdata"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/adapter/outbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	E "github.com/sagernet/sing/common/exceptions"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/common/json/badoption"
)

// TypeZhdata is the outbound type name; sing-box's own constant package has
// no zhdata type, so s-ui owns this string.
const TypeZhdata = "zhdata"

// ZhdataOutboundOptions are the JSON fields of a zhdata outbound
// (contract §7): {"type":"zhdata","tag":...,"server":...,"server_port":...,
// "uuid":...,"conns":...,"spread":...,"connect_timeout":...}. The server
// half follows the sing-box ServerOptions convention; there is no
// server_ports list -- a single host:port is what the contract defines.
type ZhdataOutboundOptions struct {
	option.ServerOptions
	UUID           string             `json:"uuid"`
	Conns          int                `json:"conns,omitempty"`
	Spread         int                `json:"spread,omitempty"`
	ConnectTimeout badoption.Duration `json:"connect_timeout,omitempty"`
}

func RegisterOutbound(registry *outbound.Registry) {
	outbound.Register[ZhdataOutboundOptions](registry, TypeZhdata, NewOutbound)
}

var _ adapter.Outbound = (*Outbound)(nil)

type Outbound struct {
	outbound.Adapter
	logger  log.ContextLogger
	options ZhdataOutboundOptions

	// The zhdata client is built on the first dial, not at box start: a
	// configured-but-unused outbound must not hold lanes open.
	access sync.Mutex
	client *zhdatacore.Client
	closed bool
}

func NewOutbound(ctx context.Context, router adapter.Router, logger log.ContextLogger, tag string, options ZhdataOutboundOptions) (adapter.Outbound, error) {
	if options.Server == "" {
		return nil, E.New("zhdata outbound: server is required")
	}
	if options.ServerPort == 0 {
		return nil, E.New("zhdata outbound: server_port is required")
	}
	if options.UUID == "" {
		return nil, E.New("zhdata outbound: uuid is required")
	}
	return &Outbound{
		// zhdata carries TCP tunnels only: no PacketConn in the protocol,
		// so UDP is not offered rather than failing later.
		Adapter: outbound.NewAdapter(TypeZhdata, tag, []string{N.NetworkTCP}, nil),
		logger:  logger,
		options: options,
	}, nil
}

// client lazily constructs the one zhdata Client this outbound shares; the
// library multiplexes lanes (conns) and spreads destinations over them
// itself. DeviceID stays nil so the library picks a random one per process:
// the outbound is a single device on this machine, and the zhdata server
// counts it against the uuid's device limit like any other client.
func (h *Outbound) ensureClient() (*zhdatacore.Client, error) {
	h.access.Lock()
	defer h.access.Unlock()
	if h.closed {
		return nil, E.New("zhdata outbound: outbound is closed")
	}
	if h.client == nil {
		client, err := zhdatacore.NewClient(&zhdatacore.ClientConfig{
			Servers: []string{net.JoinHostPort(h.options.Server, strconv.Itoa(int(h.options.ServerPort)))},
			UUID:    h.options.UUID,
			Conns:   h.options.Conns,
			Spread:  h.options.Spread,
			// Logs from the tunnel land in the panel log, at debug: the
			// handshake retries of an unreachable server would otherwise
			// spam the panel.
			Logf: func(format string, v ...any) {
				h.logger.Debug(fmt.Sprintf(format, v...))
			},
		})
		if err != nil {
			return nil, err
		}
		h.client = client
	}
	return h.client, nil
}

func (h *Outbound) DialContext(ctx context.Context, network string, destination M.Socksaddr) (net.Conn, error) {
	if network != N.NetworkTCP {
		return nil, E.New("zhdata outbound: udp not supported")
	}
	client, err := h.ensureClient()
	if err != nil {
		return nil, err
	}
	if timeout := time.Duration(h.options.ConnectTimeout); timeout > 0 {
		if _, immediate := ctx.Deadline(); !immediate {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, timeout)
			defer cancel()
		}
	}
	h.logger.InfoContext(ctx, "outbound connection to ", destination)
	// The tunnel resolves the target server-side, so the destination string
	// goes over as-is (domain or IP); ctx bounds only the dial wait.
	return client.DialContext(ctx, destination.AddrString(), int(destination.Port))
}

func (h *Outbound) ListenPacket(ctx context.Context, destination M.Socksaddr) (net.PacketConn, error) {
	return nil, E.New("zhdata outbound: udp not supported")
}

// Close releases the shared zhdata client: every lane, every tunnel.
func (h *Outbound) Close() error {
	h.access.Lock()
	defer h.access.Unlock()
	h.closed = true
	if h.client != nil {
		h.client.Close()
		h.client = nil
	}
	return nil
}
