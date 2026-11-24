package service

// The panel now ships the zhdata core itself: it asks GitHub for the newest
// release, downloads the asset for this platform, verifies it, unzips
// zhdata/zhdata(.exe) into its own directory and restarts the units onto it.
// Everything below runs against an httptest GitHub and a temp tree -- the
// network and /etc are not part of the test.

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useZhdataCoreEnv points every path and cache the core code touches at a fresh
// temp state, and pins the platform so the asset name is deterministic.
func useZhdataCoreEnv(t *testing.T, managedDir, legacy, goos, goarch string) {
	t.Helper()
	oldDir, oldLegacy := zhdataCoreDirOverride, zhdataLegacyBinary
	oldAPI, oldOS, oldArch := zhdataReleaseAPI, zhdataTargetOS, zhdataTargetArch
	oldProbe := zhdataVersionProbe
	zhdataCoreDirOverride, zhdataLegacyBinary = managedDir, legacy
	zhdataReleaseAPI, zhdataTargetOS, zhdataTargetArch = "http://127.0.0.1:1/not-used", goos, goarch

	reset := func() {
		invalidateZhdataVersionCache()
		zhdataReleaseMu.Lock()
		zhdataReleaseCache, zhdataReleaseAt = nil, time.Time{}
		zhdataReleaseMu.Unlock()
	}
	reset()
	t.Cleanup(func() {
		zhdataCoreDirOverride, zhdataLegacyBinary = oldDir, oldLegacy
		zhdataReleaseAPI, zhdataTargetOS, zhdataTargetArch = oldAPI, oldOS, oldArch
		zhdataVersionProbe = oldProbe
		reset()
	})
	t.Setenv(zhdataCoreEnvVar, "")
}

// stubZhdataVersionProbe replaces `-version` with a read of the file: every
// fake binary in these tests carries its own version as its content. The
// returned counter says how often the panel asked, which is what pins the cache.
func stubZhdataVersionProbe(t *testing.T) *int {
	t.Helper()
	calls := 0
	zhdataVersionProbe = func(path string) (string, error) {
		calls++
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		return "zhdata " + strings.TrimSpace(string(data)) + "\n", nil
	}
	return &calls
}

// fakeGitHub serves the release JSON at /latest and the asset at /asset.zip,
// the way chentyson/zhdata's releases do (including the digest field).
func fakeGitHub(t *testing.T, tag, assetName string, zipBody []byte, digest string, downloadStatus int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	var server *httptest.Server
	mux.HandleFunc("/latest", func(w http.ResponseWriter, r *http.Request) {
		asset := map[string]interface{}{
			"name":                 assetName,
			"browser_download_url": server.URL + "/asset.zip",
		}
		if digest != "" {
			asset["digest"] = digest
		}
		body, _ := json.Marshal(map[string]interface{}{
			"tag_name": tag,
			"assets":   []interface{}{asset},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	mux.HandleFunc("/asset.zip", func(w http.ResponseWriter, r *http.Request) {
		if downloadStatus != 0 && downloadStatus != http.StatusOK {
			w.WriteHeader(downloadStatus)
			return
		}
		_, _ = w.Write(zipBody)
	})
	server = httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}

func zhdataZipFile(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	for name, content := range entries {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sha256Of(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestZhdataAssetNameFor(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "zhdata-linux-64.zip"},
		{"linux", "arm64", "zhdata-linux-arm64.zip"},
		{"windows", "amd64", "zhdata-windows-64.zip"},
		{"windows", "arm64", "zhdata-windows-arm64.zip"},
		{"darwin", "amd64", "zhdata-macos-64.zip"},
		{"darwin", "arm64", "zhdata-macos-arm64.zip"},
		{"linux", "386", ""},
		{"freebsd", "amd64", ""},
	}
	for _, c := range cases {
		if got := zhdataAssetNameFor(c.goos, c.goarch); got != c.want {
			t.Errorf("zhdataAssetNameFor(%s,%s) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestParseZhdataVersionOutput(t *testing.T) {
	cases := []struct {
		output, want string
	}{
		{"zhdata 0.15.8\n", "0.15.8"},
		{"zhdata v0.16.0", "0.16.0"},
		{"  zhdata\t0.15.8 ", "0.15.8"},
		{"zhdata 0.15\n", "0.15"},
		{"zhenhanvn update service parses X.Y.Z out of this line", "unknown"},
		{"", "unknown"},
		{"zhdata-tunnel-linux-amd64: command not found\n", "unknown"},
		{"flag provided but not defined: -version\n", "unknown"},
	}
	for _, c := range cases {
		if got := parseZhdataVersionOutput(c.output); got != c.want {
			t.Errorf("parseZhdataVersionOutput(%q) = %q, want %q", c.output, got, c.want)
		}
	}
}

func TestCompareZhdataVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.15.8", "0.15.8", 0},
		{"v0.15.8", "0.15.8", 0},
		{"0.15.7", "0.15.8", -1},
		{"0.15.10", "0.15.9", 1},
		{"0.16.0", "0.15.99", 1},
		{"0.15", "0.15.0", 0},
		{"1.0.0", "1.0.0.1", -1},
	}
	for _, c := range cases {
		if got := compareZhdataVersions(c.a, c.b); got != c.want {
			t.Errorf("compareZhdataVersions(%q,%q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestVerifyZhdataDigest(t *testing.T) {
	data := []byte("the core")
	good := sha256Of(data)
	if err := verifyZhdataDigest(data, ""); err != nil {
		t.Fatalf("no digest must not fail the install: %v", err)
	}
	if err := verifyZhdataDigest(data, good); err != nil {
		t.Fatalf("good digest rejected: %v", err)
	}
	if err := verifyZhdataDigest(data, strings.ToUpper(good)); err != nil {
		t.Fatalf("upper-case hex digest rejected: %v", err)
	}
	if err := verifyZhdataDigest(data, "sha256:0000"); err == nil {
		t.Fatal("a truncated digest slipped through")
	}
	if err := verifyZhdataDigest(append(data, '!'), good); err == nil {
		t.Fatal("a modified payload slipped through")
	}
	if err := verifyZhdataDigest(data, "md5:abc"); err == nil {
		t.Fatal("a digest with no sha256 value slipped through")
	}
}

func TestExtractZhdataBinaryFromZip(t *testing.T) {
	nested := zhdataZipFile(t, map[string]string{
		"zhdata/zhdata":     "binary",
		"zhdata/README.md":  "readme",
		"__MACOSX/._zhdata": "junk",
	})
	payload, err := extractZhdataBinaryFromZip(nested, "zhdata")
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "binary" {
		t.Fatalf("payload = %q", payload)
	}

	windows := zhdataZipFile(t, map[string]string{"zhdata/zhdata.exe": "win-binary"})
	payload, err = extractZhdataBinaryFromZip(windows, "zhdata.exe")
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != "win-binary" {
		t.Fatalf("windows payload = %q", payload)
	}

	if _, err := extractZhdataBinaryFromZip(nested, "zhdata.exe"); err == nil {
		t.Fatal("a zip without the wanted binary read as fine")
	}
	if _, err := extractZhdataBinaryFromZip([]byte("not a zip"), "zhdata"); err == nil {
		t.Fatal("garbage read as a zip")
	}
}

func TestResolveZhdataCorePriority(t *testing.T) {
	managedDir := t.TempDir()
	legacy := filepath.Join(t.TempDir(), "zhdata-tunnel-linux-amd64")
	useZhdataCoreEnv(t, managedDir, legacy, "linux", "amd64")

	if location := resolveZhdataCore(); location.Installed {
		t.Fatalf("nothing installed yet: %+v", location)
	}

	if err := os.WriteFile(legacy, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	location := resolveZhdataCore()
	if !location.Installed || !location.External || location.Path != legacy {
		t.Fatalf("legacy install = %+v, want the old path flagged external", location)
	}

	managed := zhdataManagedBinaryPath()
	if err := os.WriteFile(managed, []byte("new"), 0o755); err != nil {
		t.Fatal(err)
	}
	location = resolveZhdataCore()
	if location.Path != managed || location.External {
		t.Fatalf("managed install = %+v, want %s (panel-owned)", location, managed)
	}

	envPath := filepath.Join(t.TempDir(), "custom-zhdata")
	if err := os.WriteFile(envPath, []byte("env"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(zhdataCoreEnvVar, envPath)
	location = resolveZhdataCore()
	if location.Path != envPath || !location.External || !location.Installed {
		t.Fatalf("SUI_ZHDATA_PATH = %+v, want it to win over both installs", location)
	}

	dangling := filepath.Join(t.TempDir(), "gone")
	t.Setenv(zhdataCoreEnvVar, dangling)
	location = resolveZhdataCore()
	if location.Installed || location.Path != dangling {
		t.Fatalf("a dangling SUI_ZHDATA_PATH = %+v, want it reported as configured but missing", location)
	}
}

// The status endpoint is what the UI renders, and what the frontend checks
// before it offers a new zhdata inbound.
func TestGetZhdataCoreStatusAgainstFakeGitHub(t *testing.T) {
	managedDir := t.TempDir()
	useZhdataCoreEnv(t, managedDir, filepath.Join(t.TempDir(), "no-legacy"), "linux", "amd64")
	stubZhdataVersionProbe(t)

	archive := zhdataZipFile(t, map[string]string{"zhdata/zhdata": "0.15.8"})
	server := fakeGitHub(t, "v0.15.8", "zhdata-linux-64.zip", archive, sha256Of(archive), http.StatusOK)
	zhdataReleaseAPI = server.URL + "/latest"

	// Nothing installed yet: the UI gets "install me", not an error.
	status := GetZhdataCore()
	if status.Installed || status.CurrentVersion != "" || !status.NeedUpdate {
		t.Fatalf("empty node = %+v", status)
	}
	if status.LatestVersion == nil || *status.LatestVersion != "0.15.8" {
		t.Fatalf("latest = %v, want 0.15.8", status.LatestVersion)
	}
	if status.DownloadAsset != "zhdata-linux-64.zip" {
		t.Fatalf("asset = %q", status.DownloadAsset)
	}
	if status.Error != "" {
		t.Fatalf("error = %q on a healthy lookup", status.Error)
	}

	managed := zhdataManagedBinaryPath()
	if err := os.WriteFile(managed, []byte("0.15.8"), 0o755); err != nil {
		t.Fatal(err)
	}
	status = GetZhdataCore()
	if !status.Installed || status.External || status.CurrentVersion != "0.15.8" || status.NeedUpdate {
		t.Fatalf("up to date node = %+v", status)
	}

	// An older core than the release: an update is warranted.
	if err := os.WriteFile(managed, []byte("0.15.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	status = GetZhdataCore()
	if status.CurrentVersion != "0.15.4" || !status.NeedUpdate {
		t.Fatalf("outdated node = %+v", status)
	}

	// A binary that does not answer with a version is "unknown", and the panel
	// offers the update rather than claiming it is current.
	if err := os.WriteFile(managed, []byte("say-nothing"), 0o755); err != nil {
		t.Fatal(err)
	}
	status = GetZhdataCore()
	if status.CurrentVersion != "unknown" || !status.NeedUpdate {
		t.Fatalf("unparseable node = %+v", status)
	}

	// A node still on the legacy install reports external=true so the UI can
	// suggest the migration instead of pretending the panel owns that file.
	legacy := filepath.Join(t.TempDir(), "zhdata-tunnel-linux-amd64")
	zhdataLegacyBinary = legacy
	if err := os.WriteFile(legacy, []byte("0.15.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(managed); err != nil {
		t.Fatal(err)
	}
	status = GetZhdataCore()
	if !status.Installed || !status.External || status.Path != legacy {
		t.Fatalf("legacy node = %+v", status)
	}
}

// A GitHub outage leaves the installed core fully described, latest_version
// null, and the reason in error.
func TestGetZhdataCoreWithoutGitHub(t *testing.T) {
	managedDir := t.TempDir()
	useZhdataCoreEnv(t, managedDir, filepath.Join(t.TempDir(), "no-legacy"), "linux", "amd64")
	stubZhdataVersionProbe(t)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	zhdataReleaseAPI = server.URL + "/latest"

	managed := zhdataManagedBinaryPath()
	if err := os.WriteFile(managed, []byte("0.15.8"), 0o755); err != nil {
		t.Fatal(err)
	}
	status := GetZhdataCore()
	if !status.Installed || status.CurrentVersion != "0.15.8" {
		t.Fatalf("status = %+v", status)
	}
	if status.LatestVersion != nil {
		t.Fatalf("latest = %v, want null", *status.LatestVersion)
	}
	if status.Error == "" || !strings.Contains(status.Error, "500") {
		t.Fatalf("error = %q, want the HTTP reason", status.Error)
	}
	if status.NeedUpdate {
		t.Fatal("an unreachable GitHub must not claim an update is needed")
	}
}

// The whole update path: download, checksum, unzip, replace, restart the units.
func TestUpdateZhdataCoreInstallsAndRestartsUnits(t *testing.T) {
	managedDir := t.TempDir()
	systemdDir := t.TempDir()
	useZhdataCoreEnv(t, managedDir, filepath.Join(t.TempDir(), "no-legacy"), "linux", "amd64")
	useZhdataUnitPaths(t, systemdDir)
	calls := useFakeSystemd(t)
	stubZhdataVersionProbe(t)

	// A node on the ZY10-era install, with one zhdata inbound.
	legacy := filepath.Join(t.TempDir(), "zhdata-tunnel-linux-amd64")
	zhdataLegacyBinary = legacy
	if err := os.WriteFile(legacy, []byte("0.15.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions})

	archive := zhdataZipFile(t, map[string]string{
		"zhdata/zhdata":    "0.15.8",
		"zhdata/README.md": "readme",
	})
	server := fakeGitHub(t, "v0.15.8", "zhdata-linux-64.zip", archive, sha256Of(archive), http.StatusOK)
	zhdataReleaseAPI = server.URL + "/latest"

	result, err := updateZhdataCore(rows)
	if err != nil {
		t.Fatal(err)
	}
	if result.From != "0.15.4" || result.To != "0.15.8" {
		t.Fatalf("from/to = %s -> %s", result.From, result.To)
	}
	managed := zhdataManagedBinaryPath()
	if result.Path != managed || result.External {
		t.Fatalf("result path = %+v, want the panel-managed %s", result, managed)
	}
	if strings.Join(result.Restarted, ",") != "zhdata.service" {
		t.Fatalf("restarted = %v", result.Restarted)
	}

	data, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "0.15.8" {
		t.Fatalf("installed core = %q", data)
	}
	// The old core is kept beside it: an update that does not boot is a rename
	// away from being undone.
	if backup, err := os.ReadFile(managed + ".bak"); err == nil && string(backup) != "" {
		t.Errorf("unexpected backup in a fresh install: %q", backup)
	}

	// The units moved onto the new binary, and the panel restarted them.
	unit, err := os.ReadFile(filepath.Join(systemdDir, "zhdata.service"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(unit), managed) {
		t.Fatalf("unit still points at the legacy core:\n%s", unit)
	}
	joined := strings.Join(*calls, "|")
	if !strings.Contains(joined, "enable zhdata.service") || !strings.Contains(joined, "restart zhdata.service") {
		t.Fatalf("systemctl calls = %v", *calls)
	}

	// The status right after the update shows the new version: the version cache
	// was dropped, so a UI refresh sees it immediately.
	status := GetZhdataCore()
	if status.CurrentVersion != "0.15.8" || status.NeedUpdate || status.External {
		t.Fatalf("status after update = %+v", status)
	}

	// Updating again installs over the previous managed core and keeps a backup.
	archive2 := zhdataZipFile(t, map[string]string{"zhdata/zhdata": "0.16.0"})
	server2 := fakeGitHub(t, "v0.16.0", "zhdata-linux-64.zip", archive2, sha256Of(archive2), http.StatusOK)
	zhdataReleaseAPI = server2.URL + "/latest"
	if _, err := updateZhdataCore(rows); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(managed + ".bak")
	if err != nil {
		t.Fatalf("the replaced core was not backed up: %v", err)
	}
	if string(backup) != "0.15.8" {
		t.Fatalf("backup = %q, want the core it replaced", backup)
	}
	if data, err := os.ReadFile(managed); err != nil || string(data) != "0.16.0" {
		t.Fatalf("core after the second update = %q (%v)", data, err)
	}
}

// A wrong checksum must leave the running core exactly where it was: the units
// are not restarted onto a half-trusted download.
func TestUpdateZhdataCoreDigestMismatchKeepsTheOldCore(t *testing.T) {
	managedDir := t.TempDir()
	systemdDir := t.TempDir()
	useZhdataCoreEnv(t, managedDir, filepath.Join(t.TempDir(), "no-legacy"), "linux", "amd64")
	useZhdataUnitPaths(t, systemdDir)
	calls := useFakeSystemd(t)
	stubZhdataVersionProbe(t)

	managed := zhdataManagedBinaryPath()
	if err := os.WriteFile(managed, []byte("0.15.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	rows := zhdataRows([2]string{"NODE-ZH", plainZhdataOptions})

	archive := zhdataZipFile(t, map[string]string{"zhdata/zhdata": "0.15.8"})
	server := fakeGitHub(t, "v0.15.8", "zhdata-linux-64.zip", archive, "sha256:deadbeef", http.StatusOK)
	zhdataReleaseAPI = server.URL + "/latest"

	if _, err := updateZhdataCore(rows); err == nil {
		t.Fatal("a bad checksum updated the core anyway")
	} else if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("error = %v, want the checksum sentence", err)
	}
	data, err := os.ReadFile(managed)
	if err != nil || string(data) != "0.15.4" {
		t.Fatalf("core after a failed update = %q (%v)", data, err)
	}
	for _, call := range *calls {
		if strings.HasPrefix(call, "restart") {
			t.Fatalf("a failed update restarted the process: %v", *calls)
		}
	}
}

// SUI_ZHDATA_PATH means "this core is mine, not the panel's": the update refuses
// rather than installing a binary nothing launches.
func TestUpdateZhdataCoreRefusesAnEnvOverride(t *testing.T) {
	managedDir := t.TempDir()
	useZhdataCoreEnv(t, managedDir, filepath.Join(t.TempDir(), "no-legacy"), "linux", "amd64")
	useFakeSystemd(t)

	envPath := filepath.Join(t.TempDir(), "custom-zhdata")
	if err := os.WriteFile(envPath, []byte("0.15.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(zhdataCoreEnvVar, envPath)

	server := fakeGitHub(t, "v0.15.8", "zhdata-linux-64.zip", zhdataZipFile(t, map[string]string{"zhdata/zhdata": "0.15.8"}), "", http.StatusOK)
	zhdataReleaseAPI = server.URL + "/latest"

	if _, err := UpdateZhdataCore(); err == nil || !strings.Contains(err.Error(), zhdataCoreEnvVar) {
		t.Fatalf("error = %v, want it to name %s", err, zhdataCoreEnvVar)
	}
	if _, err := os.Stat(zhdataManagedBinaryPath()); !os.IsNotExist(err) {
		t.Fatal("the panel installed a core nothing was going to launch")
	}
}

// The version probe is cached against the file, so the panel's 10s poll does not
// fork a process every time -- and a replaced binary is re-probed at once.
func TestZhdataCoreVersionProbeIsCached(t *testing.T) {
	managedDir := t.TempDir()
	useZhdataCoreEnv(t, managedDir, filepath.Join(t.TempDir(), "no-legacy"), "linux", "amd64")
	calls := stubZhdataVersionProbe(t)

	managed := zhdataManagedBinaryPath()
	if err := os.WriteFile(managed, []byte("0.15.4"), 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if got := zhdataCoreVersion(managed); got != "0.15.4" {
			t.Fatalf("version = %q", got)
		}
	}
	if *calls != 1 {
		t.Fatalf("probes = %d, want the cache to answer 3 reads with 1 probe", *calls)
	}

	if err := os.WriteFile(managed, []byte("0.15.8"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := zhdataCoreVersion(managed); got != "0.15.8" {
		t.Fatalf("version after the file changed = %q", got)
	}
	if *calls != 2 {
		t.Fatalf("probes = %d, want a replaced binary re-probed once", *calls)
	}

	invalidateZhdataVersionCache()
	if got := zhdataCoreVersion(managed); got != "0.15.8" {
		t.Fatalf("version after invalidation = %q", got)
	}
	if *calls != 3 {
		t.Fatalf("probes = %d, want invalidation to force a probe", *calls)
	}
}
