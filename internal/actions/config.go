package actions

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// marker is the first line of a config file the bot owns and may rewrite.
const marker = "# managed by agent-bot; do not edit"

var legacyMarkers = []string{
	marker,
	"# managed by grok-bot; do not edit",
	"# managed by codex-bot; do not edit",
}

// TokenEnv is the environment variable that carries the per-turn token to the engine CLI.
const TokenEnv = "AGENTBOT_TURN_TOKEN"

// ErrUnmanaged means the config file exists but was not written by the bot.
var ErrUnmanaged = errors.New("config.toml 不是 bot 生成的")

func hasManagedMarker(raw string) bool {
	for _, m := range legacyMarkers {
		if strings.HasPrefix(raw, m+"\n") || strings.HasPrefix(raw, m+"\r\n") {
			return true
		}
	}
	return false
}

// renderConfig returns the engine project config that points the agentbot MCP server at port.
// Codex reads bearer_token_env_var; Grok expands ${AGENTBOT_TURN_TOKEN} in headers.
func renderConfig(port int, engineName string) string {
	var b strings.Builder
	b.WriteString(marker + "\n")
	b.WriteString("[mcp_servers.agentbot]\n")
	fmt.Fprintf(&b, "url = \"http://127.0.0.1:%d/mcp\"\n", port)
	if strings.EqualFold(strings.TrimSpace(engineName), "codex") {
		b.WriteString("bearer_token_env_var = \"" + TokenEnv + "\"\n")
	} else {
		b.WriteString("headers = { Authorization = \"Bearer ${" + TokenEnv + "}\" }\n")
	}
	return b.String()
}

// WriteConfig writes the default (grok-style) project config at path.
func WriteConfig(path string, port int) error {
	return WriteConfigForEngine(path, port, "grok")
}

// WriteConfigForEngine writes the engine project config at path. It refuses to replace a
// file the bot did not write (accepting legacy grok-bot / codex-bot markers for seamless upgrades).
func WriteConfigForEngine(path string, port int, engineName string) error {
	current, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !hasManagedMarker(string(current)) {
			return ErrUnmanaged
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	content := renderConfig(port, engineName)
	if string(current) == content {
		return nil
	}
	return writeAtomic(path, content)
}

// writeAtomic replaces the file at path in one rename.
func writeAtomic(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config-*.toml")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
