package actions

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
	"path/filepath"
	"strings"
	"time"
)

const maxBridgeBodyBytes = 4 << 20

// WriteAntigravityMCPConfig writes <workspaceDir>/.agents/mcp_config.json so Antigravity CLI
// invokes `agent-bot mcp-bridge` over stdio while inheriting AGENTBOT_TURN_TOKEN per turn.
// It refuses to overwrite a config containing user-managed MCP servers.
func WriteAntigravityMCPConfig(workspaceDir, botBin string, port int) error {
	if strings.TrimSpace(workspaceDir) == "" {
		return errors.New("workspaceDir is required")
	}
	if strings.TrimSpace(botBin) == "" {
		return errors.New("botBin is required")
	}
	if port <= 0 {
		return errors.New("port must be positive")
	}
	path := filepath.Join(workspaceDir, ".agents", "mcp_config.json")
	current, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !isManagedAntigravityMCPConfig(current) {
			return ErrUnmanaged
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	mcpURL := fmt.Sprintf("http://127.0.0.1:%d/mcp", port)
	payload := map[string]any{
		"mcpServers": map[string]any{
			"agentbot": map[string]any{
				"command": botBin,
				"args":    []string{"mcp-bridge", "--url", mcpURL},
				"tools": map[string]any{
					"react":          map[string]any{"eager": true},
					"send_file":      map[string]any{"eager": true},
					"pronounce":      map[string]any{"eager": true},
					"send_web_image": map[string]any{"eager": true},
				},
			},
		},
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	content := string(encoded) + "\n"
	if string(current) == content {
		return nil
	}
	return writeAtomic(path, content)
}

func isManagedAntigravityMCPConfig(raw []byte) bool {
	var parsed struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed.MCPServers == nil {
		return false
	}
	for name := range parsed.MCPServers {
		n := strings.TrimSpace(name)
		if !strings.EqualFold(n, "agentbot") &&
			!strings.EqualFold(n, "grokbot") &&
			!strings.EqualFold(n, "codexbot") {
			return false
		}
	}
	return true
}

// RunStdioBridge proxies newline-delimited JSON-RPC messages from stdin to the local
// HTTP actions MCP endpoint using the per-turn bearer token, writing replies to stdout.
func RunStdioBridge(ctx context.Context, stdin io.Reader, stdout io.Writer, mcpURL, token string, client *http.Client) error {
	mcpURL = strings.TrimSpace(mcpURL)
	if mcpURL == "" {
		return errors.New("mcp url is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Minute}
	}

	reader := bufio.NewReaderSize(stdin, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, readErr := reader.ReadBytes('\n')
		trimmed := bytes.TrimSpace(line)
		if len(trimmed) > 0 {
			reply, err := forwardJSONRPC(ctx, client, mcpURL, token, trimmed)
			if err != nil {
				if fallback := bridgeRPCError(trimmed, err.Error()); len(fallback) > 0 {
					reply = fallback
				}
			}
			if len(reply) > 0 {
				var compact bytes.Buffer
				if json.Compact(&compact, reply) == nil {
					reply = compact.Bytes()
				}
				if _, wErr := stdout.Write(append(reply, '\n')); wErr != nil {
					return wErr
				}
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return readErr
		}
	}
}

func forwardJSONRPC(ctx context.Context, client *http.Client, mcpURL, token string, payload []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, mcpURL, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBridgeBodyBytes))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(body)
	if resp.StatusCode == http.StatusAccepted || (resp.StatusCode >= 200 && resp.StatusCode < 300 && len(trimmed) == 0) {
		return nil, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if json.Valid(trimmed) {
			return trimmed, nil
		}
		return nil, fmt.Errorf("mcp bridge http %d", resp.StatusCode)
	}
	return trimmed, nil
}

func bridgeRPCError(reqPayload []byte, msg string) []byte {
	var req struct {
		ID json.RawMessage `json:"id"`
	}
	if json.Unmarshal(reqPayload, &req) != nil || len(req.ID) == 0 || string(bytes.TrimSpace(req.ID)) == "null" {
		return nil
	}
	out, err := json.Marshal(rpcResponse{
		JSONRPC: "2.0",
		ID:      req.ID,
		Error:   &rpcError{Code: -32000, Message: msg},
	})
	if err != nil {
		return nil
	}
	return out
}
