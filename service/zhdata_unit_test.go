package service

// The panel owns the *launch parameters* of every zhdata process, and since ZY11
// one systemd unit per zhdata inbound instead of one for the whole panel. These
// tests pin the two things that went wrong in the field (C455: the UI moved the
// port pool to 45501-45564 while the service still bound 49888-49919) plus the
// new ZY11 surface: the unit name a tag gets, the binary a unit points at, and
// the units a save has to clean up behind it.
//
// They run against in-memory inbound rows and a stubbed systemctl, so they pass
// on a Windows dev box (no systemd, and a sqlite driver that needs cgo) exactly
// as they do on the Linux node they were written for.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alireza0/s-ui/database/model"
	"github.com/alireza0/s-ui/util"
)

// useZhdataUnitPaths points the unit tree at a temp directory.
func useZhdataUnitPaths(t *testing.T, systemdDir string) {
	t.Helper()
	old := zhdataSystemdDir
	zhdataSystemdDir = systemdDir
	t.Cleanup(func() { zhdataSystemdDir = old })
}

// useFakeSystemd makes the systemd and core-exec calls observable, so the write
// path is testable on a Windows dev box too. It returns the systemctl verbs in
// call order.
func useFakeSystemd(t *testing.T) *[]string {
	t.Helper()
	oldRun, oldLinux, oldCtl, oldInit := zhdataRunSystemd, zhdataIsLinux, zhdataHasSystemctl, zhdataRunInitView
	calls := []string{}
	zhdataRunSystemd = func(args ...string) (string, error) {
		calls = append(calls, strings.Join(args, " "))
		return "", nil
	}
	zhdataIsLinux = func() bool { return true }
	zhdataHasSystemctl = func() bool { return true }
	zhdataRunInitView = func(string) {}
	t.Cleanup(func() {
		zhdataRunSystemd, zhdataIsLinux, zhdataHasSystemctl, zhdataRunInitView = oldRun, oldLinux, oldCtl, oldInit
	})
	return &calls
}

// useZhdataCorePaths points the panel-managed core dir and the legacy path at a
// temp tree (the legacy default is an absolute Linux path this box has not got).
func useZhdataCorePaths(t *testing.T, managedDir, legacy string) {
	t.Helper()
	oldDir, oldLegacy := zhdataCoreDirOverride, zhdataLegacyBinary
	zhdataCoreDirOverride, zhdataLegacyBinary = managedDir, legacy
	t.Cleanup(func() {
		zhdataCoreDirOverride, zhdataLegacyBinary = oldDir, oldLegacy
	})
}

// zhdataRows builds inbound rows in memory: the unit code only needs tag, type
// and options, so these tests do not depend on a sqlite driver being buildable.
func zhdataRows(items ...[2]string) []*model.Inbound {
	rows := make([]*model.Inbound, 0, len(items))
	for _, item := range items {
		rows = append(rows, &model.Inbound{
			Type:    zhdataType,
			Tag:     item[0],
			Addrs:   json.RawMessage(`[]`),
			OutJson: json.RawMessage(`{}`),
			Options: json.RawMessage(item[1]),
		})
	}
	return rows
}

func mustZhdataOptions(t *testing.T, options string) *zhdataOptions {
	t.Helper()
	opts, err := parseZhdataOptions(json.RawMessage(options))
	if err != nil {
		t.Fatal(err)
	}
	return opts
}

const plainZhdataOptions = `{"tag":"NODE-ZH","listen":"","listen_ports":[49888,49889],"backend":"direct","admin_addr":"127.0.0.1:6070","max_sessions":1200}`

const relayZhdataOptions = `{"listen_ports":[50888],"backend":"direct","admin_addr":"127.0.0.1:6071","max_sessions":600,"upstream":["127.0.0.1:49888","10.0.0.1:49888"]}`

const testBinary = "/usr/local/s-ui/zhdata/zhdata"

// The launch ports and the link ports must come from one place, or they drift
// the way they did on C455.
func TestZhdataUnitExecStartUsesTheLinkListenString(t *testing.T) {
	opts := mustZhdataOptions(t, plainZhdataOptions)
	execStart := zhdataUnitExecStart("NODE-ZH", opts, "/usr/local/s-ui/db/s-ui.db", testBinary)

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(plainZhdataOptions), &raw); err != nil {
		t.Fatal(err)
	}
	wantListen := util.BuildZhdataListen(raw)
	if wantListen == "" {
		t.Fatal("link listen is empty, the test is not exercising the port pool")
	}
	if !strings.HasPrefix(execStart, testBinary+" ") {
		t.Fatalf("ExecStart %q does not launch the panel-managed core", execStart)
	}
	if !strings.Contains(execStart, "-listen "+wantListen+" ") {
		t.Fatalf("ExecStart %q does not carry the link listen %q", execStart, wantListen)
	}
	for _, want := range []string{
		"-mode server",
		"-admin 127.0.0.1:6070",
		"-users /usr/local/s-ui/db/s-ui.db",
		"-backend direct",
		"-max-sessions 1200",
		"-max-clients 1200",
		"-loglevel info",
	} {
		if !strings.Contains(execStart, want) {
			t.Errorf("ExecStart %q is missing %q", execStart, want)
		}
	}
}

// A non-empty upstream list is a relay, and the core wants the explicit zhdata
// backend for it (contract §6) -- not whatever else the row happens to say.
func TestZhdataUnitExecStartTurnsUpstreamIntoRelayBackend(t *testing.T) {
	opts := mustZhdataOptions(t, relayZhdataOptions)
	execStart := zhdataUnitExecStart("NODE-RELAY", opts, "/db/s-ui.db", testBinary)
	if !strings.Contains(execStart, "-backend zhdata") {
		t.Fatalf("relay ExecStart %q lacks the zhdata backend", execStart)
	}
	if !strings.Contains(execStart, "-upstream 127.0.0.1:49888,10.0.0.1:49888") {
		t.Fatalf("relay ExecStart %q lacks the upstream list", execStart)
	}
	if strings.Contains(execStart, "-backend direct") {
		t.Fatalf("relay ExecStart %q still carries the direct backend", execStart)
	}
}

func TestZhdataUnitExecStartPassesDecoyAndToken(t *testing.T) {
	opts := mustZhdataOptions(t, `{"listen_ports":[49888],"admin_addr":"127.0.0.1:6070","decoy":"example.com:443","admin_token":"sekret","max_sessions":0}`)
	execStart := zhdataUnitExecStart("NODE-ZH", opts, "/db/s-ui.db", testBinary)
	if !strings.Contains(execStart, "-decoy example.com:443") {
		t.Errorf("ExecStart %q lacks the decoy", execStart)
	}
	if !strings.Contains(execStart, "-admin-token sekret") {
		t.Errorf("ExecStart %q lacks the admin token", execStart)
	}
	// 0 is the core's "unlimited" and has to be written down rather than left
	// out, so a later reader can tell unlimited apart from forgotten.
	if !strings.Contains(execStart, "-max-sessions 0 -max-clients 0") {
		t.Errorf("ExecStart %q does not state unlimited sessions", execStart)
	}
}

// An inbound with no port pool must not render an empty -listen argument.
func TestZhdataUnitExecStartOmitsEmptyListen(t *testing.T) {
	opts := mustZhdataOptions(t, `{"backend":"direct","admin_addr":"127.0.0.1:6070","max_sessions":100}`)
	execStart := zhdataUnitExecStart("NODE-ZH", opts, "/db/s-ui.db", testBinary)
	if strings.Contains(execStart, "-listen") {
		t.Fatalf("ExecStart %q binds nothing yet carries -listen", execStart)
	}
	if strings.Contains(execStart, "  ") {
		t.Fatalf("ExecStart %q has a gap from an omitted argument", execStart)
	}
}

// Re-saving the same inbound must not restart the process: JSON key order and
// whitespace in the row are not launch parameter changes.
func TestZhdataUnitRenderIsStableAcrossEquivalentSaves(t *testing.T) {
	spaced := mustZhdataOptions(t, `{
	  "max_sessions": 1200,
	  "admin_addr": "127.0.0.1:6070",
	  "listen_ports": [49888, 49889],
	  "backend": "direct"
	}`)
	compact := mustZhdataOptions(t, plainZhdataOptions)
	first := zhdataUnitText("NODE-ZH", spaced, "/db/s-ui.db", testBinary)
	second := zhdataUnitText("NODE-ZH", compact, "/db/s-ui.db", testBinary)
	if first != second {
		t.Fatalf("equivalent options rendered differently:\n%s\n---\n%s", first, second)
	}
}

func TestZhdataUnitTextIsAServiceFileForTheTag(t *testing.T) {
	text := zhdataUnitText("NODE-ZH", mustZhdataOptions(t, plainZhdataOptions), "/db/s-ui.db", testBinary)
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]", "WantedBy=multi-user.target",
		"Restart=always", "Description=zhdata-tunnel server (NODE-ZH)", "ExecStart=",
		"WorkingDirectory=/usr/local/s-ui/zhdata",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("unit text is missing %q:\n%s", want, text)
		}
	}
}

// systemd takes the last non-empty ExecStart, which is exactly how the C498
// drop-in (clear the line, then set 64 ports) works.
func TestExecStartOfTakesTheLastNonEmptyLine(t *testing.T) {
	unit := "[Service]\nExecStart=/bin/a -listen :1\nExecStart=\nExecStart=/bin/b -listen :2,:3\n"
	if got := execStartOf(unit); got != "/bin/b -listen :2,:3" {
		t.Fatalf("execStartOf = %q", got)
	}
	if got := execStartOf("[Service]\nRestart=always\n"); got != "" {
		t.Fatalf("execStartOf on a unit without ExecStart = %q", got)
	}
}

// C498 keeps its port pool in a drop-in. The panel has to notice that its own
// write would be overridden rather than claim success.
func TestZhdataDropinOverridesFindsTheOverridingFile(t *testing.T) {
	dir := t.TempDir()

	if got := zhdataDropinOverrides(dir); got != "" {
		t.Fatalf("drop-in override without any files: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "environment.conf"), []byte("[Service]\nEnvironment=A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := zhdataDropinOverrides(dir); got != "" {
		t.Fatalf("drop-in override for a file without ExecStart: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "ports64.conf"), []byte("[Service]\nExecStart=\nExecStart=/bin/b -listen :49888\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := zhdataDropinOverrides(dir)
	if !strings.HasSuffix(got, "ports64.conf") {
		t.Fatalf("drop-in override = %q, want ports64.conf", got)
	}
}

// ZY11 names: the row on the default admin_addr keeps zhdata.service so a ZY10
// node is untouched; everything else gets a tag-derived name that cannot collide
// with another tag.
func TestSanitizeZhdataTagAndUnitNames(t *testing.T) {
	cases := []struct {
		tag, want string
	}{
		{"NODE-ZH", "node-zh"},
		{"C498 Relay", "c498-relay"},
		{"zh/..\\x", "zh-..-x"},
		{"入站", "inbound"},
		{"   ", "inbound"},
		{"__proto__", "__proto__"},
		{"-x-", "x"},
	}
	for _, c := range cases {
		if got := sanitizeZhdataTag(c.tag); got != c.want {
			t.Errorf("sanitizeZhdataTag(%q) = %q, want %q", c.tag, got, c.want)
		}
	}
	// A runaway tag is capped, not pasted into a unit name wholesale.
	if long := sanitizeZhdataTag("NODE/VERY/LONG/TAG/THAT/GOES/ON/AND/ON/FOREVER"); len(long) > 40 || !strings.HasPrefix(long, "node-very-long") {
		t.Errorf("long tag sanitized to %q", long)
	}

	used := map[string]bool{"zhdata-zh.service": true}
	if got := zhdataUnitNameForTag("ZH", used); got != "zhdata-zh-2.service" {
		t.Fatalf("colliding tag name = %q", got)
	}
	// "z-h" is a different name, so it does not need a suffix.
	if got := zhdataUnitNameForTag("Z/H", used); got != "zhdata-z-h.service" {
		t.Fatalf("z-h unit name = %q, want zhdata-z-h.service", got)
	}

	// The default admin address keeps the ZY10 name.
	if got := zhdataUnitNameFor("ANY", &zhdataOptions{AdminAddr: zhdataDefaultAdminAddr}); got != zhdataDefaultUnit {
		t.Fatalf("default admin unit = %q", got)
	}
	// An empty admin_addr renders the default admin, so it is the default unit.
	if got := zhdataUnitNameFor("ANY", &zhdataOptions{}); got != zhdataDefaultUnit {
		t.Fatalf("empty admin_addr unit = %q, want %s", got, zhdataDefaultUnit)
	}
	if got := zhdataUnitNameFor("RELAY", &zhdataOptions{AdminAddr: "127.0.0.1:6071"}); got != "zhdata-relay.service" {
		t.Fatalf("non-default unit = %q", got)
	}
}

// One unit per inbound, deterministic names, unparseable rows skipped.
func TestZhdataManagedUnitsNamesEveryInbound(t *testing.T) {
	rows := zhdataRows(
		[2]string{"A-ZH", plainZhdataOptions},
		[2]string{"B-RELAY", relayZhdataOptions},
		[2]string{"C-ZH", `{"listen_ports":[46000],"admin_addr":"","max_sessions":10}`},
		[2]string{"D-BROKEN", `not json`},
	)
	units := zhdataManagedUnits(rows)
	byTag := map[string]string{}
	for _, unit := range units {
		byTag[unit.inbound.Tag] = unit.unitName
	}
	if len(units) != 3 {
		t.Fatalf("managed units = %v, want the three parsable inbounds (D-BROKEN is not)", byTag)
	}
	if byTag["A-ZH"] != "zhdata.service" {
		t.Errorf("default admin inbound got %q, want zhdata.service", byTag["A-ZH"])
	}
	if byTag["B-RELAY"] != "zhdata-b-relay.service" {
		t.Errorf("relay inbound got %q", byTag["B-RELAY"])
	}
	if byTag["C-ZH"] != "zhdata-c-zh.service" {
		t.Errorf("empty admin_addr inbound got %q: it renders the default admin but must not steal zhdata.service", byTag["C-ZH"])
	}

	// Two rows on the default admin_addr: only one can own zhdata.service, and
	// which one is deterministic (tag order), not map iteration order.
	pair := zhdataRows(
		[2]string{"Z2", `{"admin_addr":"","max_sessions":5}`},
		[2]string{"Z1", `{"admin_addr":"127.0.0.1:6070","max_sessions":5}`},
	)
	names := map[string]string{}
	for _, unit := range zhdataManagedUnits(pair) {
		names[unit.inbound.Tag] = unit.unitName
	}
	if names["Z1"] != "zhdata.service" || names["Z2"] != "zhdata-z2.service" {
		t.Fatalf("two default-admin rows got %v", names)
	}
}

// The panel writes one unit per inbound, points it at the managed core, and
// enables and restarts exactly the units it touched.
func TestSyncZhdataUnitsWritesOneUnitPerInbound(t *testing.T) {
	systemdDir := t.TempDir()
	coreDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, coreDir, filepath.Join(t.TempDir(), "no-legacy"))
	calls := useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	binary := zhdataManagedBinaryPath()
	if err := os.WriteFile(binary, []byte("core"), 0o755); err != nil {
		t.Fatal(err)
	}

	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions}, [2]string{"NODE-RELAY", relayZhdataOptions})
	result, err := syncZhdataUnitsFor(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	for tag, warning := range result.warnings {
		if warning != "" {
			t.Fatalf("unexpected warning for %s: %s", tag, warning)
		}
	}

	primary := readUnitFile(t, systemdDir, "zhdata.service")
	relay := readUnitFile(t, systemdDir, "zhdata-node-relay.service")
	if !strings.Contains(primary, "Description=zhdata-tunnel server (NODE-ZH)") {
		t.Errorf("default unit lost its tag: %s", primary)
	}
	if !strings.Contains(relay, binary) || !strings.Contains(primary, binary) {
		t.Errorf("units must launch the panel-managed core %s:\n%s\n---\n%s", binary, primary, relay)
	}
	if !strings.Contains(relay, "-backend zhdata") {
		t.Errorf("relay unit is not a relay: %s", relay)
	}
	// Units are walked in tag order (NODE-RELAY before NODE-ZH), which is what
	// makes the name assignment deterministic too.
	wantCalls := []string{
		"daemon-reload", "enable zhdata-node-relay.service", "restart zhdata-node-relay.service",
		"daemon-reload", "enable zhdata.service", "restart zhdata.service",
	}
	if strings.Join(*calls, "|") != strings.Join(wantCalls, "|") {
		t.Fatalf("systemctl calls = %v, want %v", *calls, wantCalls)
	}
	if len(result.restarted) != 2 {
		t.Fatalf("restarted = %v", result.restarted)
	}
}

// ZY10's caution, kept unit by unit: an identical unit means no restart.
func TestSyncZhdataUnitsDoesNotTouchAnUnchangedUnit(t *testing.T) {
	systemdDir := t.TempDir()
	coreDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, coreDir, filepath.Join(t.TempDir(), "no-legacy"))
	calls := useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	binary := zhdataManagedBinaryPath()
	if err := os.WriteFile(binary, []byte("core"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions})

	if _, err := syncZhdataUnitsFor(rows, false); err != nil {
		t.Fatal(err)
	}
	written := readUnitFile(t, systemdDir, "zhdata.service")
	*calls = nil

	result, err := syncZhdataUnitsFor(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("an unchanged unit still moved systemd: %v", *calls)
	}
	if len(result.restarted) != 0 {
		t.Fatalf("an unchanged unit restarted: %v", result.restarted)
	}
	if len(result.warnings) != 1 || result.warnings["NODE-ZH"] != "" {
		t.Fatalf("warnings = %v", result.warnings)
	}

	// forceRestart is what a core update needs: same arguments, new binary.
	*calls = nil
	result, err = syncZhdataUnitsFor(rows, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(*calls, "|") != "restart zhdata.service" {
		t.Fatalf("force restart calls = %v", *calls)
	}
	if strings.Join(result.restarted, ",") != "zhdata.service" {
		t.Fatalf("force restart units = %v", result.restarted)
	}
	if readUnitFile(t, systemdDir, "zhdata.service") != written {
		t.Fatal("forceRestart rewrote an unchanged unit")
	}
}

// No core to launch is not a silent success, and since ZY11 the warning names
// the button that fixes it.
func TestSyncZhdataUnitsWarnsWhenTheCoreIsMissing(t *testing.T) {
	systemdDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, t.TempDir(), filepath.Join(t.TempDir(), "no-legacy"))
	useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions})
	result, err := syncZhdataUnitsFor(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	warning := result.warnings["NODE-ZH"]
	if !strings.Contains(warning, "未找到 zhdata 核心") || !strings.Contains(warning, "下载") {
		t.Fatalf("warning = %q, want it to point at the panel's own download", warning)
	}
	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata.service")); !os.IsNotExist(err) {
		t.Fatal("a unit was written even though the binary is missing")
	}

	rememberZhdataUnitWarning("NODE-ZH", warning)
	defer rememberZhdataUnitWarning("NODE-ZH", "")
	found := false
	for _, w := range zhdataWarnings(&ZhdataHealth{Tag: "NODE-ZH", AdminAddr: "127.0.0.1:6070", Reachable: true}) {
		if w.Code == "unit" {
			found = true
		}
	}
	if !found {
		t.Fatal("the unit problem never reached the inbound card warnings")
	}
}

// A node that still has the ZY10-era /opt/zhdata install keeps working: the unit
// launches that binary until the panel's own core exists.
func TestSyncZhdataUnitsFallsBackToTheLegacyBinary(t *testing.T) {
	systemdDir := t.TempDir()
	legacy := filepath.Join(t.TempDir(), "zhdata-tunnel-linux-amd64")
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, t.TempDir(), legacy)
	useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	if err := os.WriteFile(legacy, []byte("old core"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions})
	if _, err := syncZhdataUnitsFor(rows, false); err != nil {
		t.Fatal(err)
	}
	unit := readUnitFile(t, systemdDir, "zhdata.service")
	if !strings.Contains(unit, legacy) {
		t.Fatalf("unit does not launch the legacy core %s:\n%s", legacy, unit)
	}
	if location := resolveZhdataCore(); !location.Installed || !location.External {
		t.Fatalf("legacy install resolved to %+v, want installed+external", location)
	}
}

// The ZY10 protection against binding a busy port twice still applies per unit.
func TestSyncZhdataUnitsSkipsABusyAdminAddr(t *testing.T) {
	systemdDir := t.TempDir()
	coreDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, coreDir, filepath.Join(t.TempDir(), "no-legacy"))
	useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, `{"version":"0.15.8"}`)
	}))
	defer server.Close()

	binary := zhdataManagedBinaryPath()
	if err := os.WriteFile(binary, []byte("core"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := zhdataRows([2]string{"NODE-OTHER", `{"listen_ports":[47000],"admin_addr":"` + serverAddr(t, server.URL) + `","max_sessions":10}`})
	result, err := syncZhdataUnitsFor(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	warning := result.warnings["NODE-OTHER"]
	if !strings.Contains(warning, "面板没有生成 unit") {
		t.Fatalf("warning = %q, want the do-not-steal-a-running-process sentence", warning)
	}
	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata-node-other.service")); !os.IsNotExist(err) {
		t.Fatal("the panel wrote a unit over someone else's running process")
	}
}

// A drop-in still wins over what the panel writes, and the panel says so.
func TestSyncZhdataUnitsReportsDropinOverride(t *testing.T) {
	systemdDir := t.TempDir()
	coreDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, coreDir, filepath.Join(t.TempDir(), "no-legacy"))
	useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	binary := zhdataManagedBinaryPath()
	if err := os.WriteFile(binary, []byte("core"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions})
	if _, err := syncZhdataUnitsFor(rows, false); err != nil {
		t.Fatal(err)
	}

	dropin := filepath.Join(systemdDir, "zhdata.service.d")
	if err := os.MkdirAll(dropin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropin, "ports64.conf"), []byte("[Service]\nExecStart=\nExecStart=/bin/x\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := syncZhdataUnitsFor(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.warnings["NODE-ZH"], "ExecStart 覆盖") {
		t.Fatalf("warning = %q, want the drop-in override sentence", result.warnings["NODE-ZH"])
	}
}

// A unit the panel wrote for an inbound that is gone has to go with it; an
// operator's own zhdata-*.service is never touched.
func TestSyncZhdataUnitsCleansOrphanPanelUnits(t *testing.T) {
	systemdDir := t.TempDir()
	coreDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, coreDir, filepath.Join(t.TempDir(), "no-legacy"))
	calls := useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	binary := zhdataManagedBinaryPath()
	if err := os.WriteFile(binary, []byte("core"), 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := "[Unit]\nDescription=zhdata-tunnel server (OLD-TAG)\nAfter=network.target\n\n[Service]\nExecStart=" + binary + " -mode server\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile(filepath.Join(systemdDir, "zhdata-old-tag.service"), []byte(orphan), 0o644); err != nil {
		t.Fatal(err)
	}
	operator := "[Unit]\nDescription=zhdata relay on C498\n\n[Service]\nExecStart=/opt/zhdata/x -mode server\n"
	if err := os.WriteFile(filepath.Join(systemdDir, "zhdata-manual.service"), []byte(operator), 0o644); err != nil {
		t.Fatal(err)
	}

	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions})
	result, err := syncZhdataUnitsFor(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata-old-tag.service")); !os.IsNotExist(err) {
		t.Fatal("the panel left its own orphan unit behind")
	}
	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata-manual.service")); err != nil {
		t.Fatal("the panel deleted a unit it never wrote")
	}
	if len(result.removed) != 1 || result.removed[0] != "zhdata-old-tag.service" {
		t.Fatalf("removed = %v", result.removed)
	}
	joined := strings.Join(*calls, "|")
	if !strings.Contains(joined, "stop zhdata-old-tag.service") || !strings.Contains(joined, "disable zhdata-old-tag.service") {
		t.Fatalf("orphan was not stopped and disabled: %v", *calls)
	}
	if strings.Contains(joined, "zhdata-manual.service") {
		t.Fatalf("operator unit was touched: %v", *calls)
	}
}

// The ZY10 default unit is an orphan too once no inbound sits on the default
// admin_addr any more.
func TestSyncZhdataUnitsCleansAnOrphanDefaultUnit(t *testing.T) {
	systemdDir := t.TempDir()
	coreDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	useZhdataCorePaths(t, coreDir, filepath.Join(t.TempDir(), "no-legacy"))
	calls := useFakeSystemd(t)
	t.Setenv(zhdataCoreEnvVar, "")

	binary := zhdataManagedBinaryPath()
	if err := os.WriteFile(binary, []byte("core"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A ZY10 unit for a tag that has since moved to its own admin port.
	zy10 := "[Unit]\nDescription=zhdata-tunnel server (NODE-ZH)\n\n[Service]\nExecStart=" + binary + " -mode server -admin 127.0.0.1:6070\n\n[Install]\nWantedBy=multi-user.target\n"
	if err := os.WriteFile(filepath.Join(systemdDir, "zhdata.service"), []byte(zy10), 0o644); err != nil {
		t.Fatal(err)
	}

	rows := zhdataRows([2]string{"NODE-ZH", `{"listen_ports":[48000],"admin_addr":"127.0.0.1:6071","max_sessions":10}`})
	result, err := syncZhdataUnitsFor(rows, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.removed) != 1 || result.removed[0] != "zhdata.service" {
		t.Fatalf("removed = %v, want the abandoned zhdata.service", result.removed)
	}
	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata.service")); !os.IsNotExist(err) {
		t.Fatal("zhdata.service survived the inbound that moved off the default admin_addr")
	}
	joined := strings.Join(*calls, "|")
	if !strings.Contains(joined, "restart zhdata-node-zh.service") {
		t.Fatalf("the new unit was never started: %v", *calls)
	}
}

// Deleting an inbound takes its unit with it, including the ZY10 default name.
func TestRemoveZhdataUnitDeletesTheUnitsOfOneTag(t *testing.T) {
	systemdDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	calls := useFakeSystemd(t)

	panelUnit := func(tag string) string {
		return "[Unit]\nDescription=zhdata-tunnel server (" + tag + ")\n\n[Service]\nExecStart=/x -mode server\n"
	}
	if err := os.WriteFile(filepath.Join(systemdDir, "zhdata.service"), []byte(panelUnit("NODE-ZH")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(systemdDir, "zhdata-other.service"), []byte(panelUnit("OTHER")), 0o644); err != nil {
		t.Fatal(err)
	}

	RemoveZhdataUnit("NODE-ZH", json.RawMessage(plainZhdataOptions))

	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata.service")); !os.IsNotExist(err) {
		t.Fatal("the unit of the deleted inbound is still there")
	}
	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata-other.service")); err != nil {
		t.Fatal("deleting one inbound deleted another inbound's unit")
	}
	joined := strings.Join(*calls, "|")
	for _, want := range []string{"stop zhdata.service", "disable zhdata.service", "daemon-reload"} {
		if !strings.Contains(joined, want) {
			t.Errorf("systemctl calls %v are missing %q", *calls, want)
		}
	}
}

// A renamed tag is the tricky delete: the unit on disk carries the name the old
// tag got, while the row being deleted would compute a different one.
func TestRemoveZhdataUnitFindsTheUnitOfARenamedTag(t *testing.T) {
	systemdDir := t.TempDir()
	useZhdataUnitPaths(t, systemdDir)
	calls := useFakeSystemd(t)

	// Written when the tag was OLD-TAG; the row now says NEW-TAG.
	old := "[Unit]\nDescription=zhdata-tunnel server (OLD-TAG)\n\n[Service]\nExecStart=/x -mode server\n"
	if err := os.WriteFile(filepath.Join(systemdDir, "zhdata-old-tag.service"), []byte(old), 0o644); err != nil {
		t.Fatal(err)
	}
	RemoveZhdataUnit("OLD-TAG", json.RawMessage(relayZhdataOptions))
	if _, err := os.Stat(filepath.Join(systemdDir, "zhdata-old-tag.service")); !os.IsNotExist(err) {
		t.Fatal("the unit written under the old tag survived")
	}
	if !strings.Contains(strings.Join(*calls, "|"), "stop zhdata-old-tag.service") {
		t.Fatalf("systemctl calls = %v", *calls)
	}
}

// Clearing the stored warning matters as much as setting it: a node that got
// fixed must stop warning.
func TestRememberZhdataUnitWarningClears(t *testing.T) {
	rememberZhdataUnitWarning("CLEAR-ZH", "boom")
	if zhdataUnitWarning("CLEAR-ZH") != "boom" {
		t.Fatal("warning was not stored")
	}
	rememberZhdataUnitWarning("CLEAR-ZH", "")
	if zhdataUnitWarning("CLEAR-ZH") != "" {
		t.Fatal("warning was not cleared")
	}
}

// serverAddr turns an httptest URL into the host:port an inbound's admin_addr
// would carry.
func serverAddr(t *testing.T, url string) string {
	t.Helper()
	return strings.TrimPrefix(url, "http://")
}

func readUnitFile(t *testing.T, dir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
