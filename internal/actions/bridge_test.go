package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAntigravityMCPConfig(t *testing.T) {
	ws := t.TempDir()
	botBin := filepath.Join(t.TempDir(), "agent-bot")

	if err := WriteAntigravityMCPConfig(ws, botBin, 41234); err != nil {
		t.Fatalf("initial write: %v", err)
	}
	cfgPath := filepath.Join(ws, ".agents", "mcp_config.json")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Tools   map[string]struct {
				Eager bool `json:"eager"`
			} `json:"tools"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	srv, ok := parsed.MCPServers["agentbot"]
	if !ok {
		t.Fatalf("missing agentbot entry: %s", raw)
	}
	if srv.Command != botBin || len(srv.Args) != 3 || srv.Args[0] != "mcp-bridge" || srv.Args[2] != "http://127.0.0.1:41234/mcp" {
		t.Fatalf("unexpected server entry: %#v", srv)
	}
	for _, toolName := range []string{"react", "send_file", "pronounce", "send_web_image"} {
		if !srv.Tools[toolName].Eager {
			t.Fatalf("expected eager=true for tool %q", toolName)
		}
	}

	// Updating port succeeds for bot-managed config.
	if err := WriteAntigravityMCPConfig(ws, botBin, 51234); err != nil {
		t.Fatalf("update port: %v", err)
	}
	updated, _ := os.ReadFile(cfgPath)
	if !strings.Contains(string(updated), "127.0.0.1:51234/mcp") {
		t.Fatalf("port not updated: %s", updated)
	}

	// Unmanaged custom server must not be overwritten.
	manual := `{"mcpServers":{"custom":{"command":"echo"}}}`
	if err := os.WriteFile(cfgPath, []byte(manual), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteAntigravityMCPConfig(ws, botBin, 51234); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("got err=%v, want ErrUnmanaged", err)
	}
	after, _ := os.ReadFile(cfgPath)
	if string(after) != manual {
		t.Fatalf("unmanaged config overwritten: %s", after)
	}
}

func TestRunStdioBridge(t *testing.T) {
	srv, err := Listen("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	var reacted string
	env, end := srv.Begin(&Turn{
		React: func(_ context.Context, emoji string) error {
			reacted = emoji
			return nil
		},
	})
	defer end()
	token := strings.TrimPrefix(env, TokenEnv+"=")

	inLines := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized","params":{}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"react","arguments":{"emoji":"👍"}}}`,
	}, "\n") + "\n"

	var out bytes.Buffer
	if err := RunStdioBridge(context.Background(), strings.NewReader(inLines), &out, ts.URL+"/mcp", token, ts.Client()); err != nil {
		t.Fatalf("RunStdioBridge: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d reply lines (notification should produce none):\n%s", len(lines), out.String())
	}
	if !strings.Contains(lines[0], `"agentbot"`) {
		t.Fatalf("unexpected initialize reply: %s", lines[0])
	}
	if !strings.Contains(lines[1], `"react"`) {
		t.Fatalf("unexpected tools/list reply: %s", lines[1])
	}
	if !strings.Contains(lines[2], `"Reacted."`) || reacted != "👍" {
		t.Fatalf("unexpected tools/call reply=%s reacted=%q", lines[2], reacted)
	}

	// Unauthorized token returns a JSON-RPC error instead of hanging.
	var unauthOut bytes.Buffer
	err = RunStdioBridge(
		context.Background(),
		strings.NewReader(`{"jsonrpc":"2.0","id":9,"method":"ping"}`+"\n"),
		&unauthOut,
		fmt.Sprintf("%s/mcp", ts.URL),
		"wrong-token",
		ts.Client(),
	)
	if err != nil {
		t.Fatalf("unauthorized bridge error: %v", err)
	}
	if !strings.Contains(unauthOut.String(), `"error"`) || !strings.Contains(unauthOut.String(), `"id":9`) {
		t.Fatalf("expected JSON-RPC error for unauthorized token, got: %s", unauthOut.String())
	}
}
