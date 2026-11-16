// Package upgrade implements privilege-free self-inspection, verified GitHub Release
// downloads, atomic binary replacement with .prev backup/rollback, and cross-platform
// process handover for agent-bot.
package upgrade

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/rshun/agent-bot/internal/engine"
)

const (
	// DefaultRepo is the official GitHub repository from which release binaries are fetched.
	DefaultRepo = "rshun/agent-bot"
	// DefaultAPIBase is the GitHub REST API base URL.
	DefaultAPIBase = "https://api.github.com"
	// ExitCodeUpgrade is returned when falling back to systemd RestartForceExitStatus=42.
	ExitCodeUpgrade = 42

	maxSumsBytes   = 256 << 10 // 256 KiB
	maxBinaryBytes = 200 << 20 // 200 MiB
)

// ExePaths holds the startup snapshot of the running agent-bot executable.
type ExePaths struct {
	// LaunchPath is the absolute path reported by os.Executable() at startup (may be a symlink).
	LaunchPath string
	// TargetPath is the symlink-resolved physical file path on disk.
	TargetPath string
}

// SnapshotExecutable captures LaunchPath and TargetPath immediately at process startup,
// avoiding late `/proc/self/exe (deleted)` resolution issues on Linux.
func SnapshotExecutable() ExePaths {
	exe, err := os.Executable()
	if err != nil || strings.TrimSpace(exe) == "" {
		return ExePaths{}
	}
	exe = strings.TrimSuffix(exe, " (deleted)")
	launch := exe
	if abs, absErr := filepath.Abs(exe); absErr == nil && abs != "" {
		launch = abs
	}
	target := launch
	if real, symErr := filepath.EvalSymlinks(launch); symErr == nil && real != "" {
		target = strings.TrimSuffix(real, " (deleted)")
	}
	return ExePaths{
		LaunchPath: filepath.Clean(launch),
		TargetPath: filepath.Clean(target),
	}
}

// ReleaseInfo describes a verified latest GitHub Release and its matching platform assets.
type ReleaseInfo struct {
	TagName   string
	HTMLURL   string
	AssetName string
	AssetURL  string
	SumsURL   string
}

// BotStatus summarizes the current version, latest release, and write-permission state of agent-bot.
type BotStatus struct {
	Current         string
	Latest          string
	UpdateAvailable bool
	IsDev           bool
	LaunchPath      string
	TargetPath      string
	Writable        bool
	WriteHint       string
	ReleaseURL      string
	Release         ReleaseInfo
	Error           string
}

// StagedBotUpgrade holds a verified new agent-bot binary staged in TargetPath's directory.
type StagedBotUpgrade struct {
	Changed    bool
	LaunchPath string
	TargetPath string
	StagedPath string
	OldVersion string
	NewVersion string
	ReleaseURL string
}

// Upgrader inspects, downloads, verifies, stages, and commits agent-bot binary updates
// strictly within user-writable directories (zero sudo/root privilege required).
type Upgrader struct {
	CurrentVersion string
	Paths          ExePaths
	GOOS           string
	GOARCH         string
	Repo           string
	APIBase        string
	HTTP           *http.Client
	// AllowInsecureHTTP permits http:// localhost URLs in offline unit tests only.
	AllowInsecureHTTP bool
	// SkipSmokeExec disables running `<staged> --version` when cross-testing foreign OS binaries.
	SkipSmokeExec bool
	// RunCmd overrides subprocess execution in tests; nil runs os/exec with secrets scrubbed.
	RunCmd func(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error)
}

func (u *Upgrader) goos() string {
	if u != nil && strings.TrimSpace(u.GOOS) != "" {
		return strings.ToLower(strings.TrimSpace(u.GOOS))
	}
	return runtime.GOOS
}

func (u *Upgrader) goarch() string {
	if u != nil && strings.TrimSpace(u.GOARCH) != "" {
		return strings.ToLower(strings.TrimSpace(u.GOARCH))
	}
	return runtime.GOARCH
}

func (u *Upgrader) repo() string {
	if u != nil && strings.TrimSpace(u.Repo) != "" {
		return strings.TrimSpace(u.Repo)
	}
	return DefaultRepo
}

func (u *Upgrader) apiBase() string {
	if u != nil && strings.TrimSpace(u.APIBase) != "" {
		return strings.TrimRight(strings.TrimSpace(u.APIBase), "/")
	}
	return DefaultAPIBase
}

func (u *Upgrader) httpClient(timeout time.Duration) *http.Client {
	if u != nil && u.HTTP != nil {
		return u.HTTP
	}
	return &http.Client{Timeout: timeout}
}

func (u *Upgrader) targetPath() string {
	if u == nil {
		return ""
	}
	if p := strings.TrimSpace(u.Paths.TargetPath); p != "" {
		return filepath.Clean(p)
	}
	if p := strings.TrimSpace(u.Paths.LaunchPath); p != "" {
		if real, err := filepath.EvalSymlinks(p); err == nil && real != "" {
			return filepath.Clean(real)
		}
		return filepath.Clean(p)
	}
	snap := SnapshotExecutable()
	return snap.TargetPath
}

func (u *Upgrader) launchPath() string {
	if u == nil {
		return ""
	}
	if p := strings.TrimSpace(u.Paths.LaunchPath); p != "" {
		return filepath.Clean(p)
	}
	return u.targetPath()
}

// AssetNameFor maps GOOS and GOARCH to the release artifact built by .github/workflows/release.yml.
func AssetNameFor(goos, goarch string) (string, error) {
	goos = strings.ToLower(strings.TrimSpace(goos))
	goarch = strings.ToLower(strings.TrimSpace(goarch))
	switch goos + "/" + goarch {
	case "linux/amd64":
		return "agent-bot-linux-amd64", nil
	case "linux/arm64":
		return "agent-bot-linux-arm64", nil
	case "darwin/arm64":
		return "agent-bot-darwin-arm64", nil
	case "darwin/amd64":
		return "agent-bot-darwin-amd64", nil
	case "windows/amd64":
		return "agent-bot-windows-amd64.exe", nil
	default:
		return "", fmt.Errorf("当前系统架构 (%s/%s) 暂无官方预编译发布包，请从源码编译升级", goos, goarch)
	}
}

// IsDevVersion reports whether v is an unversioned local development build.
func IsDevVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v == "" || strings.EqualFold(v, "dev") {
		return true
	}
	_, ok := engine.ParseSemver(v)
	return !ok
}

// CompareVersions compares two semantic version tags via engine.CompareSemver.
func CompareVersions(a, b string) (int, bool) {
	return engine.CompareSemver(a, b)
}

type githubReleasePayload struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Assets     []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// CheckBot inspects local version/permissions and queries GitHub Releases for the latest stable release.
func (u *Upgrader) CheckBot(ctx context.Context) BotStatus {
	cur := "dev"
	if u != nil && strings.TrimSpace(u.CurrentVersion) != "" {
		cur = strings.TrimSpace(u.CurrentVersion)
	}
	st := BotStatus{
		Current:    cur,
		IsDev:      IsDevVersion(cur),
		LaunchPath: u.launchPath(),
		TargetPath: u.targetPath(),
	}

	if _, err := AssetNameFor(u.goos(), u.goarch()); err != nil {
		st.WriteHint = err.Error()
	} else if st.TargetPath == "" {
		st.WriteHint = "无法定位当前运行程序的磁盘路径。"
	} else if isEphemeralOrReadOnlyStore(st.TargetPath) {
		st.WriteHint = fmt.Sprintf("当前程序位于临时或只读目录 (%s)，不支持原地自动升级。", filepath.Dir(st.TargetPath))
	} else if err := checkDirWritable(filepath.Dir(st.TargetPath)); err == nil {
		st.Writable = true
	} else {
		st.WriteHint = botWriteHint(u.goos(), st.TargetPath)
	}

	rel, err := u.CheckLatest(ctx)
	if err != nil {
		st.Error = engine.RedactSensitive(err.Error())
		return st
	}
	st.Latest = rel.TagName
	st.ReleaseURL = rel.HTMLURL
	st.Release = rel
	if !st.IsDev && engine.IsStable(rel.TagName) {
		if cmp, ok := engine.CompareSemver(rel.TagName, cur); ok && cmp > 0 {
			st.UpdateAvailable = true
		}
	}
	return st
}

// CheckLatest queries GitHub Releases for the latest stable release and locates the platform binary and SHA256SUMS.
func (u *Upgrader) CheckLatest(ctx context.Context) (ReleaseInfo, error) {
	assetName, err := AssetNameFor(u.goos(), u.goarch())
	if err != nil {
		return ReleaseInfo{}, err
	}
	endpoint := fmt.Sprintf("%s/repos/%s/releases/latest", u.apiBase(), u.repo())
	if err := u.validateDownloadURL(endpoint); err != nil {
		return ReleaseInfo{}, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return ReleaseInfo{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "agent-bot-upgrader")

	resp, err := u.httpClient(20 * time.Second).Do(req)
	if err != nil {
		return ReleaseInfo{}, fmt.Errorf("查询 GitHub 最新版本失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ReleaseInfo{}, fmt.Errorf("查询 GitHub 最新版本失败（HTTP %d）", resp.StatusCode)
	}

	var payload githubReleasePayload
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload); err != nil {
		return ReleaseInfo{}, fmt.Errorf("解析版本信息失败：%w", err)
	}
	if payload.Draft || payload.Prerelease {
		return ReleaseInfo{}, errors.New("最新构建为预发布或草稿版本，跳过自动升级")
	}
	tag := strings.TrimSpace(payload.TagName)
	if !engine.IsStable(tag) {
		return ReleaseInfo{}, fmt.Errorf("远端版本号格式非稳定正式版：%q", tag)
	}

	info := ReleaseInfo{
		TagName:   tag,
		HTMLURL:   strings.TrimSpace(payload.HTMLURL),
		AssetName: assetName,
	}
	for _, a := range payload.Assets {
		switch strings.TrimSpace(a.Name) {
		case assetName:
			info.AssetURL = strings.TrimSpace(a.BrowserDownloadURL)
		case "SHA256SUMS":
			info.SumsURL = strings.TrimSpace(a.BrowserDownloadURL)
		}
	}
	if info.AssetURL == "" {
		return ReleaseInfo{}, fmt.Errorf("Release %s 中未找到当前平台二进制 (%s)", tag, assetName)
	}
	if info.SumsURL == "" {
		return ReleaseInfo{}, fmt.Errorf("Release %s 缺少 SHA256SUMS 校验文件，拒绝不安全升级", tag)
	}
	if err := u.validateDownloadURL(info.AssetURL); err != nil {
		return ReleaseInfo{}, err
	}
	if err := u.validateDownloadURL(info.SumsURL); err != nil {
		return ReleaseInfo{}, err
	}
	return info, nil
}

var trustedGitHubHosts = map[string]bool{
	"api.github.com":                       true,
	"github.com":                           true,
	"objects.githubusercontent.com":        true,
	"release-assets.githubusercontent.com": true,
	"codeload.github.com":                  true,
}

func (u *Upgrader) validateDownloadURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return errors.New("升级下载地址无效")
	}
	if u != nil && u.AllowInsecureHTTP {
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			return nil
		}
	}
	if parsed.Scheme != "https" {
		return errors.New("拒绝非 HTTPS 升级地址")
	}
	host := strings.ToLower(parsed.Hostname())
	if !trustedGitHubHosts[host] {
		return fmt.Errorf("拒绝非官方受信任下载域名：%s", host)
	}
	return nil
}

// ParseSHA256Sums extracts the expected lowercase 64-character hex SHA-256 digest for assetName.
func ParseSHA256Sums(sumsContent, assetName string) (string, error) {
	assetName = strings.TrimSpace(assetName)
	if assetName == "" {
		return "", errors.New("empty asset name")
	}
	for _, rawLine := range strings.Split(sumsContent, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		hashHex := strings.ToLower(strings.TrimSpace(fields[0]))
		name := strings.TrimSpace(fields[1])
		name = strings.TrimPrefix(name, "*")
		name = strings.TrimPrefix(name, "./")
		name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
		if name != assetName {
			continue
		}
		if len(hashHex) != 64 {
			return "", fmt.Errorf("SHA256SUMS 中 %s 的哈希长度无效", assetName)
		}
		if _, err := hex.DecodeString(hashHex); err != nil {
			return "", fmt.Errorf("SHA256SUMS 中 %s 的哈希格式无效", assetName)
		}
		return hashHex, nil
	}
	return "", fmt.Errorf("SHA256SUMS 中未找到 %s 的校验记录", assetName)
}

// StageBotUpgrade checks the latest release (optionally enforcing expectedTag when non-empty),
// downloads the binary into TargetPath's directory, verifies SHA-256 in constant time, checks
// executable magic bytes, and runs `<staged> --version` to verify clean startup.
func (u *Upgrader) StageBotUpgrade(ctx context.Context, expectedTag string) (StagedBotUpgrade, error) {
	cur := "dev"
	if u != nil && strings.TrimSpace(u.CurrentVersion) != "" {
		cur = strings.TrimSpace(u.CurrentVersion)
	}
	if IsDevVersion(cur) {
		return StagedBotUpgrade{}, errors.New("当前 agent-bot 为本地 dev 开发构建，已跳过自动覆盖；请部署正式 Release 版本后再执行自升级")
	}

	targetPath := u.targetPath()
	launchPath := u.launchPath()
	if targetPath == "" {
		return StagedBotUpgrade{}, errors.New("无法定位当前可执行文件物理路径")
	}
	if isEphemeralOrReadOnlyStore(targetPath) {
		return StagedBotUpgrade{}, fmt.Errorf("当前程序位于临时或只读目录 (%s)，不支持原地自动升级", filepath.Dir(targetPath))
	}
	targetDir := filepath.Dir(targetPath)
	if err := checkDirWritable(targetDir); err != nil {
		return StagedBotUpgrade{}, errors.New(botWriteHint(u.goos(), targetPath))
	}

	rel, err := u.CheckLatest(ctx)
	if err != nil {
		return StagedBotUpgrade{}, err
	}
	expectedTag = strings.TrimSpace(expectedTag)
	if expectedTag != "" && rel.TagName != expectedTag {
		return StagedBotUpgrade{}, fmt.Errorf("远端最新版本已变更为 %s（原确认版本为 %s），请重新执行 /upgrade 确认", rel.TagName, expectedTag)
	}
	cmp, ok := engine.CompareSemver(rel.TagName, cur)
	if !ok || cmp <= 0 {
		return StagedBotUpgrade{
			Changed:    false,
			LaunchPath: launchPath,
			TargetPath: targetPath,
			OldVersion: cur,
			NewVersion: cur,
			ReleaseURL: rel.HTMLURL,
		}, nil
	}

	sumsBytes, err := u.fetchSmallBytes(ctx, rel.SumsURL, maxSumsBytes, 30*time.Second)
	if err != nil {
		return StagedBotUpgrade{}, fmt.Errorf("下载 SHA256SUMS 失败：%w", err)
	}
	expectedHex, err := ParseSHA256Sums(string(sumsBytes), rel.AssetName)
	if err != nil {
		return StagedBotUpgrade{}, err
	}

	stagedPath := filepath.Join(targetDir, stagedFilename(u.goos()))
	tmpFile, err := os.OpenFile(stagedPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return StagedBotUpgrade{}, fmt.Errorf("无法在程序目录创建暂存文件 (%s)：%w", targetDir, err)
	}
	keepStaged := false
	defer func() {
		if !keepStaged {
			_ = tmpFile.Close()
			_ = os.Remove(stagedPath)
		}
	}()

	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(dlCtx, http.MethodGet, rel.AssetURL, nil)
	if err != nil {
		return StagedBotUpgrade{}, err
	}
	req.Header.Set("User-Agent", "agent-bot-upgrader")
	resp, err := u.httpClient(5 * time.Minute).Do(req)
	if err != nil {
		return StagedBotUpgrade{}, fmt.Errorf("下载新版本二进制失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return StagedBotUpgrade{}, fmt.Errorf("下载新版本二进制失败（HTTP %d）", resp.StatusCode)
	}

	hasher := sha256.New()
	var headerBuf bytes.Buffer
	tee := io.LimitReader(resp.Body, maxBinaryBytes+1)
	written, err := copyWithHeader(io.MultiWriter(tmpFile, hasher), tee, &headerBuf, 8)
	if err != nil {
		return StagedBotUpgrade{}, fmt.Errorf("写入新版本暂存文件失败：%w", err)
	}
	if written == 0 {
		return StagedBotUpgrade{}, errors.New("下载到的二进制文件为空，已中止")
	}
	if written > maxBinaryBytes {
		return StagedBotUpgrade{}, errors.New("下载文件体积超过安全上限，已中止")
	}
	if !validExecutableHeader(u.goos(), headerBuf.Bytes()) {
		return StagedBotUpgrade{}, fmt.Errorf("下载产物不是合法的 %s 可执行文件格式，已中止", u.goos())
	}

	gotHex := strings.ToLower(hex.EncodeToString(hasher.Sum(nil)))
	if subtle.ConstantTimeCompare([]byte(gotHex), []byte(expectedHex)) != 1 {
		return StagedBotUpgrade{}, fmt.Errorf("SHA-256 校验不匹配（期望 %s…，实际 %s…），已拒绝替换", expectedHex[:12], gotHex[:12])
	}

	perm := os.FileMode(0o755)
	if st, statErr := os.Stat(targetPath); statErr == nil {
		perm = st.Mode().Perm() | 0o700
		if perm&0o111 == 0 {
			perm |= 0o755
		}
	}
	if err := tmpFile.Sync(); err != nil {
		return StagedBotUpgrade{}, fmt.Errorf("刷盘暂存二进制失败：%w", err)
	}
	if err := tmpFile.Close(); err != nil {
		return StagedBotUpgrade{}, fmt.Errorf("关闭暂存二进制失败：%w", err)
	}
	if err := os.Chmod(stagedPath, perm); err != nil {
		return StagedBotUpgrade{}, fmt.Errorf("设置可执行权限失败：%w", err)
	}

	// Smoke-run `<stagedPath> --version` when running on the matching host OS/arch (or via test hook).
	if !u.SkipSmokeExec && (u.RunCmd != nil || (u.goos() == runtime.GOOS && u.goarch() == runtime.GOARCH)) {
		verifyOut, verifyErr := u.execSmoke(ctx, 15*time.Second, stagedPath, "--version")
		if verifyErr != nil {
			return StagedBotUpgrade{}, fmt.Errorf("新二进制可执行性自检失败：%s", engine.RedactSensitive(verifyErr.Error()))
		}
		if !strings.Contains(strings.ToLower(verifyOut), "agent-bot") {
			return StagedBotUpgrade{}, fmt.Errorf("新二进制自检输出异常：%q", firstLine(verifyOut))
		}
	}

	keepStaged = true
	return StagedBotUpgrade{
		Changed:    true,
		LaunchPath: launchPath,
		TargetPath: targetPath,
		StagedPath: stagedPath,
		OldVersion: cur,
		NewVersion: rel.TagName,
		ReleaseURL: rel.HTMLURL,
	}, nil
}

// CommitBinary backs up targetPath to `<targetPath>.prev` and atomically swaps stagedPath into targetPath.
func CommitBinary(targetPath, stagedPath string) (string, error) {
	return commitBinaryForOS(runtime.GOOS, targetPath, stagedPath)
}

func commitBinaryForOS(goos, targetPath, stagedPath string) (string, error) {
	prevPath := targetPath + ".prev"
	perm := os.FileMode(0o755)
	if st, err := os.Stat(targetPath); err == nil {
		perm = st.Mode().Perm() | 0o700
		if perm&0o111 == 0 {
			perm |= 0o755
		}
	}

	if strings.EqualFold(goos, "windows") {
		if _, err := os.Stat(targetPath); err == nil {
			_ = os.Remove(prevPath)
			if _, stillErr := os.Stat(prevPath); stillErr == nil {
				_ = os.Rename(prevPath, prevPath+".old")
			}
			if err := os.Rename(targetPath, prevPath); err != nil {
				return "", fmt.Errorf("备份当前运行程序失败：%w", err)
			}
			if err := os.Rename(stagedPath, targetPath); err != nil {
				_ = os.Rename(prevPath, targetPath)
				return "", fmt.Errorf("替换新版程序失败（已自动回滚原文件）：%w", err)
			}
			return prevPath, nil
		}
		if err := os.Rename(stagedPath, targetPath); err != nil {
			return "", fmt.Errorf("写入新版程序失败：%w", err)
		}
		return "", nil
	}

	// Unix: write .prev via a sibling temp file + atomic rename to avoid ETXTBSY, then rename stagedPath -> targetPath.
	if _, err := os.Stat(targetPath); err == nil {
		tmpPrev := filepath.Join(filepath.Dir(targetPath), ".agent-bot.prev.tmp")
		if err := copyExecutable(targetPath, tmpPrev, perm); err != nil {
			return "", fmt.Errorf("备份当前可执行文件失败：%w", err)
		}
		if err := os.Rename(tmpPrev, prevPath); err != nil {
			_ = os.Remove(tmpPrev)
			return "", fmt.Errorf("保存旧版备份文件失败：%w", err)
		}
	}
	if err := os.Rename(stagedPath, targetPath); err != nil {
		return "", fmt.Errorf("原子替换新版二进制失败：%w", err)
	}
	return prevPath, nil
}

// RollbackBinary restores `<targetPath>.prev` back to `targetPath` if a post-commit restart fails.
func RollbackBinary(targetPath string) error {
	return rollbackBinaryForOS(runtime.GOOS, targetPath)
}

func rollbackBinaryForOS(goos, targetPath string) error {
	prevPath := targetPath + ".prev"
	info, err := os.Stat(prevPath)
	if err != nil || info.IsDir() || info.Size() == 0 {
		return fmt.Errorf("未找到有效的旧版备份文件 %s", prevPath)
	}
	if strings.EqualFold(goos, "windows") {
		_ = os.Remove(targetPath)
		return os.Rename(prevPath, targetPath)
	}
	tmpRoll := filepath.Join(filepath.Dir(targetPath), ".agent-bot.rollback.tmp")
	if err := copyExecutable(prevPath, tmpRoll, 0o755); err != nil {
		return err
	}
	return os.Rename(tmpRoll, targetPath)
}

func copyExecutable(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, perm)
}

func stagedFilename(goos string) string {
	if strings.EqualFold(goos, "windows") {
		return ".agent-bot.upgrade.new.exe"
	}
	return ".agent-bot.upgrade.new"
}

func (u *Upgrader) execSmoke(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if u != nil && u.RunCmd != nil {
		return u.RunCmd(ctx, timeout, name, args...)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Env = engine.ScrubbedEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("自检执行超时 (%s)", timeout)
		}
		if s := firstLine(stderr.String()); s != "" {
			return "", errors.New(s)
		}
		if s := firstLine(stdout.String()); s != "" {
			return "", errors.New(s)
		}
		return "", err
	}
	if s := strings.TrimSpace(stdout.String()); s != "" {
		return s, nil
	}
	return strings.TrimSpace(stderr.String()), nil
}

func (u *Upgrader) fetchSmallBytes(ctx context.Context, rawURL string, limit int64, timeout time.Duration) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "agent-bot-upgrader")
	resp, err := u.httpClient(timeout).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("响应内容超出安全大小上限")
	}
	return data, nil
}

func copyWithHeader(dst io.Writer, src io.Reader, hdr *bytes.Buffer, hdrLimit int) (int64, error) {
	buf := make([]byte, 32*1024)
	var total int64
	for {
		nr, rErr := src.Read(buf)
		if nr > 0 {
			if hdr.Len() < hdrLimit {
				rem := hdrLimit - hdr.Len()
				if nr < rem {
					rem = nr
				}
				_, _ = hdr.Write(buf[:rem])
			}
			nw, wErr := dst.Write(buf[:nr])
			total += int64(nw)
			if wErr != nil {
				return total, wErr
			}
			if nw != nr {
				return total, io.ErrShortWrite
			}
		}
		if rErr != nil {
			if errors.Is(rErr, io.EOF) {
				return total, nil
			}
			return total, rErr
		}
	}
}

func validExecutableHeader(goos string, head []byte) bool {
	switch strings.ToLower(goos) {
	case "linux":
		return len(head) >= 4 && head[0] == 0x7f && head[1] == 'E' && head[2] == 'L' && head[3] == 'F'
	case "windows":
		return len(head) >= 2 && head[0] == 'M' && head[1] == 'Z'
	case "darwin":
		if len(head) < 4 {
			return false
		}
		magic := binary.BigEndian.Uint32(head[:4])
		switch magic {
		case 0xfeedface, 0xcefaedfe, 0xfeedfacf, 0xcffaedfe, 0xcafebabe:
			return true
		default:
			return false
		}
	default:
		return len(head) >= 4
	}
}

func isEphemeralOrReadOnlyStore(targetPath string) bool {
	clean := filepath.Clean(targetPath)
	if strings.HasPrefix(clean, "/nix/store/") {
		return true
	}
	tmpDir := filepath.Clean(os.TempDir())
	if tmpDir != "" && tmpDir != string(filepath.Separator) {
		if realTmp, err := filepath.EvalSymlinks(tmpDir); err == nil && realTmp != "" {
			tmpDir = filepath.Clean(realTmp)
		}
		rel, relErr := filepath.Rel(tmpDir, clean)
		if relErr == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			if strings.Contains(rel, "go-build") {
				return true
			}
		}
	}
	return false
}

func botWriteHint(goos, targetPath string) string {
	if strings.EqualFold(goos, "windows") {
		return fmt.Sprintf("请将程序移动到当前用户可写的目录（当前为 %s），或手动替换新版 .exe。", targetPath)
	}
	dir := filepath.Dir(targetPath)
	return fmt.Sprintf(
		"agent-bot 当前安装在 %s（目录 %s 对服务运行用户不可写）。\n"+
			"为保障安全无需授予 sudo，推荐将二进制放在运行用户自有目录（如 ~/bin/agent-bot）并在 systemd 中设置 ExecStart=/home/%%i/bin/agent-bot。",
		targetPath, dir,
	)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
