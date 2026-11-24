package service

// The launch parameters of the zhdata process behind a zhdata inbound (contract §2.4).
//
// A zhdata inbound is a row in the panel plus a process outside it. Before this
// file the process was pure operator duty: the panel pushed users and relay
// state through the admin API but never touched how the process was started, so
// changing the port pool in the UI left the process listening on the old ports
// (inbound says 45501-45564, unit says 49888-49919) and clients could not
// connect while the inbound card still looked healthy -- the chip only probes
// admin_addr.
//
// The panel now owns that unit: it renders it from the inbound options, creates
// and enables it when missing, and restarts the service when -- and only when --
// the launch line actually changed. Anything else (users, relay, caps) keeps
// flowing through the admin API without dropping tunnels.
//
// Scope is deliberately narrow: one unit, /etc/systemd/system/zhdata.service,
// for the inbound whose admin_addr is the default one. A second zhdata inbound
// pointing at a second process (C498 runs a relay on 127.0.0.1:6071) stays
// operator-managed, because the panel has no way to know the name or the
// layout of that second service.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/alireza0/s-ui/config"
	"github.com/alireza0/s-ui/database/model"
	"github.com/alireza0/s-ui/logger"
	"github.com/alireza0/s-ui/util"
	"gorm.io/gorm"
)

// Paths the operator owns. They are variables, not constants, so tests can
// point them at a temp tree and so a packager can change them in one place.
var (
	zhdataUnitPath   = "/etc/systemd/system/zhdata.service"
	zhdataUnitDropin = "/etc/systemd/system/zhdata.service.d"
	zhdataBinaryPath = "/opt/zhdata/zhdata-tunnel-linux-amd64"
	zhdataUnitName   = "zhdata.service"
)

// zhdataDefaultAdminAddr is the admin endpoint the panel manages. An inbound on
// any other address is a process the operator started themselves.
const zhdataDefaultAdminAddr = "127.0.0.1:6070"

// zhdataManagedInbound picks the inbound whose process the unit describes.
// The default admin_addr is the marker, so a panel with a second, separately
// managed process (C498) keeps working untouched. An empty admin_addr is not
// managed either: the panel cannot probe such an inbound, let alone restart it.
func zhdataManagedInbound(inbounds []*model.Inbound) (*model.Inbound, *zhdataOptions) {
	for _, inbound := range inbounds {
		opts, err := parseZhdataOptions(inbound.Options)
		if err != nil {
			continue
		}
		if opts.AdminAddr == zhdataDefaultAdminAddr {
			return inbound, opts
		}
	}
	return nil, nil
}

// zhdataUnitExecStart renders the whole ExecStart line from the inbound options.
// The order is fixed: comparing two renderings is then the same as comparing the
// launch parameters, which is what decides whether a restart is warranted.
func zhdataUnitExecStart(tag string, opts *zhdataOptions, dbPath string) string {
	args := []string{
		zhdataBinaryPath,
		"-mode", "server",
		"-admin", orDefault(opts.AdminAddr, zhdataDefaultAdminAddr),
		"-users", dbPath,
	}
	// An inbound with no port pool has nothing to bind yet: leave -listen out
	// entirely and let the core apply its own default, instead of rendering
	// "-listen" with an empty argument.
	if listen := zhdataListenFromOptions(opts); listen != "" {
		args = append(args, "-listen", listen)
	}
	if len(opts.Upstream) > 0 {
		// Non-empty upstream list = transparent relay (contract §6).
		args = append(args, "-backend", "zhdata", "-upstream", strings.Join(opts.Upstream, ","))
	} else {
		args = append(args, "-backend", orDefault(opts.Backend, "direct"))
	}
	if opts.Decoy != "" {
		args = append(args, "-decoy", opts.Decoy)
	}
	if opts.AdminToken != "" {
		args = append(args, "-admin-token", opts.AdminToken)
	}
	// max_sessions 0 means unlimited in the core; spell it out so the unit reads
	// as an explicit decision. -max-clients is its compat alias.
	sessions := fmt.Sprintf("%d", opts.MaxSessions)
	args = append(args, "-loglevel", "info", "-max-sessions", sessions, "-max-clients", sessions)
	return strings.Join(args, " ")
}

// zhdataListenFromOptions turns the inbound's port pool into the core's
// "host:port,host:port" listen form. It re-runs the same marshalling the link
// builder uses (util.BuildZhdataListen) so the port a client is handed and the
// port the process binds can never drift apart again.
func zhdataListenFromOptions(opts *zhdataOptions) string {
	encoded, err := json.Marshal(opts)
	if err != nil {
		return ""
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(encoded, &raw); err != nil {
		return ""
	}
	return util.BuildZhdataListen(raw)
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

// zhdataUnitText is the whole unit file. The Description carries the tag so an
// operator reading systemctl status sees which inbound it belongs to.
func zhdataUnitText(tag string, opts *zhdataOptions, dbPath string) string {
	execStart := zhdataUnitExecStart(tag, opts, dbPath)
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=zhdata-tunnel server (%s)\n", orDefault(tag, "panel-managed")))
	b.WriteString("After=network.target\n\n")
	b.WriteString("[Service]\n")
	b.WriteString("WorkingDirectory=" + filepath.Dir(zhdataBinaryPath) + "\n")
	b.WriteString("ExecStart=" + execStart + "\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=2\n\n")
	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=multi-user.target\n")
	return b.String()
}

// execStartOf pulls the effective ExecStart out of unit text. A drop-in that
// clears the line (`ExecStart=` alone) before setting its own is what C498 uses,
// so the last non-empty one wins -- the same rule systemd applies.
func execStartOf(unitText string) string {
	last := ""
	for _, line := range strings.Split(unitText, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "ExecStart=") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "ExecStart="))
		if value == "" {
			continue
		}
		last = value
	}
	return last
}

// zhdataDropinOverrides reports a drop-in under zhdataUnitDropin that sets
// ExecStart. Such a file silently wins over the unit the panel writes, so the
// panel has to say so instead of believing its own write.
func zhdataDropinOverrides() string {
	matches, err := filepath.Glob(filepath.Join(zhdataUnitDropin, "*.conf"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		if execStartOf(string(data)) != "" {
			return path
		}
	}
	return ""
}

// zhdataUnitWarnings carries the last unit-sync problem per inbound tag so the
// inbound card can show it. Without this the panel would have written a unit,
// watched a drop-in override it, and stayed silent -- exactly the failure mode
// this file exists to remove.
var (
	zhdataUnitMu       sync.Mutex
	zhdataUnitWarnings = map[string]string{}
)

func rememberZhdataUnitWarning(tag, warning string) {
	if tag == "" {
		return
	}
	zhdataUnitMu.Lock()
	defer zhdataUnitMu.Unlock()
	if warning == "" {
		delete(zhdataUnitWarnings, tag)
		return
	}
	zhdataUnitWarnings[tag] = warning
}

func zhdataUnitWarning(tag string) string {
	zhdataUnitMu.Lock()
	defer zhdataUnitMu.Unlock()
	return zhdataUnitWarnings[tag]
}

// SyncZhdataService aligns the unit with the managed zhdata inbound after a
// config save. It never fails the save: an unreachable systemd, a missing
// binary or a container without systemd all end in a warning.
func SyncZhdataService(db *gorm.DB) {
	tag, warning, err := syncZhdataService(db)
	if err != nil {
		logger.Warning("zhdata unit: ", err)
	}
	rememberZhdataUnitWarning(tag, warning)
}

func syncZhdataService(db *gorm.DB) (tag string, warning string, err error) {
	inbounds, err := zhdataInbounds(db)
	if err != nil {
		return "", "", err
	}
	if len(inbounds) == 0 {
		// No zhdata inbound: nothing to align. A leftover unit is left alone;
		// deleting services behind an operator's back is worse than a stray one.
		return "", "", nil
	}
	inbound, opts := zhdataManagedInbound(inbounds)
	if inbound == nil {
		logger.Info("zhdata unit: 没有 admin_addr=", zhdataDefaultAdminAddr,
			" 的 zhdata 入站，面板不托管任何 unit（现有入站的进程由运维自行管理）")
		return "", "", nil
	}
	if len(inbounds) > 1 {
		logger.Info("zhdata unit: 面板只托管 ", inbound.Tag, " 的 unit；其余 zhdata 入站指向各自的进程，由运维管理")
	}
	tag = inbound.Tag

	if _, statErr := os.Stat(zhdataBinaryPath); statErr != nil {
		return tag, fmt.Sprintf("找不到核心二进制 %s：入站参数已保存，但没有生成/更新 systemd 单元（先装核心再保存一次）", zhdataBinaryPath), nil
	}
	if runtime.GOOS != "linux" {
		return tag, "", nil
	}
	if _, lookErr := exec.LookPath("systemctl"); lookErr != nil {
		return tag, "系统上没有 systemctl（容器或非 systemd 发行版？）：入站参数已保存，但启动参数没有同步", nil
	}

	desired := zhdataUnitText(inbound.Tag, opts, config.GetDBPath())
	created := false
	current, readErr := os.ReadFile(zhdataUnitPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return tag, "", fmt.Errorf("read %s: %w", zhdataUnitPath, readErr)
	}
	if errors.Is(readErr, os.ErrNotExist) {
		created = true
		// A process answering on admin_addr without a unit at our path means the
		// operator started it some other way (another service name, a manual run,
		// a container). Writing ours anyway would bind the same ports twice and
		// Restart=always would turn that into a crash loop, so say it instead.
		if response, probeErr := zhdataAdminRequest(opts, http.MethodGet, "/v1/version", nil); probeErr == nil && len(response) > 0 {
			return tag, fmt.Sprintf("admin_addr %s 上已有 zhdata 在跑，但 %s 不存在：面板没有生成 unit，以免和现有进程抢端口。要让面板托管，请把那个服务改名为 %s，或先停掉它再保存一次", opts.AdminAddr, zhdataUnitPath, zhdataUnitName), nil
		}
	} else if string(current) == desired {
		// Nothing in the launch line moved: no daemon-reload, no restart, no
		// dropped tunnels.
		if dropin := zhdataDropinOverrides(); dropin != "" {
			return tag, fmt.Sprintf("%s 里有 ExecStart 覆盖（%s）：面板写的启动参数不生效，请把那一行搬进入站或删掉覆盖", zhdataUnitDropin, dropin), nil
		}
		return tag, "", nil
	}

	if writeErr := os.WriteFile(zhdataUnitPath, []byte(desired), 0o644); writeErr != nil {
		return tag, "", fmt.Errorf("write %s: %w", zhdataUnitPath, writeErr)
	}
	if out, runErr := runSystemd("daemon-reload"); runErr != nil {
		return tag, "", fmt.Errorf("systemctl daemon-reload: %v (%s)", runErr, strings.TrimSpace(out))
	}
	// The view is what -users reads; creating it here (idempotent, the core's
	// own sqlite engine) means a freshly started process already knows the users
	// instead of running uncapped until the panel's next push.
	zhdataRunInitView()

	if created {
		// enable so the node also comes back after a reboot; the restart below
		// starts it now.
		if out, runErr := runSystemd("enable", zhdataUnitName); runErr != nil {
			logger.Warning("zhdata unit: systemctl enable ", zhdataUnitName, ": ", runErr, " ", strings.TrimSpace(out))
		}
	}
	if out, runErr := runSystemd("restart", zhdataUnitName); runErr != nil {
		return tag, "", fmt.Errorf("systemctl restart %s: %v (%s)", zhdataUnitName, runErr, strings.TrimSpace(out))
	}
	if dropin := zhdataDropinOverrides(); dropin != "" {
		return tag, fmt.Sprintf("已按入站 %s 更新并重启 %s，但 %s 里有 ExecStart 覆盖（%s）：实际启动参数可能仍是旧的", inbound.Tag, zhdataUnitName, zhdataUnitDropin, dropin), nil
	}
	if created {
		logger.Info("zhdata unit: 已生成并启用 ", zhdataUnitPath, "（入站 ", inbound.Tag, "）")
	} else {
		logger.Info("zhdata unit: 启动参数有变化，已更新并重启 ", zhdataUnitName, "（入站 ", inbound.Tag, "）")
	}
	return tag, "", nil
}

func runSystemd(args ...string) (string, error) {
	cmd := exec.Command("systemctl", args...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// zhdataRunInitView asks the core to make sure the user view exists. Best
// effort: an older core without -init-view just errors and the panel keeps
// working on its admin-API push alone.
func zhdataRunInitView() {
	out, err := exec.Command(zhdataBinaryPath, "-init-view", config.GetDBPath()).CombinedOutput()
	if err != nil {
		logger.Warning("zhdata unit: -init-view: ", err, " ", strings.TrimSpace(string(out)), " (zhdata < 0.15.7 没有该参数，用户仍靠面板推送)")
		return
	}
	logger.Info("zhdata unit: ", strings.TrimSpace(string(out)))
}
