package antigravity

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
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
	// DefaultManifestBaseURL is the official Antigravity CLI release manifest base URL.
	DefaultManifestBaseURL = "https://antigravity-cli-auto-updater-974169037036.us-central1.run.app"
	maxManifestBytes       = 256 << 10
	maxCLIBinaryBytes      = 300 << 20
)

var allowedCLIDownloadHosts = map[string]bool{
	"antigravity-cli-auto-updater-974169037036.us-central1.run.app": true,
	"storage.googleapis.com": true,
	"antigravity.google":     true,
}

type cliManifest struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA512  string `json:"sha512"`
}

func (c *Client) manifestBase() string {
	if u := strings.TrimSpace(c.ManifestBaseURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultManifestBaseURL
}

func (c *Client) goos() string {
	if v := strings.ToLower(strings.TrimSpace(c.GOOS)); v != "" {
		return v
	}
	return runtime.GOOS
}

func (c *Client) goarch() string {
	if v := strings.ToLower(strings.TrimSpace(c.GOARCH)); v != "" {
		return v
	}
	return runtime.GOARCH
}

func (c *Client) updateHTTPClient(timeout time.Duration) *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: timeout}
}

func (c *Client) execUpdateCmd(ctx context.Context, timeout time.Duration, name string, args ...string) (string, error) {
	if c.RunUpdateCmd != nil {
		return c.RunUpdateCmd(ctx, timeout, name, args...)
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Env = engine.ScrubbedEnv()
	cmd.Stdin = nil

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	combined := strings.TrimSpace(stdout.String())
	errText := strings.TrimSpace(stderr.String())
	if err != nil {
		if runCtx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("命令执行超时 (%s)", timeout)
		}
		if errText != "" {
			return "", fmt.Errorf("%s", firstLine(errText))
		}
		if combined != "" {
			return "", fmt.Errorf("%s", firstLine(combined))
		}
		return "", err
	}
	if combined != "" {
		return combined, nil
	}
	return errText, nil
}

// CheckCLIUpgrade inspects the installed Antigravity CLI version, binary directory
// write permissions, and latest official release manifest.
func (c *Client) CheckCLIUpgrade(ctx context.Context) engine.CLIStatus {
	var st engine.CLIStatus
	rawOut, err := c.execUpdateCmd(ctx, 10*time.Second, c.bin(), "--version")
	if err != nil {
		st.Error = "读取本地 Antigravity CLI 版本失败：" + RedactSensitive(err.Error())
		return st
	}
	line := firstLine(rawOut)
	if sem, ok := engine.ParseSemver(line); ok {
		st.Current = sem.String()
	} else {
		st.Current = line
	}

	targetPath, wErr := c.checkCLIWritable()
	st.Path = targetPath
	if wErr == nil {
		st.Writable = true
	} else {
		st.WriteHint = cliWriteHint(c.goos(), targetPath, wErr)
	}

	m, err := c.fetchCLIManifest(ctx)
	if err != nil {
		st.Error = RedactSensitive(err.Error())
		return st
	}
	st.Latest = m.Version
	if engine.IsStable(st.Latest) {
		if cmp, ok := engine.CompareSemver(st.Latest, st.Current); ok && cmp > 0 {
			st.UpdateAvailable = true
		}
	}
	return st
}

// UpgradeCLI upgrades the Antigravity CLI, preferring built-in `agy update` and
// falling back to verified SHA-512 download + atomic replacement with .prev rollback.
func (c *Client) UpgradeCLI(ctx context.Context) (engine.CLIUpgradeResult, error) {
	st := c.CheckCLIUpgrade(ctx)
	if st.Error != "" {
		return engine.CLIUpgradeResult{}, errors.New(st.Error)
	}
	if !st.UpdateAvailable {
		return engine.CLIUpgradeResult{
			Changed:    false,
			OldVersion: st.Current,
			NewVersion: st.Current,
			TargetPath: st.Path,
		}, nil
	}
	if !st.Writable {
		return engine.CLIUpgradeResult{TargetPath: st.Path}, errors.New(st.WriteHint)
	}

	// 1. Try built-in `agy update` when enabled.
	if !c.SkipBuiltinUpdate {
		if out, err := c.execUpdateCmd(ctx, 3*time.Minute, c.bin(), "update"); err == nil {
			if newVer, verErr := c.readCurrentVersion(ctx); verErr == nil && newVer != "" {
				if cmp, ok := engine.CompareSemver(newVer, st.Current); ok && cmp > 0 {
					return engine.CLIUpgradeResult{
						Changed:    true,
						OldVersion: st.Current,
						NewVersion: newVer,
						TargetPath: st.Path,
						Output:     RedactSensitive(out),
					}, nil
				}
			}
		}
	}

	// 2. Fallback: fetch manifest, verify SHA-512, smoke-check, and atomically replace binary.
	m, err := c.fetchCLIManifest(ctx)
	if err != nil {
		return engine.CLIUpgradeResult{TargetPath: st.Path}, err
	}
	if err := c.validateDownloadURL(m.URL); err != nil {
		return engine.CLIUpgradeResult{TargetPath: st.Path}, err
	}
	if strings.TrimSpace(st.Path) == "" {
		return engine.CLIUpgradeResult{}, errors.New("无法定位 Antigravity CLI 安装路径")
	}

	dir := filepath.Dir(st.Path)
	stagedPath, err := c.downloadAndStageCLI(ctx, dir, m)
	if err != nil {
		return engine.CLIUpgradeResult{TargetPath: st.Path}, err
	}
	defer os.Remove(stagedPath)

	if !c.SkipSmokeExec {
		if _, smokeErr := c.execUpdateCmd(ctx, 15*time.Second, stagedPath, "--version"); smokeErr != nil {
			return engine.CLIUpgradeResult{TargetPath: st.Path}, fmt.Errorf("新版本 Antigravity CLI 冒烟校验失败：%s", RedactSensitive(smokeErr.Error()))
		}
	}

	if err := replaceBinaryWithRollback(st.Path, stagedPath); err != nil {
		return engine.CLIUpgradeResult{TargetPath: st.Path}, err
	}

	if !c.SkipSmokeExec {
		if _, verifyErr := c.execUpdateCmd(ctx, 15*time.Second, st.Path, "--version"); verifyErr != nil {
			rbMsg := rollbackCLI(st.Path)
			return engine.CLIUpgradeResult{TargetPath: st.Path}, fmt.Errorf("替换后 Antigravity CLI 自检失败（%s），%s", RedactSensitive(verifyErr.Error()), rbMsg)
		}
	}

	newVer := m.Version
	if v, verErr := c.readCurrentVersion(ctx); verErr == nil && v != "" {
		newVer = v
	}
	return engine.CLIUpgradeResult{
		Changed:    true,
		OldVersion: st.Current,
		NewVersion: newVer,
		TargetPath: st.Path,
	}, nil
}

func (c *Client) readCurrentVersion(ctx context.Context) (string, error) {
	out, err := c.execUpdateCmd(ctx, 10*time.Second, c.bin(), "--version")
	if err != nil {
		return "", err
	}
	line := firstLine(out)
	if sem, ok := engine.ParseSemver(line); ok {
		return sem.String(), nil
	}
	return line, nil
}

func (c *Client) fetchCLIManifest(ctx context.Context) (cliManifest, error) {
	plat, err := manifestPlatform(c.goos(), c.goarch())
	if err != nil {
		return cliManifest{}, err
	}
	endpoint := fmt.Sprintf("%s/manifests/%s.json", c.manifestBase(), plat)
	if err := c.validateDownloadURL(endpoint); err != nil {
		return cliManifest{}, err
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return cliManifest{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "agent-bot-upgrader")

	resp, err := c.updateHTTPClient(15 * time.Second).Do(req)
	if err != nil {
		return cliManifest{}, fmt.Errorf("查询 Antigravity CLI 最新版本失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return cliManifest{}, fmt.Errorf("查询 Antigravity CLI 最新版本失败（HTTP %d）", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return cliManifest{}, err
	}
	var m cliManifest
	if err := json.Unmarshal(body, &m); err != nil {
		return cliManifest{}, fmt.Errorf("解析 Antigravity CLI 发布清单失败：%w", err)
	}
	sem, ok := engine.ParseSemver(m.Version)
	if !ok {
		return cliManifest{}, fmt.Errorf("Antigravity CLI 发布清单版本号无效：%q", m.Version)
	}
	m.Version = sem.String()
	m.URL = strings.TrimSpace(m.URL)
	m.SHA512 = strings.ToLower(strings.TrimSpace(m.SHA512))
	if m.URL == "" || len(m.SHA512) != sha512.Size*2 {
		return cliManifest{}, errors.New("Antigravity CLI 发布清单缺少有效下载地址或 SHA-512 校验值")
	}
	return m, nil
}

func manifestPlatform(goos, goarch string) (string, error) {
	switch goos + "/" + goarch {
	case "linux/amd64":
		if isMuslLinux() {
			return "linux_amd64_musl", nil
		}
		return "linux_amd64", nil
	case "linux/arm64":
		if isMuslLinux() {
			return "linux_arm64_musl", nil
		}
		return "linux_arm64", nil
	case "darwin/amd64":
		return "darwin_amd64", nil
	case "darwin/arm64":
		return "darwin_arm64", nil
	case "windows/amd64":
		return "windows_amd64", nil
	case "windows/arm64":
		return "windows_arm64", nil
	default:
		return "", fmt.Errorf("当前系统架构 (%s/%s) 暂不支持 Antigravity CLI 在线升级", goos, goarch)
	}
}

func isMuslLinux() bool {
	if _, err := os.Stat("/lib/libc.musl-x86_64.so.1"); err == nil {
		return true
	}
	if _, err := os.Stat("/lib/libc.musl-aarch64.so.1"); err == nil {
		return true
	}
	return false
}

func (c *Client) validateDownloadURL(rawURL string) error {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return fmt.Errorf("非法的 Antigravity CLI 下载地址")
	}
	if c.AllowInsecureHTTP && u.Scheme == "http" {
		host := strings.ToLower(u.Hostname())
		if host == "127.0.0.1" || host == "localhost" || host == "::1" {
			return nil
		}
	}
	if u.Scheme != "https" {
		return fmt.Errorf("拒绝非 HTTPS 下载地址 (%s)", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if !allowedCLIDownloadHosts[host] {
		return fmt.Errorf("下载地址域名 %q 不在 Antigravity 官方白名单内", host)
	}
	return nil
}

func (c *Client) downloadAndStageCLI(ctx context.Context, targetDir string, m cliManifest) (string, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, m.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "agent-bot-upgrader")

	resp, err := c.updateHTTPClient(5 * time.Minute).Do(req)
	if err != nil {
		return "", fmt.Errorf("下载 Antigravity CLI 失败：%w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载 Antigravity CLI 失败（HTTP %d）", resp.StatusCode)
	}

	tmpArchive, err := os.CreateTemp(targetDir, ".agy-dl-*")
	if err != nil {
		return "", err
	}
	tmpArchivePath := tmpArchive.Name()
	defer os.Remove(tmpArchivePath)

	h := sha512.New()
	written, copyErr := io.Copy(io.MultiWriter(tmpArchive, h), io.LimitReader(resp.Body, maxCLIBinaryBytes+1))
	closeErr := tmpArchive.Close()
	if copyErr != nil {
		return "", fmt.Errorf("写入下载缓存失败：%w", copyErr)
	}
	if closeErr != nil {
		return "", closeErr
	}
	if written > maxCLIBinaryBytes {
		return "", errors.New("下载的 Antigravity CLI 安装包超过大小上限")
	}

	actualHex := hex.EncodeToString(h.Sum(nil))
	if subtle.ConstantTimeCompare([]byte(actualHex), []byte(m.SHA512)) != 1 {
		return "", errors.New("Antigravity CLI SHA-512 校验失败，已中止升级")
	}

	staged, err := os.CreateTemp(targetDir, ".agy-staged-*")
	if err != nil {
		return "", err
	}
	stagedPath := staged.Name()
	keepStaged := false
	defer func() {
		if !keepStaged {
			_ = os.Remove(stagedPath)
		}
	}()

	isTarGz := strings.Contains(strings.ToLower(m.URL), ".tar.gz")
	if isTarGz {
		if err := extractBinaryFromTarGz(tmpArchivePath, staged); err != nil {
			_ = staged.Close()
			return "", err
		}
	} else {
		src, openErr := os.Open(tmpArchivePath)
		if openErr != nil {
			_ = staged.Close()
			return "", openErr
		}
		_, cpErr := io.Copy(staged, io.LimitReader(src, maxCLIBinaryBytes))
		_ = src.Close()
		if cpErr != nil {
			_ = staged.Close()
			return "", cpErr
		}
	}
	if err := staged.Chmod(0o755); err != nil {
		_ = staged.Close()
		return "", err
	}
	if err := staged.Close(); err != nil {
		return "", err
	}

	if err := verifyExecutableMagic(stagedPath, c.goos()); err != nil {
		return "", err
	}
	keepStaged = true
	return stagedPath, nil
}

func extractBinaryFromTarGz(archivePath string, dst io.Writer) error {
	f, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("解压 Antigravity CLI gzip 失败：%w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("读取 Antigravity CLI tar 归档失败：%w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		if base == "antigravity" || base == "agy" || base == "agy.exe" {
			if _, cpErr := io.Copy(dst, io.LimitReader(tr, maxCLIBinaryBytes)); cpErr != nil {
				return cpErr
			}
			return nil
		}
	}
	return errors.New("归档中未找到 antigravity/agy 可执行文件")
}

func verifyExecutableMagic(path, goos string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	var hdr [4]byte
	if _, err := io.ReadFull(f, hdr[:]); err != nil {
		return fmt.Errorf("读取可执行文件头失败：%w", err)
	}
	switch goos {
	case "linux":
		if hdr != [4]byte{0x7f, 'E', 'L', 'F'} {
			return errors.New("校验失败：产物不是有效的 Linux ELF 可执行文件")
		}
	case "windows":
		if hdr[0] != 'M' || hdr[1] != 'Z' {
			return errors.New("校验失败：产物不是有效的 Windows PE 可执行文件")
		}
	case "darwin":
		magic := binary.BigEndian.Uint32(hdr[:])
		switch magic {
		case 0xfeedface, 0xfeedfacf, 0xcafebabe, 0xcefaedfe, 0xcffaedfe:
		default:
			return errors.New("校验失败：产物不是有效的 macOS Mach-O 可执行文件")
		}
	}
	return nil
}

func replaceBinaryWithRollback(targetPath, stagedPath string) error {
	prevPath := targetPath + ".prev"
	_ = os.Remove(prevPath)
	hasPrev := false
	if _, err := os.Stat(targetPath); err == nil {
		if err := os.Rename(targetPath, prevPath); err != nil {
			return fmt.Errorf("备份旧版 Antigravity CLI 失败：%w", err)
		}
		hasPrev = true
	}
	if err := os.Rename(stagedPath, targetPath); err != nil {
		if hasPrev {
			_ = os.Rename(prevPath, targetPath)
		}
		return fmt.Errorf("替换 Antigravity CLI 失败：%w", err)
	}
	return nil
}

func rollbackCLI(targetPath string) string {
	prevPath := targetPath + ".prev"
	if _, err := os.Stat(prevPath); err != nil {
		return "未找到 .prev 备份可回滚"
	}
	if err := os.Rename(prevPath, targetPath); err != nil {
		return "自动回滚失败：" + RedactSensitive(err.Error())
	}
	return "已自动回滚至升级前版本"
}

func (c *Client) checkCLIWritable() (string, error) {
	bin := c.bin()
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return bin, err
	}
	if abs, absErr := filepath.Abs(resolved); absErr == nil && abs != "" {
		resolved = abs
	}
	if real, symErr := filepath.EvalSymlinks(resolved); symErr == nil && real != "" {
		resolved = real
	}
	dir := filepath.Dir(resolved)
	f, err := os.CreateTemp(dir, ".agent-bot-agycheck-*")
	if err != nil {
		return resolved, err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return resolved, nil
}

func cliWriteHint(goos, targetPath string, err error) string {
	dir := filepath.Dir(targetPath)
	if dir == "" || dir == "." {
		return err.Error()
	}
	if goos == "windows" {
		return fmt.Sprintf("Antigravity CLI 所在目录 (%s) 当前无写入权限，请在终端手动更新或重装到用户目录。", dir)
	}
	return fmt.Sprintf(
		"Antigravity CLI 所在目录 (%s) 当前运行用户无写入权限。\n"+
			"建议将该用户下的 agy 安装到 ~/.local/bin（curl -fsSL https://antigravity.google/cli/install.sh | bash），即可免 root 自动升级。",
		dir,
	)
}
