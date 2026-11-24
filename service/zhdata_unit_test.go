package service

// The panel only owns the *launch parameters* of the zhdata process; the process
// itself stays outside it. These tests pin the two things that went wrong in the
// field (C455: the UI moved the port pool to 45501-45564 while the service still
// bound 49888-49919): the ExecStart line has to be derived from the same port
// pool the links use, and a save has to be able to say what it could not do
// instead of looking healthy.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alireza0/s-ui/database/model"
	"github.com/alireza0/s-ui/util"
)

func useZhdataUnitPaths(t *testing.T, unitPath, dropinDir, binary string) {
	t.Helper()
	oldUnit, oldDropin, oldBinary := zhdataUnitPath, zhdataUnitDropin, zhdataBinaryPath
	zhdataUnitPath, zhdataUnitDropin, zhdataBinaryPath = unitPath, dropinDir, binary
	t.Cleanup(func() {
		zhdataUnitPath, zhdataUnitDropin, zhdataBinaryPath = oldUnit, oldDropin, oldBinary
	})
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

// The launch ports and the link ports must come from one place, or they drift
// the way they did on C455.
func TestZhdataUnitExecStartUsesTheLinkListenString(t *testing.T) {
	opts := mustZhdataOptions(t, plainZhdataOptions)
	execStart := zhdataUnitExecStart("NODE-ZH", opts, "/usr/local/s-ui/db/s-ui.db")

	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(plainZhdataOptions), &raw); err != nil {
		t.Fatal(err)
	}
	wantListen := util.BuildZhdataListen(raw)
	if wantListen == "" {
		t.Fatal("link listen is empty, the test is not exercising the port pool")
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
	opts := mustZhdataOptions(t, `{"listen_ports":[50888],"backend":"direct","admin_addr":"127.0.0.1:6071","max_sessions":600,"upstream":["127.0.0.1:49888","10.0.0.1:49888"]}`)
	execStart := zhdataUnitExecStart("NODE-RELAY", opts, "/db/s-ui.db")
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
	execStart := zhdataUnitExecStart("NODE-ZH", opts, "/db/s-ui.db")
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
	execStart := zhdataUnitExecStart("NODE-ZH", opts, "/db/s-ui.db")
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
	first := zhdataUnitText("NODE-ZH", spaced, "/db/s-ui.db")
	second := zhdataUnitText("NODE-ZH", compact, "/db/s-ui.db")
	if first != second {
		t.Fatalf("equivalent options rendered differently:\n%s\n---\n%s", first, second)
	}
}

func TestZhdataUnitTextIsAServiceFileForTheTag(t *testing.T) {
	text := zhdataUnitText("NODE-ZH", mustZhdataOptions(t, plainZhdataOptions), "/db/s-ui.db")
	for _, want := range []string{
		"[Unit]", "[Service]", "[Install]", "WantedBy=multi-user.target",
		"Restart=always", "Description=zhdata-tunnel server (NODE-ZH)", "ExecStart=",
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
	useZhdataUnitPaths(t, filepath.Join(dir, "zhdata.service"), dir, filepath.Join(dir, "zhdata"))

	if got := zhdataDropinOverrides(); got != "" {
		t.Fatalf("drop-in override without any files: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "environment.conf"), []byte("[Service]\nEnvironment=A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := zhdataDropinOverrides(); got != "" {
		t.Fatalf("drop-in override for a file without ExecStart: %q", got)
	}
	if err := os.WriteFile(filepath.Join(dir, "ports64.conf"), []byte("[Service]\nExecStart=\nExecStart=/bin/b -listen :49888\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := zhdataDropinOverrides()
	if !strings.HasSuffix(got, "ports64.conf") {
		t.Fatalf("drop-in override = %q, want ports64.conf", got)
	}
}

// The panel manages one unit. A second zhdata inbound on its own admin port
// (C498's relay on 6071) stays the operator's.
func TestZhdataManagedInboundPicksThePanelProcess(t *testing.T) {
	db := zhdataTestDB(t)
	relay := seedZhdataInbound(t, db, "NODE-RELAY", `{"listen_ports":[50888],"admin_addr":"127.0.0.1:6071","upstream":["127.0.0.1:49888"],"max_sessions":600}`)
	primary := seedZhdataInbound(t, db, "NODE-ZH", plainZhdataOptions)

	inbounds, err := zhdataInbounds(db)
	if err != nil {
		t.Fatal(err)
	}
	inbound, opts := zhdataManagedInbound(inbounds)
	if inbound == nil || inbound.Tag != primary.Tag {
		t.Fatalf("managed inbound = %+v, want %s", inbound, primary.Tag)
	}
	if inbound.Tag == relay.Tag {
		t.Fatal("the operator's relay inbound must not become the panel's unit")
	}
	if opts.MaxSessions != 1200 {
		t.Fatalf("managed options lost the session cap: %+v", opts)
	}

	onlyRelay := []*model.Inbound{relay}
	if other, _ := zhdataManagedInbound(onlyRelay); other != nil {
		t.Fatalf("managed inbound on a foreign admin_addr = %s", other.Tag)
	}
}

func TestSyncZhdataServiceWithoutInboundDoesNothing(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "zhdata.service")
	useZhdataUnitPaths(t, unitPath, dir, filepath.Join(dir, "zhdata-tunnel"))

	tag, warning, err := syncZhdataService(zhdataTestDB(t))
	if err != nil {
		t.Fatal(err)
	}
	if tag != "" || warning != "" {
		t.Fatalf("no inbound yet reported tag=%q warning=%q", tag, warning)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatal("a panel without zhdata inbounds must not create a unit")
	}
}

// No binary means the launch parameters cannot be applied at all. That has to
// land as a warning on the inbound card, not as a silently "successful" save.
func TestSyncZhdataServiceWarnsWhenTheCoreIsNotInstalled(t *testing.T) {
	dir := t.TempDir()
	unitPath := filepath.Join(dir, "zhdata.service")
	useZhdataUnitPaths(t, unitPath, dir, filepath.Join(dir, "missing-zhdata"))

	db := zhdataTestDB(t)
	seedZhdataInbound(t, db, "NODE-ZH", plainZhdataOptions)

	tag, warning, err := syncZhdataService(db)
	if err != nil {
		t.Fatal(err)
	}
	if tag != "NODE-ZH" {
		t.Fatalf("tag = %q, want NODE-ZH", tag)
	}
	if !strings.Contains(warning, "找不到核心二进制") {
		t.Fatalf("warning = %q, want the missing binary sentence", warning)
	}
	if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
		t.Fatal("a unit was written even though the binary is missing")
	}

	rememberZhdataUnitWarning(tag, warning)
	defer rememberZhdataUnitWarning(tag, "")
	warnings := zhdataWarnings(&ZhdataHealth{Tag: "NODE-ZH", AdminAddr: "127.0.0.1:6070", Reachable: true})
	found := false
	for _, w := range warnings {
		if w.Code == "unit" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unit problem never reached the inbound card warnings: %+v", warnings)
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
