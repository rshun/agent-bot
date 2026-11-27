package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/rshun/agent-bot/internal/config"
	"github.com/rshun/agent-bot/internal/platform"
	"github.com/rshun/agent-bot/internal/sandbox"
)

func TestParsePushArgs(t *testing.T) {
	req, err := parsePushArgs([]string{"-p", "tg", "--chat", "123", "-f", "/srv/a.csv", "盘前检查", "无异常"}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if req.Platform != "telegram" || req.ChatID != "123" || len(req.Files) != 1 || req.Files[0] != "/srv/a.csv" || req.Text != "盘前检查 无异常" {
		t.Fatalf("unexpected req: %#v", req)
	}

	pipedReq, err := parsePushArgs(nil, strings.NewReader("来自管道的通知\n"), true)
	if err != nil || pipedReq.Text != "来自管道的通知" {
		t.Fatalf("piped got %#v, err %v", pipedReq, err)
	}

	if _, err := parsePushArgs(nil, nil, false); err == nil {
		t.Fatal("expected error when both text and file are empty")
	}
}

func TestParseCLI(t *testing.T) {
	inv, err := parseCLI(nil)
	if err != nil || inv.Mode != modeDaemon {
		t.Fatalf("empty args: %#v, err=%v", inv, err)
	}

	for _, flag := range []string{"-v", "--version", "version"} {
		if inv, err := parseCLI([]string{flag}); err != nil || inv.Mode != modeVersion {
			t.Fatalf("%s: %#v, err=%v", flag, inv, err)
		}
	}
	for _, flag := range []string{"-h", "--help", "help"} {
		if inv, err := parseCLI([]string{flag}); err != nil || inv.Mode != modeHelp {
			t.Fatalf("%s: %#v, err=%v", flag, inv, err)
		}
	}

	inv, err = parseCLI([]string{"send", "-p", "dc", "升级成功"})
	if err != nil || inv.Mode != modePush || !reflect.DeepEqual(inv.PushArgs, []string{"-p", "dc", "升级成功"}) {
		t.Fatalf("send after: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"-p", "dc", "send", "升级成功"})
	if err != nil || inv.Mode != modePush || !reflect.DeepEqual(inv.PushArgs, []string{"-p", "dc", "升级成功"}) {
		t.Fatalf("flags before send: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"--platform=tg", "-f", "/srv/a.csv", "notify", "日报"})
	if err != nil || inv.Mode != modePush || !reflect.DeepEqual(inv.PushArgs, []string{"--platform=tg", "-f", "/srv/a.csv", "日报"}) {
		t.Fatalf("flags before notify: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"push", "部署完成"})
	if err != nil || inv.Mode != modePush || !reflect.DeepEqual(inv.PushArgs, []string{"部署完成"}) {
		t.Fatalf("push alias: %#v, err=%v", inv, err)
	}

	inv, err = parseCLI([]string{"run-task", "42"})
	if err != nil || inv.Mode != modeRunTask || inv.TaskID != 42 {
		t.Fatalf("run-task: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"run-task", "42", "--data-dir", "/srv/data", "--env-file", "/srv/.env"})
	if err != nil || inv.Mode != modeRunTask || inv.TaskID != 42 || inv.DataDir != "/srv/data" || inv.EnvFile != "/srv/.env" {
		t.Fatalf("run-task flags after: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"--data-dir", "/srv/data", "--env-file", "/srv/.env", "run-task", "7"})
	if err != nil || inv.Mode != modeRunTask || inv.TaskID != 7 || inv.DataDir != "/srv/data" || inv.EnvFile != "/srv/.env" {
		t.Fatalf("run-task flags before: %#v, err=%v", inv, err)
	}
	if _, err := parseCLI([]string{"run-task"}); err == nil {
		t.Fatal("expected error for bare run-task")
	}
	if _, err := parseCLI([]string{"run-task", "abc"}); err == nil {
		t.Fatal("expected error for non-numeric task id")
	}
	if _, err := parseCLI([]string{"run-task", "42", "--data-dir"}); err == nil {
		t.Fatal("expected error for missing --data-dir value")
	}

	inv, err = parseCLI([]string{"agy-hook"})
	if err != nil || inv.Mode != modeAGYHook {
		t.Fatalf("agy-hook: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"mcp-bridge", "--url", "http://127.0.0.1:41234/mcp"})
	if err != nil || inv.Mode != modeMCPBridge || inv.BridgeURL != "http://127.0.0.1:41234/mcp" {
		t.Fatalf("mcp-bridge: %#v, err=%v", inv, err)
	}
	inv, err = parseCLI([]string{"mcp-bridge", "--url=http://127.0.0.1:51234/mcp"})
	if err != nil || inv.Mode != modeMCPBridge || inv.BridgeURL != "http://127.0.0.1:51234/mcp" {
		t.Fatalf("mcp-bridge=: %#v, err=%v", inv, err)
	}
	if _, err := parseCLI([]string{"mcp-bridge"}); err == nil {
		t.Fatal("expected error for bare mcp-bridge")
	}

	for _, bad := range [][]string{
		{"--unknown-flag"},
		{"pushtest", "hello"},
		{"-p", "dc", "unknown", "hello"},
	} {
		if _, err := parseCLI(bad); err == nil {
			t.Fatalf("expected error for %v, got nil", bad)
		}
	}
}

func TestTargetPlatformsDefaultDualPush(t *testing.T) {
	ctx := context.Background()
	dualCfg := config.Config{
		TelegramToken: "placeholder-tg-val",
		DiscordToken:  "placeholder-dc-val",
		TelegramUsers: map[string]struct{}{"10001": {}},
		DiscordUsers:  map[string]struct{}{"20002": {}},
	}

	plats, err := targetPlatforms(ctx, dualCfg, nil, pushRequest{Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Telegram, platform.Discord}) {
		t.Fatalf("dual default got %v, err=%v", plats, err)
	}

	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{Platform: platform.Discord, Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Discord}) {
		t.Fatalf("explicit dc got %v, err=%v", plats, err)
	}
	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{Platform: platform.Telegram, Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Telegram}) {
		t.Fatalf("explicit tg got %v, err=%v", plats, err)
	}

	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{ChatID: "20002", Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Discord}) {
		t.Fatalf("chat infer dc got %v, err=%v", plats, err)
	}

	if _, err := targetPlatforms(ctx, dualCfg, nil, pushRequest{ChatID: "99999", Text: "hello"}); err == nil {
		t.Fatal("expected error for unknown --chat without -p on dual platform")
	}

	t.Setenv("AGENT_BOT_TASK_PLATFORM", "telegram")
	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Telegram}) {
		t.Fatalf("env task platform tg got %v, err=%v", plats, err)
	}
	plats, err = targetPlatforms(ctx, dualCfg, nil, pushRequest{Platform: platform.Discord, Text: "hello"})
	if err != nil || !reflect.DeepEqual(plats, []string{platform.Discord}) {
		t.Fatalf("explicit -p dc override got %v, err=%v", plats, err)
	}
}

func TestEnvCandidateAndResolveDataDir(t *testing.T) {
	exeDir := t.TempDir()
	paths := envCandidatePaths("", []string{exeDir})
	wantEnv := filepath.Join(exeDir, ".env")
	if paths[len(paths)-1] != wantEnv {
		t.Fatalf("expected exe .env %q in %v", wantEnv, paths)
	}

	dataDir := filepath.Join(exeDir, "data")
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "bot.db"), []byte("sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	nonExistentDefault := filepath.Join(t.TempDir(), "missing-data")
	if got := resolveDataDir(nonExistentDefault, wantEnv, []string{exeDir}); got != dataDir {
		t.Fatalf("resolveDataDir got %q, want %q", got, dataDir)
	}
}

func TestIsNoReplyPushTextAndRunPushSilentSkip(t *testing.T) {
	for _, s := range []string{"[[NO_REPLY]]", "  [[NO_REPLY]]\n", "`[[NO_REPLY]]`"} {
		if !isNoReplyPushText(s) {
			t.Fatalf("expected isNoReplyPushText(%q) == true", s)
		}
	}
	for _, s := range []string{"", "正常通知", "[[NO_REPLY]] 附带内容"} {
		if isNoReplyPushText(s) {
			t.Fatalf("expected isNoReplyPushText(%q) == false", s)
		}
	}
	if err := runPushCommand(context.Background(), config.Config{}, nil, sandbox.Policy{}, []string{"[[NO_REPLY]]"}); err != nil {
		t.Fatalf("expected silent nil return for [[NO_REPLY]], got %v", err)
	}
}
