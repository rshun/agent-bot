package antigravity

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMCPConfigLifecycleAndDoctor(t *testing.T) {
	ctx := context.Background()
	cfgPath := filepath.Join(t.TempDir(), "mcp_config.json")

	// Seed with reserved entry + custom top-level field to verify preservation & filtering.
	initial := `{"keepMe":true,"mcpServers":{"agentbot":{"command":"agent-bot","args":["mcp-bridge"]}}}`
	if err := os.WriteFile(cfgPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-secret-placeholder" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25","capabilities":{}}}`))
	}))
	defer ts.Close()

	c := &Client{MCPConfigPath: cfgPath, HTTP: ts.Client()}

	// Add remote HTTP MCP server with header.
	if _, err := c.MCPAdd(ctx, []string{"-H", "Authorization: Bearer test-secret-placeholder", "sentry", ts.URL + "/mcp"}); err != nil {
		t.Fatalf("MCPAdd http: %v", err)
	}
	// Add local stdio MCP server.
	if _, err := c.MCPAdd(ctx, []string{"-e", "TOKEN=placeholder_val", "fetch", "--", "uvx", "mcp-server-fetch"}); err != nil {
		t.Fatalf("MCPAdd stdio: %v", err)
	}
	// Reserved name rejected.
	if _, err := c.MCPAdd(ctx, []string{"agentbot", "https://example.com/mcp"}); err == nil {
		t.Fatal("expected error adding reserved agentbot")
	}

	listOut, err := c.MCPList(ctx)
	if err != nil {
		t.Fatalf("MCPList: %v", err)
	}
	if listOut != "fetch\nsentry" {
		t.Fatalf("unexpected MCPList:\n%s", listOut)
	}
	if strings.Contains(listOut, ts.URL) || strings.Contains(listOut, "test-secret-placeholder") {
		t.Fatalf("MCPList leaked URL or token:\n%s", listOut)
	}

	// Doctor probes sentry successfully without leaking headers/URL.
	docOut, err := c.MCPDoctor(ctx, "sentry")
	if err != nil {
		t.Fatalf("MCPDoctor: %v", err)
	}
	if docOut != "sentry [http]: ok" {
		t.Fatalf("unexpected MCPDoctor output: %q", docOut)
	}

	// Disable and re-check list.
	if _, err := c.MCPEnable(ctx, "sentry", false); err != nil {
		t.Fatalf("MCPEnable(false): %v", err)
	}
	listDisabled, _ := c.MCPList(ctx)
	if !strings.Contains(listDisabled, "sentry (disabled)") {
		t.Fatalf("expected disabled marker, got:\n%s", listDisabled)
	}

	// Remove fetch and confirm unknown top-level key survived.
	if _, err := c.MCPRemove(ctx, "fetch"); err != nil {
		t.Fatalf("MCPRemove: %v", err)
	}
	raw, _ := os.ReadFile(cfgPath)
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if string(top["keepMe"]) != "true" {
		t.Fatalf("extra field lost: %s", raw)
	}
}
