package codex

import (
	"bytes"
	"context"
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
	DefaultNPMRegistry = "https://registry.npmjs.org"
	CodexNPMPackage    = "@openai/codex"
	maxNPMMetaBytes    = 2 << 20 // 2 MiB
)

func (c *Client) npmBin() string {
	if b := strings.TrimSpace(c.NPMBin); b != "" {
		return b
	}
	return "npm"
}

func (c *Client) npmRegistry() string {
	if u := strings.TrimSpace(c.NPMRegistry); u != "" {
		return strings.TrimRight(u, "/")
	}
	return DefaultNPMRegistry
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

// CheckCLIUpgrade inspects the installed Codex CLI version, npm prefix write permission, and latest npm version.
func (c *Client) CheckCLIUpgrade(ctx context.Context) engine.CLIStatus {
	var st engine.CLIStatus
	rawOut, err := c.execUpdateCmd(ctx, 10*time.Second, c.bin(), "--version")
	if err != nil {
		st.Error = "读取本地 Codex CLI 版本失败：" + RedactSensitive(err.Error())
		return st
	}
	line := firstLine(rawOut)
	if sem, ok := engine.ParseSemver(line); ok {
		st.Current = sem.String()
	} else {
		st.Current = line
	}

	prefix, wErr := c.checkNPMWritable(ctx)
	st.Path = prefix
	if wErr == nil {
		st.Writable = true
	} else {
		st.WriteHint = npmWriteHint(prefix, wErr)
	}

	latest, err := c.fetchLatestCLIVersion(ctx)
	if err != nil {
		st.Error = RedactSensitive(err.Error())
		return st
	}
	st.Latest = latest
	if engine.IsStable(st.Latest) {
		if cmp, ok := engine.CompareSemver(st.Latest, st.Current); ok && cmp > 0 {
			st.UpdateAvailable = true
		}
	}
	return st
}

// UpgradeCLI upgrades @openai/codex via npm, verifies `--version`, and rolls back automatically on failure.
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

	c.ResetLeader(ctx)
	targetPkg := fmt.Sprintf("%s@%s", CodexNPMPackage, st.Latest)
	if _, err := c.execUpdateCmd(ctx, 5*time.Minute, c.npmBin(), "install", "-g", targetPkg, "--no-fund", "--no-audit"); err != nil {
		return engine.CLIUpgradeResult{TargetPath: st.Path}, fmt.Errorf("执行 npm 升级失败：%s", RedactSensitive(err.Error()))
	}

	verifyOut, verifyErr := c.execUpdateCmd(ctx, 15*time.Second, c.bin(), "--version")
	if verifyErr != nil {
		rollbackMsg := "未找到可回滚的旧版本号"
		if oldSem, ok := engine.ParseSemver(st.Current); ok {
			oldPkg := fmt.Sprintf("%s@%s", CodexNPMPackage, oldSem.String())
			if _, rbErr := c.execUpdateCmd(ctx, 5*time.Minute, c.npmBin(), "install", "-g", oldPkg, "--no-fund", "--no-audit"); rbErr == nil {
				rollbackMsg = "已自动回滚至 " + oldSem.String()
			} else {
				rollbackMsg = "自动回滚失败：" + RedactSensitive(rbErr.Error())
			}
		}
		return engine.CLIUpgradeResult{TargetPath: st.Path}, fmt.Errorf("升级后 Codex CLI 自检失败（%s），%s", RedactSensitive(verifyErr.Error()), rollbackMsg)
	}

	c.ResetLeader(context.Background())
	newVer := firstLine(verifyOut)
	if sem, ok := engine.ParseSemver(newVer); ok {
		newVer = sem.String()
	}
	if newVer == "" {
		newVer = st.Latest
	}
	return engine.CLIUpgradeResult{
		Changed:    true,
		OldVersion: st.Current,
		NewVersion: newVer,
		TargetPath: st.Path,
	}, nil
}

func (c *Client) fetchLatestCLIVersion(ctx context.Context) (string, error) {
	escapedPkg := strings.ReplaceAll(url.PathEscape(CodexNPMPackage), "/", "%2f")
	regURL := fmt.Sprintf("%s/%s/latest", c.npmRegistry(), escapedPkg)
	reqCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	req, reqErr := http.NewRequestWithContext(reqCtx, http.MethodGet, regURL, nil)
	if reqErr == nil {
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "agent-bot-upgrader")
		if resp, doErr := c.updateHTTPClient(12 * time.Second).Do(req); doErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				if body, rErr := io.ReadAll(io.LimitReader(resp.Body, maxNPMMetaBytes)); rErr == nil {
					var meta struct {
						Version string `json:"version"`
					}
					if json.Unmarshal(body, &meta) == nil {
						if sem, ok := engine.ParseSemver(meta.Version); ok {
							return sem.String(), nil
						}
					}
				}
			}
		}
	}

	out, err := c.execUpdateCmd(ctx, 20*time.Second, c.npmBin(), "view", CodexNPMPackage, "version")
	if err != nil {
		return "", fmt.Errorf("查询 npm 最新版本失败：%s", RedactSensitive(err.Error()))
	}
	if sem, ok := engine.ParseSemver(firstLine(out)); ok {
		return sem.String(), nil
	}
	return "", fmt.Errorf("npm 返回了无法解析的版本号：%q", firstLine(out))
}

func (c *Client) checkNPMWritable(ctx context.Context) (string, error) {
	out, err := c.execUpdateCmd(ctx, 10*time.Second, c.npmBin(), "config", "get", "prefix")
	if err != nil {
		return "", fmt.Errorf("无法执行 npm（请确认已安装 Node.js/npm 并在 PATH 中）：%s", RedactSensitive(err.Error()))
	}
	prefix := firstLine(out)
	if prefix == "" || prefix == "undefined" {
		return "", errors.New("npm config get prefix 返回空路径")
	}

	var dirs []string
	if runtime.GOOS == "windows" {
		dirs = []string{prefix, filepath.Join(prefix, "node_modules"), filepath.Join(prefix, "node_modules", "@openai", "codex")}
	} else {
		dirs = []string{
			prefix,
			filepath.Join(prefix, "bin"),
			filepath.Join(prefix, "lib"),
			filepath.Join(prefix, "lib", "node_modules"),
			filepath.Join(prefix, "lib", "node_modules", "@openai", "codex"),
		}
	}
	checked := 0
	for _, d := range dirs {
		info, statErr := os.Stat(d)
		if statErr != nil || !info.IsDir() {
			continue
		}
		checked++
		if wErr := probeWritableDir(d); wErr != nil {
			return prefix, wErr
		}
	}
	if checked == 0 {
		parent := filepath.Dir(prefix)
		if wErr := probeWritableDir(parent); wErr != nil {
			return prefix, wErr
		}
	}
	return prefix, nil
}

func probeWritableDir(dir string) error {
	f, err := os.CreateTemp(dir, ".agent-bot-npmcheck-*")
	if err != nil {
		return err
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return nil
}

func npmWriteHint(prefix string, err error) string {
	if prefix == "" {
		return err.Error()
	}
	return fmt.Sprintf(
		"npm 全局目录 (%s) 当前运行用户无写入权限。\n"+
			"可在服务器上切到运行用户执行一次迁移，后续即可免 root 自动升级 Codex CLI：\n"+
			"mkdir -p ~/.npm-global && npm config set prefix ~/.npm-global\n"+
			"echo 'export PATH=\"$HOME/.npm-global/bin:$PATH\"' >> ~/.profile\n"+
			"npm install -g @openai/codex@latest",
		prefix,
	)
}
