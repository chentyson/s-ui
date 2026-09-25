// hy2probe —— 用 sing-quic 的 hysteria2 客户端做真实 TCP-through-hysteria2 吞吐探测。
//
// 目的：验证服务端「按用户带宽限速（bwlimit）」在 hysteria2 入站上是否真的生效。
// 它走的就是 hysteria2 入站 NewConnectionEx（TCP 流）这条路，和真实客户端一致。
//
// 用法（在控制机、s-ui 仓库树内编译：go build -o /tmp/hy2probe ./hy2probe）：
//   /tmp/hy2probe <server_host:port> <sni> <password> <dest_host:port> <path> <秒数>
// 例：
//   /tmp/hy2probe 107.173.159.175:9443 h28.boosoo.cn <pw> speed.cloudflare.com:80 /__down?bytes=500000000 12
//
// 判读：结果 ≈ group 设定的带宽（如 1.5M ≈ 1.5-1.9 Mbps）→ 限速生效；
//       结果跑到链路上限（远高于设定值）→ 限速没生效。
package main

import (
	"context"
	stdtls "crypto/tls"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/sagernet/sing/common/logger"
	M "github.com/sagernet/sing/common/metadata"
	aTLS "github.com/sagernet/sing/common/tls"
	qtls "github.com/sagernet/sing-quic"
	"github.com/sagernet/sing-quic/hysteria2"
)

// ---- 最小 std tls 适配（sing 的 aTLS.Config 是接口，sing-box 里的实现太重） ----

type stdConfig struct {
	cfg     *stdtls.Config
	timeout time.Duration
}

func (c *stdConfig) ServerName() string              { return c.cfg.ServerName }
func (c *stdConfig) SetServerName(s string)          { c.cfg.ServerName = s }
func (c *stdConfig) NextProtos() []string            { return c.cfg.NextProtos }
func (c *stdConfig) SetNextProtos(p []string)        { c.cfg.NextProtos = p }
func (c *stdConfig) HandshakeTimeout() time.Duration { return c.timeout }
func (c *stdConfig) SetHandshakeTimeout(d time.Duration) {
	c.timeout = d
}
func (c *stdConfig) STDConfig() (*stdtls.Config, error) { return c.cfg, nil }
func (c *stdConfig) Client(conn net.Conn) (aTLS.Conn, error) {
	return &stdConn{Conn: stdtls.Client(conn, c.cfg)}, nil
}
func (c *stdConfig) Clone() aTLS.Config {
	return &stdConfig{cfg: c.cfg.Clone(), timeout: c.timeout}
}

type stdConn struct{ *stdtls.Conn }

func (c *stdConn) NetConn() net.Conn                          { return c.Conn }
func (c *stdConn) HandshakeContext(ctx context.Context) error { return c.Conn.HandshakeContext(ctx) }

// ---- 最小 dialer（N.Dialer）----

type stdDialer struct{ d *net.Dialer }

func (s stdDialer) DialContext(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
	return s.d.DialContext(ctx, network, dest.String())
}
func (s stdDialer) ListenPacket(ctx context.Context, dest M.Socksaddr) (net.PacketConn, error) {
	return net.ListenUDP("udp", nil)
}

// ----

func main() {
	if len(os.Args) < 7 {
		fmt.Fprintln(os.Stderr, "用法: hy2probe <server_host:port> <sni> <password> <dest_host:port> <path> <秒数>")
		os.Exit(2)
	}
	server, sni, password, dest, path := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]
	seconds, _ := strconv.Atoi(os.Args[6])

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds+20)*time.Second)
	defer cancel()

	client, err := hysteria2.NewClient(hysteria2.ClientOptions{
		Context:       ctx,
		Dialer:        stdDialer{&net.Dialer{Timeout: 10 * time.Second}},
		Logger:        logger.NOP(),
		ServerAddress: M.ParseSocksaddr(server),
		Password:      password,
		// 0 = 不用 brutal，走 BBR；测的就是服务端限速器，别让客户端 CC 干扰
		SendBPS:    0,
		ReceiveBPS: 0,
		TLSConfig: &stdConfig{cfg: &stdtls.Config{
			ServerName:         sni,
			InsecureSkipVerify: true,
		}},
		QUICOptions: qtls.QUICOptions{
			IdleTimeout:     30 * time.Second,
			KeepAlivePeriod: 10 * time.Second,
		},
		UDPDisabled: true,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "NewClient:", err)
		os.Exit(1)
	}
	defer client.CloseWithError(nil)

	t0 := time.Now()
	conn, err := client.DialConn(ctx, M.ParseSocksaddr(dest))
	if err != nil {
		fmt.Fprintln(os.Stderr, "DialConn:", err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Printf("  已连上 %s（握手 %.2fs），目标 %s%s\n", server, time.Since(t0).Seconds(), dest, path)

	host, _, _ := net.SplitHostPort(dest)
	req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nUser-Agent: hy2probe\r\nConnection: close\r\n\r\n", path, host)
	if _, err = conn.Write([]byte(req)); err != nil {
		fmt.Fprintln(os.Stderr, "写请求失败:", err)
		os.Exit(1)
	}

	deadline := time.Now().Add(time.Duration(seconds) * time.Second)
	buf := make([]byte, 1<<16)
	var total int64
	start := time.Now()
	for time.Now().Before(deadline) {
		conn.SetReadDeadline(deadline.Add(2 * time.Second))
		n, err := conn.Read(buf)
		if n > 0 {
			total += int64(n)
		}
		if err != nil {
			break
		}
	}
	elapsed := time.Since(start).Seconds()
	fmt.Printf("  %d 字节 / %.2fs = %.0f KB/s = %.2f Mbps\n", total, elapsed, float64(total)/elapsed/1024, float64(total)*8/elapsed/1e6)
}
