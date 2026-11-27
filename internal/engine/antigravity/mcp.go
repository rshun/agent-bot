package antigravity

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/rshun/agent-bot/internal/engine"
)

// RedactSensitive masks tokens, API keys, passwords, and auth headers in CLI outputs.
var RedactSensitive = engine.RedactSensitive

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}

func (c *Client) mcpConfigPath() string {
	if p := strings.TrimSpace(c.MCPConfigPath); p != "" {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".gemini", "config", "mcp_config.json")
	}
	return filepath.Join(".gemini", "config", "mcp_config.json")
}

// Version returns the installed Antigravity CLI version string.
func (c *Client) Version(ctx context.Context) (string, error) {
	out, err := c.runAdmin(ctx, 10*time.Second, "--version")
	if err != nil {
		return "", err
	}
	return firstLine(out), nil
}

// ResetLeader is a no-op for Antigravity CLI because mcp_config.json is loaded per invocation.
func (c *Client) ResetLeader(context.Context) {}

type mcpFile struct {
	MCPServers map[string]mcpServerEntry
	Extra      map[string]json.RawMessage
}

type mcpServerEntry struct {
	Command  string            `json:"command,omitempty"`
	Args     []string          `json:"args,omitempty"`
	Env      map[string]string `json:"env,omitempty"`
	Cwd      string            `json:"cwd,omitempty"`
	ServerURL string           `json:"serverUrl,omitempty"`
	URL      string            `json:"url,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
	Disabled bool              `json:"disabled,omitempty"`
	Raw      map[string]json.RawMessage
}

func (e mcpServerEntry) remoteURL() string {
	if u := strings.TrimSpace(e.ServerURL); u != "" {
		return u
	}
	return strings.TrimSpace(e.URL)
}

func readMCPConfig(path string) (mcpFile, error) {
	res := mcpFile{
		MCPServers: map[string]mcpServerEntry{},
		Extra:      map[string]json.RawMessage{},
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return res, nil
		}
		return res, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return res, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return res, fmt.Errorf("解析 %s 失败：%w", path, err)
	}
	for k, v := range top {
		if k != "mcpServers" {
			res.Extra[k] = v
		}
	}
	if rawServers, ok := top["mcpServers"]; ok && len(bytes.TrimSpace(rawServers)) > 0 {
		var servers map[string]json.RawMessage
		if err := json.Unmarshal(rawServers, &servers); err != nil {
			return res, fmt.Errorf("解析 mcpServers 失败：%w", err)
		}
		for name, rawEntry := range servers {
			var entry mcpServerEntry
			if err := json.Unmarshal(rawEntry, &entry); err != nil {
				return res, fmt.Errorf("解析 MCP 服务 %q 配置失败：%w", name, err)
			}
			var rawMap map[string]json.RawMessage
			if json.Unmarshal(rawEntry, &rawMap) == nil {
				entry.Raw = rawMap
			}
			res.MCPServers[name] = entry
		}
	}
	return res, nil
}

func writeMCPConfig(path string, cfg mcpFile) error {
	top := make(map[string]any, len(cfg.Extra)+1)
	for k, v := range cfg.Extra {
		top[k] = v
	}
	servers := make(map[string]any, len(cfg.MCPServers))
	for name, entry := range cfg.MCPServers {
		servers[name] = marshalServerEntry(entry)
	}
	top["mcpServers"] = servers

	encoded, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomicBytes(path, append(encoded, '\n'), 0o600)
}

func marshalServerEntry(entry mcpServerEntry) map[string]json.RawMessage {
	out := make(map[string]json.RawMessage, len(entry.Raw)+4)
	for k, v := range entry.Raw {
		out[k] = v
	}
	setJSON := func(key string, val any) {
		if b, err := json.Marshal(val); err == nil {
			out[key] = b
		}
	}
	if entry.Command != "" {
		setJSON("command", entry.Command)
	}
	if len(entry.Args) > 0 {
		setJSON("args", entry.Args)
	}
	if len(entry.Env) > 0 {
		setJSON("env", entry.Env)
	}
	if entry.Cwd != "" {
		setJSON("cwd", entry.Cwd)
	}
	if entry.ServerURL != "" {
		setJSON("serverUrl", entry.ServerURL)
		delete(out, "url")
	} else if entry.URL != "" {
		setJSON("url", entry.URL)
	}
	if len(entry.Headers) > 0 {
		setJSON("headers", entry.Headers)
	}
	if entry.Disabled {
		setJSON("disabled", true)
	} else {
		delete(out, "disabled")
	}
	return out
}

// MCPList lists configured global MCP servers without exposing URLs, headers, or command arguments.
func (c *Client) MCPList(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cfg, err := readMCPConfig(c.mcpConfigPath())
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(cfg.MCPServers))
	for name := range cfg.MCPServers {
		if strings.TrimSpace(name) == "" || engine.IsReservedMCPName(name) {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	var lines []string
	for _, name := range names {
		entry := cfg.MCPServers[name]
		if entry.Disabled {
			lines = append(lines, name+" (disabled)")
		} else {
			lines = append(lines, name)
		}
	}
	return strings.Join(lines, "\n"), nil
}

// MCPDoctor probes configured global MCP servers by performing an MCP initialize handshake.
func (c *Client) MCPDoctor(ctx context.Context, name string) (string, error) {
	cfg, err := readMCPConfig(c.mcpConfigPath())
	if err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	var targets []string
	if name != "" {
		if err := checkMCPName(name); err != nil {
			return "", err
		}
		if _, ok := cfg.MCPServers[name]; !ok {
			return "", fmt.Errorf("未找到 MCP 服务 %q。", name)
		}
		targets = []string{name}
	} else {
		for k := range cfg.MCPServers {
			if strings.TrimSpace(k) == "" || engine.IsReservedMCPName(k) {
				continue
			}
			targets = append(targets, k)
		}
		sort.Strings(targets)
	}
	if len(targets) == 0 {
		return "", nil
	}

	var lines []string
	for _, srvName := range targets {
		entry := cfg.MCPServers[srvName]
		if entry.Disabled {
			lines = append(lines, fmt.Sprintf("%s: disabled", srvName))
			continue
		}
		if remote := entry.remoteURL(); remote != "" {
			if err := c.probeHTTPMCP(ctx, remote, entry.Headers); err != nil {
				lines = append(lines, fmt.Sprintf("%s [http]: FAILED (%s)", srvName, firstLine(RedactSensitive(err.Error()))))
			} else {
				lines = append(lines, fmt.Sprintf("%s [http]: ok", srvName))
			}
			continue
		}
		if strings.TrimSpace(entry.Command) != "" {
			if err := c.probeStdioMCP(ctx, entry); err != nil {
				lines = append(lines, fmt.Sprintf("%s [stdio]: FAILED (%s)", srvName, firstLine(RedactSensitive(err.Error()))))
			} else {
				lines = append(lines, fmt.Sprintf("%s [stdio]: ok", srvName))
			}
			continue
		}
		lines = append(lines, fmt.Sprintf("%s: invalid config (missing command or serverUrl)", srvName))
	}
	return strings.Join(lines, "\n"), nil
}

func (c *Client) probeHTTPMCP(ctx context.Context, targetURL string, headers map[string]string) error {
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	payload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"agent-bot","version":"1"}}}`
	req, err := http.NewRequestWithContext(runCtx, http.MethodPost, targetURL, strings.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("http %d", resp.StatusCode)
	}
	return validateInitializeReply(body)
}

func (c *Client) probeStdioMCP(ctx context.Context, entry mcpServerEntry) error {
	runCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(runCtx, entry.Command, entry.Args...)
	if entry.Cwd != "" {
		cmd.Dir = entry.Cwd
	}
	env := engine.ScrubbedEnv()
	for k, v := range entry.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	payload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"agent-bot","version":"1"}}}` + "\n"
	cmd.Stdin = strings.NewReader(payload)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 16*1024), 1024*1024)
	var line []byte
	for sc.Scan() {
		trimmed := bytes.TrimSpace(sc.Bytes())
		if len(trimmed) > 0 && trimmed[0] == '{' {
			line = append([]byte(nil), trimmed...)
			break
		}
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if len(line) > 0 {
		return validateInitializeReply(line)
	}
	if waitErr != nil {
		if s := firstLine(stderr.String()); s != "" {
			return errors.New(s)
		}
		return waitErr
	}
	return errors.New("stdio server exited without JSON-RPC response")
}

func validateInitializeReply(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return errors.New("empty response")
	}
	// Streamable HTTP may return SSE lines (`data: {...}`).
	if bytes.Contains(trimmed, []byte("data:")) && trimmed[0] != '{' {
		for _, line := range bytes.Split(trimmed, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if payload, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				payload = bytes.TrimSpace(payload)
				if len(payload) > 0 && payload[0] == '{' {
					trimmed = payload
					break
				}
			}
		}
	}
	var rpc struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &rpc); err != nil {
		return fmt.Errorf("invalid JSON-RPC response: %w", err)
	}
	if rpc.Error != nil {
		return errors.New(rpc.Error.Message)
	}
	if len(rpc.Result) == 0 {
		return errors.New("missing JSON-RPC result")
	}
	return nil
}

// MCPAdd adds or updates a global MCP server in ~/.gemini/config/mcp_config.json.
func (c *Client) MCPAdd(ctx context.Context, rawArgs []string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name, entry, err := parseMCPAddArgs(rawArgs)
	if err != nil {
		return "", err
	}
	path := c.mcpConfigPath()
	cfg, err := readMCPConfig(path)
	if err != nil {
		return "", err
	}
	cfg.MCPServers[name] = entry
	if err := writeMCPConfig(path, cfg); err != nil {
		return "", err
	}
	if entry.remoteURL() != "" {
		return fmt.Sprintf("%s (http)", name), nil
	}
	return fmt.Sprintf("%s (stdio)", name), nil
}

// MCPRemove deletes an MCP server from ~/.gemini/config/mcp_config.json.
func (c *Client) MCPRemove(ctx context.Context, name string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if err := checkMCPName(name); err != nil {
		return "", err
	}
	path := c.mcpConfigPath()
	cfg, err := readMCPConfig(path)
	if err != nil {
		return "", err
	}
	if _, ok := cfg.MCPServers[name]; !ok {
		return "", fmt.Errorf("未找到 MCP 服务 %q。", name)
	}
	delete(cfg.MCPServers, name)
	if err := writeMCPConfig(path, cfg); err != nil {
		return "", err
	}
	return name, nil
}

// MCPEnable enables or disables an MCP server in ~/.gemini/config/mcp_config.json.
func (c *Client) MCPEnable(ctx context.Context, name string, enable bool) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name = strings.TrimSpace(name)
	if err := checkMCPName(name); err != nil {
		return "", err
	}
	path := c.mcpConfigPath()
	cfg, err := readMCPConfig(path)
	if err != nil {
		return "", err
	}
	entry, ok := cfg.MCPServers[name]
	if !ok {
		return "", fmt.Errorf("未找到 MCP 服务 %q。", name)
	}
	entry.Disabled = !enable
	cfg.MCPServers[name] = entry
	if err := writeMCPConfig(path, cfg); err != nil {
		return "", err
	}
	return name, nil
}

func checkMCPName(name string) error {
	if name == "" {
		return errors.New("请指定 MCP 服务名称。")
	}
	if strings.HasPrefix(name, "-") {
		return errors.New("MCP 服务名称不能以 - 开头。")
	}
	if engine.IsReservedMCPName(name) {
		return errors.New("agentbot 是机器人内置通信通道，不能修改或删除。")
	}
	return nil
}

func parseMCPAddArgs(rawArgs []string) (string, mcpServerEntry, error) {
	if len(rawArgs) == 0 {
		return "", mcpServerEntry{}, errors.New("参数不足。")
	}
	envMap := map[string]string{}
	headersMap := map[string]string{}
	var pos []string
	var afterDashDash []string
	hasDashDash := false

	parseKV := func(raw, sep string) (string, string, bool) {
		k, v, ok := strings.Cut(raw, sep)
		if !ok {
			return "", "", false
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			return "", "", false
		}
		return k, v, true
	}

	for i := 0; i < len(rawArgs); i++ {
		tok := rawArgs[i]
		if hasDashDash {
			afterDashDash = append(afterDashDash, tok)
			continue
		}
		if tok == "--" {
			hasDashDash = true
			continue
		}
		switch {
		case tok == "-s" || tok == "--scope" || strings.HasPrefix(tok, "--scope="):
			return "", mcpServerEntry{}, errors.New("无需指定 --scope，默认固定写入当前用户的 ~/.gemini/config/mcp_config.json。")
		case tok == "-e" || tok == "--env":
			if i+1 >= len(rawArgs) {
				return "", mcpServerEntry{}, fmt.Errorf("参数 %s 缺少对应的值。", tok)
			}
			i++
			k, v, ok := parseKV(rawArgs[i], "=")
			if !ok {
				return "", mcpServerEntry{}, errors.New("环境变量格式应为 KEY=VALUE。")
			}
			envMap[k] = v
		case strings.HasPrefix(tok, "--env="):
			k, v, ok := parseKV(strings.TrimPrefix(tok, "--env="), "=")
			if !ok {
				return "", mcpServerEntry{}, errors.New("环境变量格式应为 KEY=VALUE。")
			}
			envMap[k] = v
		case tok == "-H" || tok == "--header":
			if i+1 >= len(rawArgs) {
				return "", mcpServerEntry{}, fmt.Errorf("参数 %s 缺少对应的值。", tok)
			}
			i++
			k, v, ok := parseKV(rawArgs[i], ":")
			if !ok {
				return "", mcpServerEntry{}, errors.New("请求头格式应为 \"Header: Value\"。")
			}
			headersMap[k] = v
		case strings.HasPrefix(tok, "--header="):
			k, v, ok := parseKV(strings.TrimPrefix(tok, "--header="), ":")
			if !ok {
				return "", mcpServerEntry{}, errors.New("请求头格式应为 \"Header: Value\"。")
			}
			headersMap[k] = v
		case tok == "-t" || tok == "--transport":
			if i+1 >= len(rawArgs) {
				return "", mcpServerEntry{}, fmt.Errorf("参数 %s 缺少对应的值。", tok)
			}
			i++
			if strings.EqualFold(strings.TrimSpace(rawArgs[i]), "sse") {
				return "", mcpServerEntry{}, errors.New("Antigravity CLI 仅支持 Streamable HTTP（serverUrl）或 stdio，不支持旧版 SSE。")
			}
		case strings.HasPrefix(tok, "--transport="):
			if strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(tok, "--transport=")), "sse") {
				return "", mcpServerEntry{}, errors.New("Antigravity CLI 仅支持 Streamable HTTP（serverUrl）或 stdio，不支持旧版 SSE。")
			}
		case strings.HasPrefix(tok, "-"):
			return "", mcpServerEntry{}, fmt.Errorf("不支持的选项：%s", tok)
		default:
			pos = append(pos, tok)
		}
	}

	if len(pos) == 0 {
		return "", mcpServerEntry{}, errors.New("请提供 MCP 名称和地址/启动命令。")
	}
	name := strings.TrimSpace(pos[0])
	if err := checkMCPName(name); err != nil {
		return "", mcpServerEntry{}, err
	}

	if hasDashDash {
		if len(afterDashDash) == 0 {
			return "", mcpServerEntry{}, errors.New("请在 -- 后提供本地 MCP 启动命令。")
		}
		entry := mcpServerEntry{
			Command: afterDashDash[0],
			Args:    afterDashDash[1:],
		}
		if len(envMap) > 0 {
			entry.Env = envMap
		}
		return name, entry, nil
	}

	restPos := pos[1:]
	if len(restPos) == 0 {
		return "", mcpServerEntry{}, errors.New("请提供 MCP 的 Streamable HTTP URL 或本地启动命令。")
	}
	target := restPos[0]
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		if len(restPos) > 1 {
			return "", mcpServerEntry{}, errors.New("远程 URL 模式只需要一个地址参数；请求头请用 -H \"Header: Value\"。")
		}
		entry := mcpServerEntry{
			ServerURL: target,
		}
		if len(headersMap) > 0 {
			entry.Headers = headersMap
		}
		return name, entry, nil
	}

	entry := mcpServerEntry{
		Command: restPos[0],
		Args:    restPos[1:],
	}
	if len(envMap) > 0 {
		entry.Env = envMap
	}
	return name, entry, nil
}

func (c *Client) runAdmin(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	bin := c.bin()
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Env = envWithoutSecrets()
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		cmd.Dir = home
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	combined := strings.TrimSpace(ansiRE.ReplaceAllString(stdout.String(), ""))
	if combined == "" {
		combined = strings.TrimSpace(ansiRE.ReplaceAllString(stderr.String(), ""))
	} else if se := strings.TrimSpace(ansiRE.ReplaceAllString(stderr.String(), "")); se != "" {
		combined = combined + "\n" + se
	}
	if err != nil {
		if runCtx.Err() != nil {
			return combined, runCtx.Err()
		}
		detail := firstLine(RedactSensitive(stderr.String()))
		if detail == "" {
			detail = firstLine(RedactSensitive(stdout.String()))
		}
		if detail != "" {
			return combined, &RunError{Message: detail}
		}
		return combined, err
	}
	return combined, nil
}
