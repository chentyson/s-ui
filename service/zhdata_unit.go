package service

// The launch parameters of the zhdata process behind every zhdata inbound
// (contract §2.4).
//
// A zhdata inbound is a row in the panel plus a process outside it. Before this
// file the process was pure operator duty: the panel pushed users and relay
// state through the admin API but never touched how the process was started, so
// changing the port pool in the UI left the process listening on the old ports
// (inbound says 45501-45564, unit says 49888-49919) and clients could not
// connect while the inbound card still looked healthy -- the chip only probes
// admin_addr.
//
// The panel owns that unit now: it renders it from the inbound options, creates
// and enables it when missing, and restarts the service when -- and only when --
// the launch line actually changed. Anything else (users, relay, caps) keeps
// flowing through the admin API without dropping tunnels.
//
// ZY10 could only do that for one inbound: a single /etc/systemd/system/
// zhdata.service, for the row whose admin_addr was the default one, pointing at
// a binary only the operator knew the path of. ZY11 widens both ends:
//
//   - one unit per manageable zhdata inbound. The row on the default admin_addr
//     keeps zhdata.service so a ZY10 node is untouched; the rest get
//     zhdata-<sanitized-tag>.service, deduplicated so two tags cannot collide.
//   - the binary is the panel's own (service/zhdata_core.go): <s-ui dir>/zhdata/
//     zhdata(.exe). A node that still has the ZY10-era /opt/zhdata install keeps
//     launching it -- flagged external so the UI can suggest the migration.
//
// Deleting an inbound takes its unit with it (stop + disable + remove), and a
// unit the panel wrote that no inbound maps to any more is cleaned up on the
// next save -- otherwise a renamed tag leaves a stray service binding the old
// port pool forever.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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
	zhdataSystemdDir       = "/etc/systemd/system"
	zhdataDefaultUnit      = "zhdata.service"
	zhdataUnitPrefix       = "zhdata-"
	zhdataUnitDropinSuffix = ".d"
)

// zhdataUnitDescription marks a unit as panel-generated. ZY10 wrote the same
// prefix, so its units are recognized (and cleaned up) by ZY11.
const zhdataUnitDescription = "Description=zhdata-tunnel server"

// zhdataDefaultAdminAddr is the admin endpoint whose inbound keeps the
// ZY10 unit name. Anything else gets a tag-derived name.
const zhdataDefaultAdminAddr = "127.0.0.1:6070"

func zhdataUnitPath(unitName string) string {
	return filepath.Join(zhdataSystemdDir, unitName)
}

func zhdataUnitDropinDir(unitName string) string {
	return zhdataUnitPath(unitName) + zhdataUnitDropinSuffix
}

// zhdataManagedUnit is one zhdata inbound plus the unit the panel owns for it.
type zhdataManagedUnit struct {
	inbound  *model.Inbound
	opts     *zhdataOptions
	unitName string
}

// sanitizeZhdataTag turns an inbound tag into the middle of a unit name:
// lowercase, anything outside [a-z0-9._-] becomes a dash, separators trimmed,
// and an empty result falls back to "inbound" so the name never degenerates
// into "zhdata-.service".
func sanitizeZhdataTag(tag string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(tag)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), ".-")
	if len(name) > 40 {
		name = strings.Trim(name[:40], ".-")
	}
	if name == "" {
		return "inbound"
	}
	return name
}

// zhdataUnitNameForTag builds the tag-derived name, taking the next free
// suffix when two tags sanitize to the same thing ("zh" and "z/h" both become
// zhdata-zh.service, which would silently merge two processes).
func zhdataUnitNameForTag(tag string, used map[string]bool) string {
	base := zhdataUnitPrefix + sanitizeZhdataTag(tag)
	name := base + ".service"
	for i := 2; used != nil && used[name]; i++ {
		name = fmt.Sprintf("%s-%d.service", base, i)
	}
	return name
}

// zhdataEffectiveAdminAddr is the address the unit will run the admin API on:
// an empty one renders the default, exactly as zhdataUnitExecStart does.
func zhdataEffectiveAdminAddr(opts *zhdataOptions) string {
	return orDefault(opts.AdminAddr, zhdataDefaultAdminAddr)
}

// zhdataUnitNameFor is the name one inbound would get outside of the full-set
// assignment (used when an inbound is being deleted and the rest may have
// claimed the shared name).
func zhdataUnitNameFor(tag string, opts *zhdataOptions) string {
	if zhdataEffectiveAdminAddr(opts) == zhdataDefaultAdminAddr {
		return zhdataDefaultUnit
	}
	return zhdataUnitNameForTag(tag, nil)
}

// zhdataManagedUnits maps every manageable zhdata inbound to a unit name.
// Manageable means its options parse -- ZY10 also required the default
// admin_addr, which left every other process (and every inbound whose admin
// address was still empty) with no unit at all.
// Inbounds are walked in tag order so the assignment is deterministic: the
// first row on the default admin_addr gets zhdata.service.
func zhdataManagedUnits(inbounds []*model.Inbound) []zhdataManagedUnit {
	type candidate struct {
		inbound *model.Inbound
		opts    *zhdataOptions
	}
	var list []candidate
	for _, inbound := range inbounds {
		if inbound == nil {
			continue
		}
		opts, err := parseZhdataOptions(inbound.Options)
		if err != nil {
			logger.Warning("zhdata unit: 入站 ", inbound.Tag, " 的 options 无法解析，跳过 unit：", err)
			continue
		}
		list = append(list, candidate{inbound: inbound, opts: opts})
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].inbound.Tag < list[j].inbound.Tag })

	used := map[string]bool{}
	units := make([]zhdataManagedUnit, 0, len(list))
	for _, item := range list {
		name := ""
		if zhdataEffectiveAdminAddr(item.opts) == zhdataDefaultAdminAddr {
			name = zhdataDefaultUnit
		}
		if name == "" || used[name] {
			name = zhdataUnitNameForTag(item.inbound.Tag, used)
		}
		used[name] = true
		units = append(units, zhdataManagedUnit{inbound: item.inbound, opts: item.opts, unitName: name})
	}
	return units
}

// zhdataUnitExecStart renders the whole ExecStart line from the inbound options.
// The order is fixed: comparing two renderings is then the same as comparing the
// launch parameters, which is what decides whether a restart is warranted.
func zhdataUnitExecStart(tag string, opts *zhdataOptions, dbPath string, binaryPath string) string {
	args := []string{
		binaryPath,
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
// operator reading systemctl status sees which inbound it belongs to -- and so
// the panel can find its own units again (orphan cleanup, inbound deletion).
func zhdataUnitText(tag string, opts *zhdataOptions, dbPath string, binaryPath string) string {
	execStart := zhdataUnitExecStart(tag, opts, dbPath, binaryPath)
	var b strings.Builder
	b.WriteString("[Unit]\n")
	b.WriteString(fmt.Sprintf("Description=%s (%s)\n", strings.TrimPrefix(zhdataUnitDescription, "Description="), orDefault(tag, "panel-managed")))
	b.WriteString("After=network.target\n\n")
	b.WriteString("[Service]\n")
	// Unit files are read by systemd, so every path in one is POSIX even when
	// the panel rendering it happens to run elsewhere.
	b.WriteString("WorkingDirectory=" + filepath.ToSlash(filepath.Dir(binaryPath)) + "\n")
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

// zhdataDropinOverrides reports a drop-in under the unit's .d directory that
// sets ExecStart. Such a file silently wins over the unit the panel writes, so
// the panel has to say so instead of believing its own write.
func zhdataDropinOverrides(dropinDir string) string {
	matches, err := filepath.Glob(filepath.Join(dropinDir, "*.conf"))
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

// panelUnitTag reads the inbound tag out of a panel-written unit file, and says
// whether the file is a panel unit at all.
func panelUnitTag(unitText string) (string, bool) {
	prefix := zhdataUnitDescription + " ("
	for _, line := range strings.Split(unitText, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Description=") {
			if !strings.HasPrefix(line, prefix) {
				return "", strings.HasPrefix(line, zhdataUnitDescription)
			}
			tag := strings.TrimSuffix(strings.TrimPrefix(line, prefix), ")")
			return tag, true
		}
	}
	return "", false
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

// zhdataSyncResult is what one sync pass produced: which units were restarted,
// what each inbound should warn about, and which unit belongs to which tag.
type zhdataSyncResult struct {
	restarted []string
	removed   []string
	warnings  map[string]string
	units     map[string]string
}

// SyncZhdataServices aligns one unit per zhdata inbound with its options after
// a config save, and drops the units no inbound maps to any more. It never
// fails the save: an unreachable systemd, a missing binary or a container
// without systemd all end in a warning.
func SyncZhdataServices(db *gorm.DB) {
	result, err := syncZhdataUnits(db, false)
	if err != nil {
		logger.Warning("zhdata unit: ", err)
		return
	}
	for tag := range result.units {
		rememberZhdataUnitWarning(tag, result.warnings[tag])
	}
}

// zhdataMissingCoreWarning is what an inbound card says when there is no core to
// launch. ZY10 told the operator to install one by hand; ZY11 points at the
// button that does it.
func zhdataMissingCoreWarning() string {
	return fmt.Sprintf("未找到 zhdata 核心：可在面板「入站」页的 zhdata 核心 处下载/更新后重新保存（面板托管路径 %s；旧路径 %s）",
		zhdataManagedBinaryPath(), zhdataLegacyBinary)
}

func zhdataSystemctlAvailable() bool {
	_, err := exec.LookPath("systemctl")
	return err == nil
}

// The two gates that decide whether systemd can be driven from here. They are
// variables so the unit-writing path below is testable on a Windows dev box (a
// stubbed systemctl, a temp unit tree) instead of only on a Linux node.
var (
	zhdataIsLinux      = func() bool { return runtime.GOOS == "linux" }
	zhdataHasSystemctl = func() bool { return zhdataSystemctlAvailable() }
	// zhdataRunSystemd is every systemctl call the panel makes, so a test can
	// watch the verbs instead of needing systemd in the box.
	zhdataRunSystemd = func(args ...string) (string, error) {
		cmd := exec.Command("systemctl", args...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	// zhdataRunInitView asks the core to make sure the user view exists. Best
	// effort: an older core without -init-view just errors and the panel keeps
	// working on its admin-API push alone.
	zhdataRunInitView = func(binaryPath string) {
		out, err := exec.Command(binaryPath, "-init-view", config.GetDBPath()).CombinedOutput()
		if err != nil {
			logger.Warning("zhdata unit: -init-view: ", err, " ", strings.TrimSpace(string(out)), " (zhdata < 0.15.7 没有该参数，用户仍靠面板推送)")
			return
		}
		logger.Info("zhdata unit: ", strings.TrimSpace(string(out)))
	}
)

// zhdataInboundsOf lists the zhdata inbounds defensively: the panel reports core
// status and installs a core whether or not the store answers, and a half-open
// database (a test binary built without cgo, a store that failed to open) must
// not take the API down with it.
func zhdataInboundsOf(db *gorm.DB) (inbounds []*model.Inbound, err error) {
	if db == nil {
		return nil, nil
	}
	defer func() {
		if r := recover(); r != nil {
			inbounds, err = nil, fmt.Errorf("数据库不可用: %v", r)
		}
	}()
	return zhdataInbounds(db)
}

func syncZhdataUnits(db *gorm.DB, forceRestart bool) (*zhdataSyncResult, error) {
	inbounds, err := zhdataInboundsOf(db)
	if err != nil {
		return nil, err
	}
	return syncZhdataUnitsFor(inbounds, forceRestart)
}

func syncZhdataUnitsFor(inbounds []*model.Inbound, forceRestart bool) (*zhdataSyncResult, error) {
	result := &zhdataSyncResult{warnings: map[string]string{}, units: map[string]string{}}
	units := zhdataManagedUnits(inbounds)
	for _, unit := range units {
		result.units[unit.inbound.Tag] = unit.unitName
	}

	// systemd only exists on the deployment target; the same caution ZY10
	// applied (no writes, no restarts on a non-Linux dev box; a warning instead
	// of a silent failure when systemd is missing).
	if !zhdataIsLinux() {
		return result, nil
	}
	if !zhdataHasSystemctl() {
		for _, unit := range units {
			result.warnings[unit.inbound.Tag] = "系统上没有 systemctl（容器或非 systemd 发行版？）：入站参数已保存，但启动参数没有同步"
		}
		return result, nil
	}

	// Orphans first: a unit the panel wrote for an inbound that is gone (deleted
	// outside the panel, or a tag rename) keeps Restart=always on an old port
	// pool forever otherwise. The Description is the marker, so an operator's own
	// zhdata-*.service is never touched.
	desired := map[string]bool{}
	for _, unit := range units {
		desired[unit.unitName] = true
	}
	zhdataRemoveOrphanUnits(desired, result)

	if len(units) == 0 {
		return result, nil
	}

	location := resolveZhdataCore()
	if !location.Installed {
		warning := zhdataMissingCoreWarning()
		for _, unit := range units {
			result.warnings[unit.inbound.Tag] = warning
		}
		return result, nil
	}
	if location.External {
		logger.Info("zhdata unit: 仍在用面板外的核心 ", location.Path, "，建议改用面板托管路径 ", zhdataManagedBinaryPath(), "（在面板里更新一次核心即可）")
	}

	for _, unit := range units {
		tag := unit.inbound.Tag
		warning, syncErr := syncOneZhdataUnit(unit, location.Path, forceRestart, result)
		if syncErr != nil {
			logger.Warning("zhdata unit: ", warning, ": ", syncErr)
			result.warnings[tag] = fmt.Sprintf("同步 %s 失败: %v", unit.unitName, syncErr)
			continue
		}
		result.warnings[tag] = warning
	}
	return result, nil
}

// syncOneZhdataUnit writes/renews one unit the way ZY10 did it, one unit per
// inbound instead of one for the whole panel.
func syncOneZhdataUnit(unit zhdataManagedUnit, binaryPath string, forceRestart bool, result *zhdataSyncResult) (warning string, err error) {
	tag := unit.inbound.Tag
	unitPath := zhdataUnitPath(unit.unitName)
	dropinDir := zhdataUnitDropinDir(unit.unitName)

	desired := zhdataUnitText(tag, unit.opts, config.GetDBPath(), binaryPath)
	created := false
	current, readErr := os.ReadFile(unitPath)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return "", fmt.Errorf("read %s: %w", unitPath, readErr)
	}
	if errors.Is(readErr, os.ErrNotExist) {
		created = true
		// A process answering on admin_addr without a unit at our path means the
		// operator started it some other way (another service name, a manual run,
		// a container). Writing ours anyway would bind the same ports twice and
		// Restart=always would turn that into a crash loop, so say it instead.
		if response, probeErr := zhdataAdminRequest(unit.opts, http.MethodGet, "/v1/version", nil); probeErr == nil && len(response) > 0 {
			return fmt.Sprintf("%s 上已有 zhdata 在跑，但 %s 不存在：面板没有生成 unit，以免和现有进程抢端口。要让面板托管，请把那个服务改名为 %s，或先停掉它再保存一次",
				zhdataEffectiveAdminAddr(unit.opts), unitPath, unit.unitName), nil
		}
	} else if string(current) == desired && !forceRestart {
		// Nothing in the launch line moved: no daemon-reload, no restart, no
		// dropped tunnels.
		if dropin := zhdataDropinOverrides(dropinDir); dropin != "" {
			return fmt.Sprintf("%s 里有 ExecStart 覆盖（%s）：面板写的启动参数不生效，请把那一行搬进入站或删掉覆盖", dropinDir, dropin), nil
		}
		return "", nil
	}

	changed := errors.Is(readErr, os.ErrNotExist) || string(current) != desired
	if changed {
		if writeErr := os.WriteFile(unitPath, []byte(desired), 0o644); writeErr != nil {
			return "", fmt.Errorf("write %s: %w", unitPath, writeErr)
		}
		if out, runErr := zhdataRunSystemd("daemon-reload"); runErr != nil {
			return "", fmt.Errorf("systemctl daemon-reload: %v (%s)", runErr, strings.TrimSpace(out))
		}
	}
	// The view is what -users reads; creating it here (idempotent, the core's
	// own sqlite engine) means a freshly started process already knows the users
	// instead of running uncapped until the panel's next push.
	zhdataRunInitView(binaryPath)

	if created {
		// enable so the node also comes back after a reboot; the restart below
		// starts it now.
		if out, runErr := zhdataRunSystemd("enable", unit.unitName); runErr != nil {
			logger.Warning("zhdata unit: systemctl enable ", unit.unitName, ": ", runErr, " ", strings.TrimSpace(out))
		}
	}
	if out, runErr := zhdataRunSystemd("restart", unit.unitName); runErr != nil {
		return "", fmt.Errorf("systemctl restart %s: %v (%s)", unit.unitName, runErr, strings.TrimSpace(out))
	}
	if result != nil {
		result.restarted = append(result.restarted, unit.unitName)
	}
	if dropin := zhdataDropinOverrides(dropinDir); dropin != "" {
		return fmt.Sprintf("已按入站 %s 更新并重启 %s，但 %s 里有 ExecStart 覆盖（%s）：实际启动参数可能仍是旧的", tag, unit.unitName, dropinDir, dropin), nil
	}
	if created {
		logger.Info("zhdata unit: 已生成并启用 ", unitPath, "（入站 ", tag, "）")
	} else {
		logger.Info("zhdata unit: 启动参数有变化或核心刚更新，已更新并重启 ", unit.unitName, "（入站 ", tag, "）")
	}
	return "", nil
}

// zhdataRemoveOrphanUnits stops, disables and deletes every panel-written
// zhdata unit that no current inbound maps to.
func zhdataRemoveOrphanUnits(desired map[string]bool, result *zhdataSyncResult) {
	for _, path := range zhdataPanelUnitFiles("") {
		name := filepath.Base(path)
		if desired[name] {
			continue
		}
		if out, runErr := zhdataRunSystemd("stop", name); runErr != nil {
			logger.Warning("zhdata unit: systemctl stop ", name, ": ", runErr, " ", strings.TrimSpace(out))
		}
		if out, runErr := zhdataRunSystemd("disable", name); runErr != nil {
			logger.Warning("zhdata unit: systemctl disable ", name, ": ", runErr, " ", strings.TrimSpace(out))
		}
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			logger.Warning("zhdata unit: 删除 ", path, ": ", removeErr)
			continue
		}
		logger.Info("zhdata unit: 已删除没有对应入站的面板 unit ", path)
		if out, runErr := zhdataRunSystemd("daemon-reload"); runErr != nil {
			logger.Warning("zhdata unit: systemctl daemon-reload: ", runErr, " ", strings.TrimSpace(out))
		}
		if result != nil {
			result.removed = append(result.removed, name)
		}
	}
}

// zhdataPanelUnitFiles lists the panel-written zhdata unit files in the systemd
// directory. tagFilter, when set, narrows to the units of one inbound tag.
func zhdataPanelUnitFiles(tag string) []string {
	matches, err := filepath.Glob(filepath.Join(zhdataSystemdDir, "zhdata*.service"))
	if err != nil {
		return nil
	}
	var out []string
	for _, path := range matches {
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		unitTag, panel := panelUnitTag(string(data))
		if !panel {
			continue
		}
		if tag != "" && unitTag != tag {
			continue
		}
		out = append(out, path)
	}
	return out
}

// RemoveZhdataUnit stops and deletes the unit of a zhdata inbound that is being
// deleted. Losing a unit file is never a reason to fail the delete -- the row is
// gone either way -- so everything here warns.
func RemoveZhdataUnit(tag string, options json.RawMessage) {
	if !zhdataIsLinux() || !zhdataHasSystemctl() {
		return
	}
	names := []string{}
	// A panel unit carrying this tag in its Description is the authoritative
	// answer: the name the tag would get now can differ (another inbound claimed
	// the default name, or the tag was renamed after the unit was written).
	for _, path := range zhdataPanelUnitFiles(orDefault(tag, "panel-managed")) {
		names = append(names, filepath.Base(path))
	}
	if opts, err := parseZhdataOptions(options); err == nil {
		name := zhdataUnitNameFor(tag, opts)
		path := zhdataUnitPath(name)
		if !containsName(names, name) && zhdataFileExists(path) {
			if data, readErr := os.ReadFile(path); readErr == nil {
				if _, panel := panelUnitTag(string(data)); panel {
					names = append(names, name)
				}
			}
		}
	}
	if len(names) == 0 {
		return
	}
	reload := false
	for _, name := range names {
		if out, runErr := zhdataRunSystemd("stop", name); runErr != nil {
			logger.Warning("zhdata unit: systemctl stop ", name, ": ", runErr, " ", strings.TrimSpace(out))
		}
		if out, runErr := zhdataRunSystemd("disable", name); runErr != nil {
			logger.Warning("zhdata unit: systemctl disable ", name, ": ", runErr, " ", strings.TrimSpace(out))
		}
		path := zhdataUnitPath(name)
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			logger.Warning("zhdata unit: 删除 ", path, ": ", removeErr)
		}
		logger.Info("zhdata unit: 入站 ", tag, " 已删除，同步删除 ", path)
		reload = true
	}
	if reload {
		if out, runErr := zhdataRunSystemd("daemon-reload"); runErr != nil {
			logger.Warning("zhdata unit: systemctl daemon-reload: ", runErr, " ", strings.TrimSpace(out))
		}
	}
	rememberZhdataUnitWarning(tag, "")
}

func containsName(names []string, name string) bool {
	for _, candidate := range names {
		if candidate == name {
			return true
		}
	}
	return false
}
