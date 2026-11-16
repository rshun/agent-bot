package actions

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteConfigCreatesAndUpdatesBothEngines(t *testing.T) {
	dir := t.TempDir()
	grokPath := filepath.Join(dir, ".grok", "config.toml")
	if err := WriteConfigForEngine(grokPath, 41234, "grok"); err != nil {
		t.Fatalf("first write grok: %v", err)
	}
	raw, err := os.ReadFile(grokPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	for _, want := range []string{"[mcp_servers.agentbot]", "127.0.0.1:41234/mcp", "${AGENTBOT_TURN_TOKEN}"} {
		if !strings.Contains(text, want) {
			t.Fatalf("grok config missing %q in:\n%s", want, text)
		}
	}

	// Simulate upgrading from legacy grok-bot marker.
	legacy := "# managed by grok-bot; do not edit\n[mcp_servers.grokbot]\nurl = \"http://127.0.0.1:40000/mcp\"\n"
	if err := os.WriteFile(grokPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigForEngine(grokPath, 50001, "grok"); err != nil {
		t.Fatalf("upgrade from legacy marker: %v", err)
	}
	raw, _ = os.ReadFile(grokPath)
	if !strings.Contains(string(raw), "127.0.0.1:50001/mcp") {
		t.Fatalf("port was not updated:\n%s", raw)
	}

	codexPath := filepath.Join(dir, ".codex", "config.toml")
	if err := WriteConfigForEngine(codexPath, 41235, "codex"); err != nil {
		t.Fatalf("write codex: %v", err)
	}
	codexRaw, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	codexText := string(codexRaw)
	for _, want := range []string{"[mcp_servers.agentbot]", "127.0.0.1:41235/mcp", `bearer_token_env_var = "AGENTBOT_TURN_TOKEN"`} {
		if !strings.Contains(codexText, want) {
			t.Fatalf("codex config missing %q in:\n%s", want, codexText)
		}
	}
}

func TestWriteConfigRefusesUnmanaged(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".grok", "config.toml")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	const handWritten = "[mcp_servers.mine]\ncommand = \"echo\"\n"
	if err := os.WriteFile(path, []byte(handWritten), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfig(path, 41234); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("err = %v, want ErrUnmanaged", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != handWritten {
		t.Fatalf("unmanaged file was modified:\n%s", raw)
	}
}
