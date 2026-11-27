package antigravity

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureWorkspaceHooksAndRules(t *testing.T) {
	ws := t.TempDir()
	botBin := filepath.Join(t.TempDir(), "agent-bot")

	if err := EnsureWorkspaceHooks(ws, botBin); err != nil {
		t.Fatalf("EnsureWorkspaceHooks: %v", err)
	}
	hooksPath := filepath.Join(ws, ".agents", "hooks.json")
	rawHooks, err := os.ReadFile(hooksPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawHooks), managedHookName) || !strings.Contains(string(rawHooks), "agy-hook") {
		t.Fatalf("unexpected hooks.json:\n%s", rawHooks)
	}

	// Refuse overwriting user-managed hooks.json.
	manualHooks := `{"custom-hook":{"PreToolUse":[]}}`
	if err := os.WriteFile(hooksPath, []byte(manualHooks), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureWorkspaceHooks(ws, botBin); !errors.Is(err, ErrUnmanagedConfig) {
		t.Fatalf("expected ErrUnmanagedConfig for manual hooks, got %v", err)
	}

	if err := EnsureWorkspaceRules(ws); err != nil {
		t.Fatalf("EnsureWorkspaceRules: %v", err)
	}
	rulesPath := filepath.Join(ws, ".agents", "GEMINI.md")
	rawRules, err := os.ReadFile(rulesPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(rawRules), managedRulesHead+"\n") || !strings.Contains(string(rawRules), "/dir add rw") {
		t.Fatalf("unexpected GEMINI.md:\n%s", rawRules)
	}
}

func runHookCase(t *testing.T, policyJSON string, payload string) hookOutput {
	t.Helper()
	var out bytes.Buffer
	if err := RunPreToolHook(strings.NewReader(payload), &out, policyJSON); err != nil {
		t.Fatalf("RunPreToolHook: %v", err)
	}
	var dec hookOutput
	if err := json.Unmarshal(out.Bytes(), &dec); err != nil {
		t.Fatalf("unmarshal hook output %q: %v", out.String(), err)
	}
	return dec
}

func TestRunPreToolHookDecisions(t *testing.T) {
	ws := t.TempDir()
	rwDir := filepath.Join(ws, "allowed")
	if err := os.MkdirAll(rwDir, 0o750); err != nil {
		t.Fatal(err)
	}
	deniedEnv := filepath.Join(ws, ".env")

	policyBytes, _ := json.Marshal(TurnPolicy{
		ShellTools: false,
		WebSearch:  false,
		WriteDirs:  []string{rwDir},
		DenyPaths:  []string{"**/.env", "**/*.pem"},
	})
	pol := string(policyBytes)

	// Missing policy fails closed.
	if dec := runHookCase(t, "", `{"toolCall":{"name":"view_file","args":{"AbsolutePath":"/tmp/a"}}}`); dec.Decision != "deny" {
		t.Fatalf("missing policy should deny, got %#v", dec)
	}

	// Shell denied when ShellTools=false.
	if dec := runHookCase(t, pol, `{"toolCall":{"name":"run_command","args":{"CommandLine":"ls"}}}`); dec.Decision != "deny" || !strings.Contains(dec.Reason, "/shell on") {
		t.Fatalf("shell should deny with /shell on hint, got %#v", dec)
	}

	// Web search denied when WebSearch=false.
	if dec := runHookCase(t, pol, `{"toolCall":{"name":"search_web","args":{"query":"news"}}}`); dec.Decision != "deny" {
		t.Fatalf("search_web should deny, got %#v", dec)
	}

	// Write inside WriteDirs approved.
	okWrite, _ := json.Marshal(map[string]any{
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{"TargetFile": filepath.Join(rwDir, "out.txt")},
		},
	})
	if dec := runHookCase(t, pol, string(okWrite)); dec.Decision != "auto_approve" {
		t.Fatalf("write inside WriteDirs should auto_approve, got %#v", dec)
	}

	// Write outside WriteDirs denied with /dir hint and never /auth on.
	badWrite, _ := json.Marshal(map[string]any{
		"toolCall": map[string]any{
			"name": "write_to_file",
			"args": map[string]any{"TargetFile": filepath.Join(ws, "outside.txt")},
		},
	})
	if dec := runHookCase(t, pol, string(badWrite)); dec.Decision != "deny" || !strings.Contains(dec.Reason, "/dir add rw") {
		t.Fatalf("outside write should deny with /dir hint, got %#v", dec)
	}

	// Denied pattern (.env) blocked on both read and write.
	envRead, _ := json.Marshal(map[string]any{
		"toolCall": map[string]any{
			"name": "view_file",
			"args": map[string]any{"AbsolutePath": deniedEnv},
		},
	})
	if dec := runHookCase(t, pol, string(envRead)); dec.Decision != "deny" {
		t.Fatalf(".env read should deny, got %#v", dec)
	}

	// JSONSchemaOnly blocks all tools.
	schemaPol, _ := json.Marshal(TurnPolicy{JSONSchemaOnly: true, ShellTools: true, WebSearch: true})
	if dec := runHookCase(t, string(schemaPol), `{"toolCall":{"name":"view_file","args":{"AbsolutePath":"/tmp/a"}}}`); dec.Decision != "deny" {
		t.Fatalf("JSONSchemaOnly should deny all tools, got %#v", dec)
	}
}
