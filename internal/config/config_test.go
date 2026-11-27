package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func clearEngineEnvs(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"BOT_ENGINE", "ENGINE_BIN", "ENGINE_HOME", "AUTH_PATH", "ENGINE_TIMEOUT", "ENGINE_MAX_TURNS", "BOT_MODELS",
		"GROK_BIN", "GROK_MODEL", "GROK_MODELS", "GROK_HOME", "GROK_AUTH_PATH", "GROK_TIMEOUT", "GROK_MAX_TURNS",
		"CODEX_BIN", "CODEX_MODEL", "CODEX_MODELS", "CODEX_HOME", "CODEX_AUTH_PATH", "CODEX_TIMEOUT", "CODEX_TIMEOUT_MS", "CODEX_MAX_TURNS",
		"AGY_BIN", "AGY_MODEL", "AGY_MODELS", "AGY_HOME", "AGY_AUTH_PATH", "AGY_TIMEOUT", "AGY_MAX_TURNS",
		"ANTIGRAVITY_BIN", "ANTIGRAVITY_MODEL", "ANTIGRAVITY_MODELS", "ANTIGRAVITY_HOME", "ANTIGRAVITY_AUTH_PATH", "ANTIGRAVITY_TIMEOUT", "ANTIGRAVITY_MAX_TURNS",
		"USER", "LOGNAME",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadRequiresOnePlatform(t *testing.T) {
	clearEngineEnvs(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "")
	t.Setenv("ALLOWED_USER_ID", "")
	t.Setenv("DISCORD_ALLOWED_USER_IDS", "")
	t.Setenv("DISCORD_ALLOWED_USER_ID", "")
	if _, err := Load(); err == nil {
		t.Fatal("expected error")
	}
}

func TestLoadEngineRoutingAndLegacyEnvCompat(t *testing.T) {
	clearEngineEnvs(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "placeholder-tg")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "")
	t.Setenv("ALLOWED_USER_ID", "12345")
	t.Setenv("DEFAULT_MODEL", "")

	// Default -> grok
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "grok" || cfg.DefaultModel != "grok-4.7" || cfg.EngineBin != "grok" || cfg.EnvFile != "/etc/agent-bot/grok.env" {
		t.Fatalf("unexpected default grok config: %#v", cfg)
	}

	// Auto-detect codex from legacy CODEX_* env vars
	t.Setenv("CODEX_MODEL", "o3")
	t.Setenv("CODEX_MODELS", "gpt-5.4, o3, gpt-5.4")
	t.Setenv("CODEX_TIMEOUT_MS", "180000")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "codex" || cfg.DefaultModel != "o3" || cfg.EngineBin != "codex" || cfg.EnvFile != "/etc/agent-bot/codex.env" {
		t.Fatalf("unexpected codex config: %#v", cfg)
	}
	if len(cfg.CandidateModels) != 2 || cfg.CandidateModels[0] != "gpt-5.4" || cfg.CandidateModels[1] != "o3" {
		t.Fatalf("CandidateModels=%#v", cfg.CandidateModels)
	}
	if cfg.EngineTimeout != 3*time.Minute {
		t.Fatalf("EngineTimeout=%v", cfg.EngineTimeout)
	}

	// Auto-detect antigravity from AGY_* env vars and explicit BOT_ENGINE=agy alias.
	clearEngineEnvs(t)
	t.Setenv("AGY_MODEL", "gemini-4-neon-high")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "antigravity" || cfg.DefaultModel != "gemini-4-neon-high" || cfg.EngineBin != "agy" || cfg.EnvFile != "/etc/agent-bot/antigravity.env" {
		t.Fatalf("unexpected auto-detected antigravity config: %#v", cfg)
	}
	clearEngineEnvs(t)
	t.Setenv("BOT_ENGINE", "agy")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Engine != "antigravity" || cfg.DefaultModel != defaultAntigravityModel || cfg.EngineBin != "agy" {
		t.Fatalf("unexpected agy alias config: %#v", cfg)
	}

	// Explicit invalid BOT_ENGINE rejected
	t.Setenv("BOT_ENGINE", "unknown-engine")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid BOT_ENGINE")
	}
}

func TestLoadDiscordStripsBotPrefixAndAdminIDs(t *testing.T) {
	clearEngineEnvs(t)
	t.Setenv("BOT_ENGINE", "grok")
	t.Setenv("TELEGRAM_BOT_TOKEN", "placeholder-tg")
	t.Setenv("ALLOWED_USER_IDS", "10,20")
	t.Setenv("ADMIN_USER_IDS", "10")
	t.Setenv("DISCORD_BOT_TOKEN", "Bot abc.def")
	t.Setenv("DISCORD_ALLOWED_USER_IDS", "99,100")
	t.Setenv("DISCORD_ADMIN_USER_IDS", "100")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DiscordToken != "abc.def" {
		t.Fatalf("%q", cfg.DiscordToken)
	}
	if _, ok := cfg.TelegramAdmins["10"]; !ok || len(cfg.TelegramAdmins) != 1 {
		t.Fatalf("TelegramAdmins=%#v", cfg.TelegramAdmins)
	}
	if _, ok := cfg.DiscordAdmins["100"]; !ok || len(cfg.DiscordAdmins) != 1 {
		t.Fatalf("DiscordAdmins=%#v", cfg.DiscordAdmins)
	}

	t.Setenv("ADMIN_USER_IDS", "not-a-number")
	if _, err := Load(); err == nil {
		t.Fatal("expected error for invalid ADMIN_USER_IDS")
	}
}

func TestLoadEnvFileAndUserProfile(t *testing.T) {
	clearEngineEnvs(t)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("DEFAULT_MODEL=gpt-5.4-mini\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BOT_ENV_FILE", "")
	t.Setenv("DEFAULT_MODEL", "")
	loaded := LoadEnvFile("", filepath.Join(dir, "missing.env"), envPath)
	if loaded != envPath || os.Getenv("BOT_ENV_FILE") != envPath || os.Getenv("DEFAULT_MODEL") != "gpt-5.4-mini" {
		t.Fatalf("loaded=%q BOT_ENV_FILE=%q DEFAULT_MODEL=%q", loaded, os.Getenv("BOT_ENV_FILE"), os.Getenv("DEFAULT_MODEL"))
	}

	home := t.TempDir()
	profile := strings.Join([]string{
		`# user profile`,
		`export MCP_EXAMPLE_VAR="placeholder-mcp-a"`,
		`export MCP_LITERAL_VAR='placeholder$literal'`,
		`UNEXPORTED_MCP_VAR=plain_val # inline comment`,
		`export EXISTING_BOT_VAR="from_profile"`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(home, ".bash_profile"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MCP_EXAMPLE_VAR", "")
	t.Setenv("MCP_LITERAL_VAR", "")
	t.Setenv("UNEXPORTED_MCP_VAR", "")
	t.Setenv("EXISTING_BOT_VAR", "from_systemd")

	LoadUserProfile(home)

	if got := os.Getenv("MCP_EXAMPLE_VAR"); got != "placeholder-mcp-a" {
		t.Fatalf("MCP_EXAMPLE_VAR = %q", got)
	}
	if got := os.Getenv("MCP_LITERAL_VAR"); got != "placeholder$literal" {
		t.Fatalf("MCP_LITERAL_VAR = %q", got)
	}
	if got := os.Getenv("UNEXPORTED_MCP_VAR"); got != "plain_val" {
		t.Fatalf("UNEXPORTED_MCP_VAR = %q", got)
	}
	if got := os.Getenv("EXISTING_BOT_VAR"); got != "from_systemd" {
		t.Fatalf("EXISTING_BOT_VAR overwritten: %q", got)
	}
}

func TestLoadAutoUpgradeAndTaskSettings(t *testing.T) {
	clearEngineEnvs(t)
	t.Setenv("TELEGRAM_BOT_TOKEN", "placeholder-tg")
	t.Setenv("DISCORD_BOT_TOKEN", "")
	t.Setenv("ALLOWED_USER_IDS", "1")
	t.Setenv("AUTO_UPGRADE", "")
	t.Setenv("AUTO_UPGRADE_TIME", "")
	t.Setenv("UPGRADE_REPO", "")
	t.Setenv("NPM_BIN", "")
	t.Setenv("ENGINE_MAX_TURNS", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.AutoUpgrade || cfg.AutoUpgradeTime != "04:30" || cfg.UpgradeRepo != "rshun/agent-bot" || cfg.NPMBin != "npm" || cfg.EngineMaxTurns != -1 {
		t.Fatalf("unexpected defaults: %#v", cfg)
	}

	t.Setenv("AUTO_UPGRADE", "true")
	t.Setenv("AUTO_UPGRADE_TIME", "5:05")
	t.Setenv("UPGRADE_REPO", "example/agent-bot")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.AutoUpgrade || cfg.AutoUpgradeTime != "05:05" || cfg.UpgradeRepo != "example/agent-bot" {
		t.Fatalf("unexpected custom upgrade config: %#v", cfg)
	}

	for _, bad := range []struct{ env, val string }{
		{"AUTO_UPGRADE", "not-a-bool"},
		{"AUTO_UPGRADE_TIME", "25:00"},
		{"AUTO_UPGRADE_TIME", "0430"},
		{"ENGINE_MAX_TURNS", "-1"},
	} {
		t.Run(bad.env+"="+bad.val, func(t *testing.T) {
			t.Setenv(bad.env, bad.val)
			if _, err := Load(); err == nil {
				t.Fatalf("expected error for %s=%q", bad.env, bad.val)
			}
		})
	}
}
