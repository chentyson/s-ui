package service

// The panel now owns the zhdata *binary* as well as its launch parameters (ZY11).
//
// ZY10 gave the panel the unit file but still left the core itself outside it:
// the operator copied zhdata-linux-amd64 to /opt/zhdata by hand, and updating
// it meant ssh, stop, cp, start -- with the panel's ExecStart pointing at a path
// only that ritual knew about. Nodes drifted (C455 ran 0.15.4 for weeks while
// releases shipped), and a panel built for a fresh node had no core to point at
// at all.
//
// So the core lives inside the panel's own deployment now:
// <dir of the s-ui binary>/zhdata/zhdata(.exe), downloaded from the public
// GitHub release, checksum-verified, installed atomically, and restarted into
// the units the panel wrote (service/zhdata_unit.go). Everything a package or a
// test wants to move -- the release URL, the install dir, the probe, the
// download timeout -- is a variable, not a constant.

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alireza0/s-ui/database"
	"github.com/alireza0/s-ui/database/model"
	"github.com/alireza0/s-ui/logger"
)

// Everything the panel knows about the zhdata core on this node, as the
// frontend sees it (GET api/zhdataCore).
type ZhdataCoreStatus struct {
	Installed bool `json:"installed"`
	// Path is the binary the units would launch; empty when nothing was found.
	Path string `json:"path"`
	// External marks a core the panel found but does not own: the legacy
	// /opt/zhdata install or a SUI_ZHDATA_PATH override. The UI turns that into
	// a "migrate me" hint, because updating from the panel writes the managed
	// path and the units switch over.
	External bool `json:"external"`
	// CurrentVersion is the version the binary reports, "unknown" when it
	// exists but does not answer parseably, and "" when nothing is installed.
	CurrentVersion string `json:"current_version"`
	// LatestVersion is null when the release lookup failed -- the reason is in
	// Error, which is also where a download or install failure lands.
	LatestVersion *string `json:"latest_version"`
	// NeedUpdate is the one flag the UI needs: nothing installed, an
	// unparseable version, or a version older than the release.
	NeedUpdate bool `json:"need_update"`
	// DownloadAsset is the release asset the panel would fetch for this node
	// (zhdata-<os>-<arch>.zip), "" when this platform has no asset.
	DownloadAsset string   `json:"download_asset"`
	Units         []string `json:"units,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// ZhdataCoreUpdateResult is what POST api/zhdataCore/update answers on success.
type ZhdataCoreUpdateResult struct {
	From      string   `json:"from"`
	To        string   `json:"to"`
	Path      string   `json:"path"`
	External  bool     `json:"external"`
	Restarted []string `json:"restarted"`
}

// Sources and knobs. Variables, not constants, so tests can point them at an
// httptest server and a temp tree.
var (
	zhdataReleaseAPI          = "https://api.github.com/repos/chentyson/zhdata/releases/latest"
	zhdataCoreDirOverride     = ""
	zhdataLegacyBinary        = "/opt/zhdata/zhdata-tunnel-linux-amd64"
	zhdataReleaseAPITimeout   = 15 * time.Second
	zhdataDownloadTimeout     = 120 * time.Second
	zhdataVersionProbeTimeout = 10 * time.Second
	zhdataReleaseCacheTTL     = 10 * time.Minute
	// zhdataTargetOS/zhdataTargetArch pick the release asset. They default to
	// the running platform; a test sets them to pin the asset name.
	zhdataTargetOS   = runtime.GOOS
	zhdataTargetArch = runtime.GOARCH
)

// zhdataCoreEnvVar lets an operator keep the core wherever they like. It wins
// over the panel-managed path, and the panel then refuses to update it.
const zhdataCoreEnvVar = "SUI_ZHDATA_PATH"

const zhdataUnknownVersion = "unknown"

// zhdataCoreLocation is the resolved answer to "which binary do the units run".
type zhdataCoreLocation struct {
	Path      string
	External  bool
	Installed bool
}

// zhdataDeployDir is the folder the running s-ui binary sits in -- the same rule
// config.GetDBFolderPath uses, so "the panel's own directory" means one thing.
func zhdataDeployDir() string {
	dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
	if err != nil {
		return "."
	}
	return dir
}

// zhdataManagedDir is where the panel keeps its core.
func zhdataManagedDir() string {
	if zhdataCoreDirOverride != "" {
		return zhdataCoreDirOverride
	}
	return filepath.Join(zhdataDeployDir(), "zhdata")
}

// zhdataBinaryName is the file name the core ships under on a platform.
func zhdataBinaryName(goos string) string {
	if goos == "windows" {
		return "zhdata.exe"
	}
	return "zhdata"
}

// zhdataManagedBinaryPath is the panel-owned binary for the target platform.
func zhdataManagedBinaryPath() string {
	return filepath.Join(zhdataManagedDir(), zhdataBinaryName(zhdataTargetOS))
}

func zhdataFileExists(path string) bool {
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// resolveZhdataCore decides which binary the units launch, in priority order:
// SUI_ZHDATA_PATH, the panel-managed path, the ZY10-era /opt/zhdata install.
// The last two differ only in whether the panel owns the file: a node that still
// has the old install keeps working (external=true) until someone updates from
// the panel, which writes the managed path and switches the units over.
func resolveZhdataCore() zhdataCoreLocation {
	if env := strings.TrimSpace(os.Getenv(zhdataCoreEnvVar)); env != "" {
		return zhdataCoreLocation{Path: env, External: true, Installed: zhdataFileExists(env)}
	}
	if path := zhdataManagedBinaryPath(); zhdataFileExists(path) {
		return zhdataCoreLocation{Path: path, Installed: true}
	}
	if zhdataFileExists(zhdataLegacyBinary) {
		return zhdataCoreLocation{Path: zhdataLegacyBinary, External: true, Installed: true}
	}
	return zhdataCoreLocation{}
}

// --- version of the installed core ---

// zhdataVersionEntry caches a probe result against the file's identity, so a
// replaced binary is re-probed without a timer and the panel's poll does not
// fork a process every 10 seconds.
type zhdataVersionEntry struct {
	identity string
	version  string
}

var (
	zhdataVersionMu    sync.Mutex
	zhdataVersionCache = map[string]zhdataVersionEntry{}
)

// zhdataVersionProbe is the only place that execs the core for its version.
var zhdataVersionProbe = func(path string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), zhdataVersionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, path, "-version").CombinedOutput()
	return string(out), err
}

// zhdataVersionOutputRe matches the whole version token of `-version` output,
// which is "zhdata 0.15.8" (cmd/zhdata/main.go prints zhdata.Version verbatim).
var zhdataVersionOutputRe = regexp.MustCompile(`^v?\d+(\.\d+)+$`)

// parseZhdataVersionOutput pulls X.Y.Z out of `-version` output. Anything that
// does not look like a version is "unknown", not a guess.
func parseZhdataVersionOutput(output string) string {
	for _, field := range strings.Fields(output) {
		field = strings.Trim(field, "()[],\"'")
		if zhdataVersionOutputRe.MatchString(field) {
			return normalizeZhdataVersion(field)
		}
	}
	return zhdataUnknownVersion
}

// normalizeZhdataVersion strips the "v" a release tag carries so a tag and the
// version a binary prints compare equal.
func normalizeZhdataVersion(version string) string {
	version = strings.TrimSpace(version)
	version = strings.TrimPrefix(version, "v")
	version = strings.TrimPrefix(version, "V")
	return strings.TrimSpace(version)
}

// compareZhdataVersions orders two dotted versions numerically; missing parts
// count as 0, non-numeric parts as their digits only.
func compareZhdataVersions(a, b string) int {
	parts := func(v string) []int {
		fields := strings.Split(normalizeZhdataVersion(v), ".")
		out := make([]int, 0, len(fields))
		for _, field := range fields {
			digits := strings.TrimLeft(field, "0123456789")
			numeric := field[:len(field)-len(digits)]
			n, err := strconv.Atoi(numeric)
			if err != nil {
				n = 0
			}
			out = append(out, n)
		}
		return out
	}
	x, y := parts(a), parts(b)
	for i := 0; i < len(x) || i < len(y); i++ {
		var l, r int
		if i < len(x) {
			l = x[i]
		}
		if i < len(y) {
			r = y[i]
		}
		if l != r {
			if l < r {
				return -1
			}
			return 1
		}
	}
	return 0
}

// zhdataCoreVersion asks the binary for its version, through the cache.
func zhdataCoreVersion(path string) string {
	if path == "" {
		return ""
	}
	info, err := os.Stat(path)
	if err != nil {
		return zhdataUnknownVersion
	}
	identity := fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
	key, err := filepath.Abs(path)
	if err != nil {
		key = path
	}

	zhdataVersionMu.Lock()
	cached, ok := zhdataVersionCache[key]
	zhdataVersionMu.Unlock()
	if ok && cached.identity == identity {
		return cached.version
	}

	output, probeErr := zhdataVersionProbe(path)
	version := parseZhdataVersionOutput(output)
	if probeErr != nil && version == zhdataUnknownVersion {
		logger.Warning("zhdata core: ", path, " -version: ", probeErr)
	}
	zhdataVersionMu.Lock()
	zhdataVersionCache[key] = zhdataVersionEntry{identity: identity, version: version}
	zhdataVersionMu.Unlock()
	return version
}

// invalidateZhdataVersionCache drops every remembered version. An install
// replaces the binary, and the panel's next poll has to show the new version
// immediately rather than after the UI learns to distrust the cache.
func invalidateZhdataVersionCache() {
	zhdataVersionMu.Lock()
	zhdataVersionCache = map[string]zhdataVersionEntry{}
	zhdataVersionMu.Unlock()
}

// --- release lookup ---

type zhdataReleaseAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type zhdataReleaseResponse struct {
	TagName string               `json:"tag_name"`
	Assets  []zhdataReleaseAsset `json:"assets"`
}

// zhdataRelease is the release narrowed to this node: the asset to fetch, its
// checksum, and the version it installs.
type zhdataRelease struct {
	Version   string
	AssetName string
	AssetURL  string
	Digest    string
}

// zhdataPlatformSuffix maps a Go platform to the release asset naming
// (linux->linux, darwin->macos; amd64->64).
func zhdataPlatformSuffix(goos, goarch string) (string, bool) {
	osPart := ""
	switch goos {
	case "linux":
		osPart = "linux"
	case "windows":
		osPart = "windows"
	case "darwin":
		osPart = "macos"
	default:
		return "", false
	}
	archPart := ""
	switch goarch {
	case "amd64":
		archPart = "64"
	case "arm64":
		archPart = "arm64"
	default:
		return "", false
	}
	return osPart + "-" + archPart, true
}

// zhdataAssetNameFor is the asset name this platform downloads.
func zhdataAssetNameFor(goos, goarch string) string {
	suffix, ok := zhdataPlatformSuffix(goos, goarch)
	if !ok {
		return ""
	}
	return "zhdata-" + suffix + ".zip"
}

var (
	zhdataReleaseMu    sync.Mutex
	zhdataReleaseCache *zhdataRelease
	zhdataReleaseAt    time.Time
)

// getZhdataRelease looks up the newest public release of chentyson/zhdata.
// The lookup is cached (GitHub allows 60 unauthenticated requests an hour and
// the UI polls), except right before an update, which wants the truth.
func getZhdataRelease(force bool) (*zhdataRelease, error) {
	zhdataReleaseMu.Lock()
	cached := zhdataReleaseCache
	zhdataReleaseMu.Unlock()
	if cached != nil && !force && time.Since(zhdataReleaseAt) < zhdataReleaseCacheTTL {
		return cached, nil
	}

	client := &http.Client{Timeout: zhdataReleaseAPITimeout}
	req, err := http.NewRequest(http.MethodGet, zhdataReleaseAPI, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "s-ui")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("无法访问 %s: %w", zhdataReleaseAPI, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<22))
	if err != nil {
		return nil, fmt.Errorf("读取 %s 失败: %w", zhdataReleaseAPI, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s 返回 HTTP %d", zhdataReleaseAPI, resp.StatusCode)
	}
	var payload zhdataReleaseResponse
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("解析 GitHub release 失败: %v", err)
	}
	if strings.TrimSpace(payload.TagName) == "" {
		return nil, fmt.Errorf("%s 没有 tag_name（响应不是 release？）", zhdataReleaseAPI)
	}

	release := &zhdataRelease{Version: normalizeZhdataVersion(payload.TagName)}
	want := zhdataAssetNameFor(zhdataTargetOS, zhdataTargetArch)
	for _, asset := range payload.Assets {
		if asset.Name == want {
			release.AssetName = asset.Name
			release.AssetURL = asset.URL
			release.Digest = strings.TrimSpace(asset.Digest)
			break
		}
	}
	if want == "" {
		return release, fmt.Errorf("没有 %s/%s 的 zhdata 发行资产", zhdataTargetOS, zhdataTargetArch)
	}
	if release.AssetName == "" {
		return release, fmt.Errorf("最新 release %s 里没有 %s", payload.TagName, want)
	}

	zhdataReleaseMu.Lock()
	zhdataReleaseCache = release
	zhdataReleaseAt = time.Now()
	zhdataReleaseMu.Unlock()
	return release, nil
}

// zhdataDownloadAsset pulls one release asset into memory (the zips are ~5MB).
func zhdataDownloadAsset(url string) ([]byte, error) {
	client := &http.Client{Timeout: zhdataDownloadTimeout}
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "s-ui")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<14))
		return nil, fmt.Errorf("下载 %s 返回 HTTP %d: %s", filepath.Base(url), resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// 64MB is far above any zhdata zip and stops a wrong URL from eating RAM.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("下载 %s 是空文件", filepath.Base(url))
	}
	return body, nil
}

// verifyZhdataDigest checks the asset against GitHub's "digest" field. No
// digest (an API that stopped sending it) is not a failure: the install just
// goes unverified, as it did when the operator copied a tarball by hand.
func verifyZhdataDigest(data []byte, digest string) error {
	digest = strings.TrimSpace(digest)
	if digest == "" {
		return nil
	}
	want := strings.TrimSpace(strings.TrimPrefix(strings.ToLower(digest), "sha256:"))
	if want == "" {
		return fmt.Errorf("digest %q 里没有 sha256 值", digest)
	}
	sum := sha256.Sum256(data)
	if !strings.EqualFold(hex.EncodeToString(sum[:]), want) {
		return fmt.Errorf("sha256 校验失败：期望 %s，实际 %s（下载被截断或资产被替换？）", want, hex.EncodeToString(sum[:]))
	}
	return nil
}

// extractZhdataBinaryFromZip pulls zhdata/zhdata(.exe) out of the release zip.
// The archives carry one top-level directory (the release job's staging dir),
// so the match is by name with that directory allowed in front.
func extractZhdataBinaryFromZip(data []byte, binaryName string) ([]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("打开下载的 zip 失败: %w", err)
	}
	var names []string
	for _, file := range reader.File {
		name := filepath.ToSlash(file.Name)
		names = append(names, name)
		if file.FileInfo().IsDir() {
			continue
		}
		if strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		base := pathBase(name)
		if base != binaryName {
			continue
		}
		if name != binaryName && !strings.HasSuffix(name, "/"+binaryName) {
			continue
		}
		rc, err := file.Open()
		if err != nil {
			return nil, fmt.Errorf("读取 zip 里的 %s 失败: %w", name, err)
		}
		payload, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("解压 %s 失败: %w", name, err)
		}
		if len(payload) == 0 {
			return nil, fmt.Errorf("zip 里的 %s 是空文件", name)
		}
		return payload, nil
	}
	return nil, fmt.Errorf("下载的 zip 里没有 %s（内容：%s）", binaryName, strings.Join(names, ", "))
}

func pathBase(name string) string {
	if i := strings.LastIndex(name, "/"); i >= 0 {
		return name[i+1:]
	}
	return name
}

// installZhdataBinary writes the payload to a temp file next to the target,
// chmods it, backs the old binary up, and renames it into place. Two renames,
// so an interrupted download never leaves a half-written core behind the units.
func installZhdataBinary(payload []byte, target string) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 %s 失败: %w", dir, err)
	}
	temp, err := os.CreateTemp(dir, ".zhdata-*.new")
	if err != nil {
		return fmt.Errorf("在 %s 里建临时文件失败: %w", dir, err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(payload); err != nil {
		temp.Close()
		return fmt.Errorf("写入 %s 失败: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("关闭 %s 失败: %w", tempName, err)
	}
	if err := os.Chmod(tempName, 0o755); err != nil {
		return fmt.Errorf("chmod %s 失败: %w", tempName, err)
	}
	if zhdataFileExists(target) {
		backup := target + ".bak"
		_ = os.Remove(backup)
		if err := os.Rename(target, backup); err != nil {
			return fmt.Errorf("备份旧核心 %s 失败: %w（进程正在使用它？先停掉再更新）", target, err)
		}
	}
	if err := os.Rename(tempName, target); err != nil {
		return fmt.Errorf("替换 %s 失败: %w", target, err)
	}
	return nil
}

// UpdateZhdataCore downloads the newest release, verifies it, installs it in the
// panel's own directory, and restarts the managed units onto it. Synchronous on
// purpose: the UI shows one spinner and one result, and a zip is a few MB.
func UpdateZhdataCore() (*ZhdataCoreUpdateResult, error) {
	inbounds, err := zhdataInboundsOf(database.GetDB())
	if err != nil {
		// The core still gets installed; the units just are not told about it
		// until the store answers again.
		logger.Warning("zhdata core: 读取 zhdata 入站失败: ", err)
		inbounds = nil
	}
	return updateZhdataCore(inbounds)
}

func updateZhdataCore(inbounds []*model.Inbound) (*ZhdataCoreUpdateResult, error) {
	if env := strings.TrimSpace(os.Getenv(zhdataCoreEnvVar)); env != "" {
		return nil, fmt.Errorf("%s=%s 指向面板外的核心，面板不更新它：取消该环境变量后重试（面板会装到 %s）",
			zhdataCoreEnvVar, env, zhdataManagedBinaryPath())
	}
	location := resolveZhdataCore()
	from := ""
	if location.Installed {
		from = zhdataCoreVersion(location.Path)
	}

	release, err := getZhdataRelease(true)
	if err != nil {
		return nil, err
	}
	archive, err := zhdataDownloadAsset(release.AssetURL)
	if err != nil {
		return nil, err
	}
	if err := verifyZhdataDigest(archive, release.Digest); err != nil {
		return nil, err
	}
	binaryName := zhdataBinaryName(zhdataTargetOS)
	payload, err := extractZhdataBinaryFromZip(archive, binaryName)
	if err != nil {
		return nil, err
	}

	target := zhdataManagedBinaryPath()
	if err := installZhdataBinary(payload, target); err != nil {
		return nil, err
	}
	logger.Info("zhdata core: 已安装 ", target, "（", location.Path, " -> ", release.Version, "）")
	invalidateZhdataVersionCache()

	// The launch line may have moved (a legacy /opt/zhdata install becomes the
	// managed path), and either way the running process is the old binary, so
	// the units are re-rendered and restarted even when the arguments did not
	// change. forceRestart is what ZY10's "ExecStart 没变就不动进程" rule lacks.
	restarted, warnings := syncAfterCoreInstall(inbounds)
	for tag, warning := range warnings {
		if warning != "" {
			logger.Warning("zhdata core: 入站 ", tag, ": ", warning)
		}
	}

	result := &ZhdataCoreUpdateResult{
		From:      from,
		To:        release.Version,
		Path:      target,
		External:  false,
		Restarted: restarted,
	}
	if result.Restarted == nil {
		result.Restarted = []string{}
	}
	return result, nil
}

// syncAfterCoreInstall restarts every managed unit onto the freshly installed
// core and returns the unit names it restarted.
func syncAfterCoreInstall(inbounds []*model.Inbound) ([]string, map[string]string) {
	result, err := syncZhdataUnitsFor(inbounds, true)
	if err != nil {
		logger.Warning("zhdata core: 更新后同步 systemd 单元失败: ", err)
		return nil, nil
	}
	return result.restarted, result.warnings
}

// GetZhdataCore is the read-only core status the UI renders, and the check a
// frontend makes before offering to create a zhdata inbound.
func GetZhdataCore() *ZhdataCoreStatus {
	status := &ZhdataCoreStatus{}
	location := resolveZhdataCore()
	status.Path = location.Path
	status.Installed = location.Installed
	status.External = location.External
	if location.Installed {
		status.CurrentVersion = zhdataCoreVersion(location.Path)
	}
	status.DownloadAsset = zhdataAssetNameFor(zhdataTargetOS, zhdataTargetArch)
	if inbounds, err := zhdataInboundsOf(database.GetDB()); err == nil {
		for _, unit := range zhdataManagedUnits(inbounds) {
			status.Units = append(status.Units, unit.unitName)
		}
	}

	release, err := getZhdataRelease(false)
	if err != nil {
		status.Error = err.Error()
		status.NeedUpdate = !location.Installed
		return status
	}
	latest := release.Version
	status.LatestVersion = &latest

	switch {
	case !location.Installed:
		status.NeedUpdate = true
	case status.CurrentVersion == zhdataUnknownVersion || status.CurrentVersion == "":
		// No idea what is running: offer the update rather than claim it is
		// current.
		status.NeedUpdate = true
	default:
		status.NeedUpdate = compareZhdataVersions(status.CurrentVersion, latest) < 0
	}
	return status
}
