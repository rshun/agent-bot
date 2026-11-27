package antigravity

import (
	"os"
	"slices"
	"strings"
	"testing"
)

func TestBuildArgsAndPrompt(t *testing.T) {
	c := &Client{
		DefaultModel: DefaultModels[0],
		InboxDir:     t.TempDir(),
		Workspace:    t.TempDir(),
	}

	t.Run("new session with write dirs and attached image", func(t *testing.T) {
		args, prompt, cleanup, err := c.buildArgs(ReplyOptions{
			Model:      "gemini-4-neon-high",
			Prompt:     "check this",
			WebSearch:  true,
			WriteDirs:  []string{"/srv/work"},
			ShellTools: true,
			Memories:   []string{"Prefers Go", "line1\nline2"},
			React:      true,
			Images:     []Image{{MIME: "image/png", Data: []byte("PNG")}},
		})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		for _, want := range []string{"--input-format", "stream-json", "--output-format", "stream-json", "--disable-slash-commands", "--model", "gemini-4-neon-high", "--add-dir", "/srv/work", c.InboxDir} {
			if !slices.Contains(args, want) {
				t.Fatalf("missing arg %q in %v", want, args)
			}
		}
		if slices.Contains(args, "--conversation") {
			t.Fatalf("did not expect --conversation on new session: %v", args)
		}
		for _, wantText := range []string{"- Prefers Go", "- line1 line2", "agentbot__react", "agy-img-", "check this"} {
			if !strings.Contains(prompt, wantText) {
				t.Fatalf("expected %q in prompt:\n%s", wantText, prompt)
			}
		}
	})

	t.Run("resume session without write dirs", func(t *testing.T) {
		args, prompt, cleanup, err := c.buildArgs(ReplyOptions{
			SessionID: "conv-abc",
			Model:     "gemini-4-hydrogen-medium",
			Prompt:    "hi",
		})
		if err != nil {
			t.Fatal(err)
		}
		defer cleanup()

		if !slices.Contains(args, "--conversation") || !slices.Contains(args, "conv-abc") {
			t.Fatalf("expected --conversation conv-abc in %v", args)
		}
		if !strings.Contains(prompt, "/dir add rw /absolute/path") || !strings.Contains(prompt, "/shell on") {
			t.Fatalf("expected /dir and /shell guidance in prompt:\n%s", prompt)
		}
		if strings.Contains(prompt, "via /auth on") || strings.Contains(prompt, "/search on") {
			t.Fatalf("unexpected legacy command hint in prompt:\n%s", prompt)
		}
	})

	t.Run("json schema writes temp file and cleans up", func(t *testing.T) {
		args, prompt, cleanup, err := c.buildArgs(ReplyOptions{
			Model:      "gemini-4-hydrogen-medium",
			Prompt:     "every day at 9am",
			JSONSchema: `{"type":"object"}`,
		})
		if err != nil {
			t.Fatal(err)
		}
		idx := slices.Index(args, "--json-schema")
		if idx < 0 || idx+1 >= len(args) {
			cleanup()
			t.Fatalf("missing --json-schema in %v", args)
		}
		schemaPath := args[idx+1]
		data, err := os.ReadFile(schemaPath)
		if err != nil || string(data) != `{"type":"object"}` {
			cleanup()
			t.Fatalf("unexpected schema file %q err=%v", string(data), err)
		}
		if !strings.Contains(prompt, "Do not call any tools") {
			cleanup()
			t.Fatalf("expected tool-free instruction in schema prompt: %s", prompt)
		}
		cleanup()
		if _, err := os.Stat(schemaPath); !os.IsNotExist(err) {
			t.Fatalf("schema file not cleaned up")
		}
	})
}

func TestEnvWithoutSecretsScrubsTokens(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "placeholder_tg_token")
	t.Setenv("DISCORD_BOT_TOKEN", "placeholder_dc_token")
	t.Setenv("DATA_DIR", "/tmp/data")
	t.Setenv("SAFE_CUSTOM_VAR", "keep_me")

	env := envWithoutSecrets()
	for _, kv := range env {
		if strings.HasPrefix(kv, "TELEGRAM_BOT_TOKEN=") || strings.HasPrefix(kv, "DISCORD_BOT_TOKEN=") || strings.HasPrefix(kv, "DATA_DIR=") {
			t.Fatalf("secret or bot config leaked into subprocess env: %s", kv)
		}
	}
	if !slices.Contains(env, "SAFE_CUSTOM_VAR=keep_me") {
		t.Fatalf("expected SAFE_CUSTOM_VAR preserved")
	}
}
