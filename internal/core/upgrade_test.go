package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rshun/agent-bot/internal/engine"
	"github.com/rshun/agent-bot/internal/platform"
	"github.com/rshun/agent-bot/internal/store"
	"github.com/rshun/agent-bot/internal/upgrade"
)

type fakeCLIUpgraderEngine struct {
	memoryTestEngine
	displayName string
	cliVer      string
	latestVer   string
	directReady bool
	writable    bool
	writeHint   string
	models      []string
	updateCalls int
	restarts    int
}

func (f *fakeCLIUpgraderEngine) EngineName() string { return "codex" }

func (f *fakeCLIUpgraderEngine) DisplayName() string {
	if f.displayName != "" {
		return f.displayName
	}
	return "Codex CLI"
}

func (f *fakeCLIUpgraderEngine) Version(context.Context) (string, error) {
	if f.cliVer == "" {
		return "0.118.0", nil
	}
	return f.cliVer, nil
}

func (f *fakeCLIUpgraderEngine) ResetLeader(context.Context)                 { f.restarts++ }
func (f *fakeCLIUpgraderEngine) MCPList(context.Context) (string, error)     { return "", nil }
func (f *fakeCLIUpgraderEngine) MCPDoctor(context.Context, string) (string, error) {
	return "OK", nil
}
func (f *fakeCLIUpgraderEngine) MCPAdd(context.Context, []string) (string, error) { return "", nil }
func (f *fakeCLIUpgraderEngine) MCPRemove(context.Context, string) (string, error) {
	return "", nil
}
func (f *fakeCLIUpgraderEngine) MCPEnable(context.Context, string, bool) (string, error) {
	return "", nil
}

func (f *fakeCLIUpgraderEngine) CheckCLIUpgrade(context.Context) engine.CLIStatus {
	cur := f.cliVer
	if cur == "" {
		cur = "0.118.0"
	}
	updateAvail := f.latestVer != "" && f.latestVer != cur
	return engine.CLIStatus{
		Current:           cur,
		Latest:            f.latestVer,
		UpdateAvailable:   updateAvail,
		DirectUpdateReady: f.directReady,
		Path:              "/home/codex/bin/codex",
		Writable:          f.writable,
		WriteHint:         f.writeHint,
	}
}

func (f *fakeCLIUpgraderEngine) UpgradeCLI(context.Context) (engine.CLIUpgradeResult, error) {
	f.updateCalls++
	oldVer := f.cliVer
	if oldVer == "" {
		oldVer = "0.118.0"
	}
	newVer := f.latestVer
	if newVer == "" {
		newVer = oldVer
	}
	f.cliVer = newVer
	return engine.CLIUpgradeResult{
		Changed:    oldVer != newVer,
		OldVersion: oldVer,
		NewVersion: newVer,
		TargetPath: "/home/codex/bin/codex",
	}, nil
}

func (f *fakeCLIUpgraderEngine) Models(context.Context) (engine.ModelList, error) {
	if len(f.models) > 0 {
		return engine.ModelList{Names: f.models}, nil
	}
	return engine.ModelList{Names: []string{"gpt-5.4"}}, nil
}

func TestUpgradeAdminAuthorization(t *testing.T) {
	eng := &fakeCLIUpgraderEngine{
		displayName: "Grok CLI",
		cliVer:      "0.4.1",
		directReady: true,
		writable:    true,
	}
	svc, _ := newMemoryService(t, eng)
	svc.Version = "v1.0.0"
	binPath := filepath.Join(t.TempDir(), "agent-bot")
	_ = os.WriteFile(binPath, []byte("old"), 0o755)
	svc.Upgrader = &upgrade.Upgrader{
		CurrentVersion: "dev",
		Paths:          upgrade.ExePaths{LaunchPath: binPath, TargetPath: binPath},
		GOOS:           "linux",
		GOARCH:         "amd64",
	}

	// 1. Single-owner dual-platform (1 TG user + 1 Discord user, no ADMIN_USER_IDS): both allowed.
	svc.AllowedUsers = map[string]map[string]struct{}{
		platform.Telegram: {"tg-owner": {}},
		platform.Discord:  {"dc-owner": {}},
	}
	if got := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c1", UserID: "tg-owner", Text: "/upgrade cli"}); !strings.Contains(got, "Grok CLI") {
		t.Fatalf("single-owner TG denied: %s", got)
	}
	if got := sendSync(t, svc, platform.Inbound{Platform: platform.Discord, ChatID: "dc1", UserID: "dc-owner", Text: "/upgrade cli"}); !strings.Contains(got, "Grok CLI") {
		t.Fatalf("single-owner Discord denied: %s", got)
	}

	// 2. Multi-user shared bot without ADMIN_USER_IDS: refuses until admin is configured.
	svc.AllowedUsers[platform.Telegram] = map[string]struct{}{"u1": {}, "u2": {}}
	gotShared := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade"})
	if !strings.Contains(gotShared, "ADMIN_USER_IDS") {
		t.Fatalf("expected ADMIN_USER_IDS prompt for multi-user setup, got: %s", gotShared)
	}

	// 3. Multi-user shared bot with explicit ADMIN_USER_IDS={"u1"}: u2 denied, u1 allowed.
	svc.AdminUsers = map[string]map[string]struct{}{
		platform.Telegram: {"u1": {}},
	}
	gotDenied := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c2", UserID: "u2", Text: "/upgrade"})
	if !strings.Contains(gotDenied, "仅管理员可执行版本升级") {
		t.Fatalf("non-admin u2 was not denied: %s", gotDenied)
	}
	gotAdmin := sendSync(t, svc, platform.Inbound{Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade cli"})
	if !strings.Contains(gotAdmin, "Grok CLI") {
		t.Fatalf("admin u1 failed: %s", gotAdmin)
	}
}

func TestParseNaturalUpgradeIntentAndSafety(t *testing.T) {
	eng := &fakeCLIUpgraderEngine{cliVer: "0.118.0", latestVer: "0.120.0", writable: true}
	svc, _ := newMemoryService(t, eng)
	svc.Version = "dev"

	// Natural language triggers ONLY open the check panel with buttons and NEVER execute UpgradeCLI.
	var out memCapture
	text, _, _, btns, err := svc.answerWithButtons(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "升级一下", TextMessage: true,
	}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "版本与升级检查") || len(btns) == 0 || eng.updateCalls != 0 {
		t.Fatalf("expected check panel without auto-execution, text=%q btns=%d calls=%d", text, len(btns), eng.updateCalls)
	}

	cases := map[string]string{
		"升级机器人":          "bot",
		"更新 agent-bot":   "bot",
		"升级 grok-bot":    "bot",
		"升级 codex-bot":   "bot",
		"更新 Grok CLI！":        "cli",
		"升级 Codex CLI":        "cli",
		"升级 agy":              "cli",
		"update antigravity cli": "cli",
		"check for updates":     "",
		"upgrade bot":       "bot",
		"update codex cli":  "cli",
	}
	for phrase, wantFocus := range cases {
		if focus, ok := parseNaturalUpgradeIntent(phrase); !ok || focus != wantFocus {
			t.Fatalf("%q => (%q, %v), want (%q, true)", phrase, focus, ok, wantFocus)
		}
	}

	// Technical questions must NOT trigger self-upgrade panel.
	for _, question := range []string{
		"Ubuntu 怎么升级内核？",
		"帮我写一个自动升级脚本",
		"Python pip 怎么更新？",
		"how to update ubuntu",
		"https://example.com 升级一下",
	} {
		if _, ok := parseNaturalUpgradeIntent(question); ok {
			t.Fatalf("technical question %q falsely matched natural upgrade intent", question)
		}
	}
}

func TestUpgradeCLICommandAndDeprecatedModelAlert(t *testing.T) {
	eng := &fakeCLIUpgraderEngine{
		displayName: "Grok CLI",
		cliVer:      "0.4.1",
		latestVer:   "0.4.5",
		writable:    true,
		models:      []string{"grok-4.8", "grok-4.8-mini"},
	}
	svc, st := newMemoryService(t, eng)
	_ = st.SetModel(context.Background(), platform.Telegram, "c1", "grok-4.7")

	var out memCapture
	reply, _, _, btns, err := svc.answerWithButtons(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "c1", UserID: "u1", Text: "/upgrade confirm cli", TextMessage: true,
	}, &out, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "0.4.1") || !strings.Contains(reply, "0.4.5") {
		t.Fatalf("missing version diff in reply: %s", reply)
	}
	if !strings.Contains(reply, "grok-4.7") || !strings.Contains(reply, "下线或更名") || len(btns) == 0 {
		t.Fatalf("expected deprecated model alert & buttons, reply=%s btns=%d", reply, len(btns))
	}
}

func TestUpgradePanelConfirmAutoAndPostRestartNotice(t *testing.T) {
	payload := append([]byte{0x7f, 'E', 'L', 'F'}, []byte("-agent-bot-v1.2.0-binary")...)
	sum := sha256.Sum256(payload)
	sumHex := hex.EncodeToString(sum[:])

	var srv *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/rshun/agent-bot/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name":   "v1.2.0",
			"html_url":   srv.URL + "/releases/tag/v1.2.0",
			"draft":      false,
			"prerelease": false,
			"assets": []map[string]string{
				{"name": "agent-bot-linux-amd64", "browser_download_url": srv.URL + "/dl/agent-bot-linux-amd64"},
				{"name": "SHA256SUMS", "browser_download_url": srv.URL + "/dl/SHA256SUMS"},
			},
		})
	})
	mux.HandleFunc("/dl/agent-bot-linux-amd64", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(payload)
	})
	mux.HandleFunc("/dl/SHA256SUMS", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  agent-bot-linux-amd64\n", sumHex)
	})
	srv = httptest.NewServer(mux)
	defer srv.Close()

	dataDir := filepath.Join(t.TempDir(), "data")
	st, err := store.Open(dataDir, "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "agent-bot")
	if err := os.WriteFile(binPath, []byte("old-v1.1.0"), 0o755); err != nil {
		t.Fatal(err)
	}

	eng := &fakeCLIUpgraderEngine{
		displayName: "Codex CLI",
		cliVer:      "0.118.0",
		latestVer:   "0.120.0",
		writable:    true,
	}
	tgOut := &buttonCaptureOut{}
	var restartedExe string
	var msgCountAtRestart int

	svc := &Service{
		Store:           st,
		Engine:          eng,
		AllowedUsers:    map[string]map[string]struct{}{platform.Telegram: {"u1": {}}},
		Senders:         map[string]platform.Replier{platform.Telegram: tgOut},
		Version:         "v1.1.0",
		BackupDir:       filepath.Join(dataDir, "backups"),
		AutoUpgrade:     false,
		AutoUpgradeTime: "04:30",
		Upgrader: &upgrade.Upgrader{
			CurrentVersion:    "v1.1.0",
			Paths:             upgrade.ExePaths{LaunchPath: binPath, TargetPath: binPath},
			GOOS:              "linux",
			GOARCH:            "amd64",
			APIBase:           srv.URL,
			HTTP:              srv.Client(),
			AllowInsecureHTTP: true,
			SkipSmokeExec:     true,
		},
		RequestRestart: func(p string) {
			restartedExe = p
			msgCountAtRestart = tgOut.count()
		},
	}

	sendTG := func(text string) (string, [][]platform.Button) {
		t.Helper()
		svc.Handle(context.Background(), platform.Inbound{
			Platform: platform.Telegram, ChatID: "tg-1", UserID: "u1", Text: text, TextMessage: true,
		}, tgOut)
		waitIdle(t, svc, platform.Telegram, "tg-1")
		return tgOut.last()
	}

	// 1. Check `/upgrade` panel displays version-locked confirmation buttons + auto toggle.
	panelText, panelBtns := sendTG("/upgrade")
	if !strings.Contains(panelText, "v1.1.0") || !strings.Contains(panelText, "v1.2.0") || !strings.Contains(panelText, "0.120.0") {
		t.Fatalf("unexpected panel:\n%s", panelText)
	}
	for _, wantCmd := range []string{"/upgrade confirm bot v1.2.0", "/upgrade confirm cli", "/upgrade confirm all v1.2.0", "/upgrade auto on"} {
		if !hasButtonCommand(panelBtns, wantCmd) {
			t.Fatalf("missing button %q in %#v", wantCmd, panelBtns)
		}
	}

	// 2. Stale tag confirmation must be rejected without modifying the binary.
	staleReply, _ := sendTG("/upgrade confirm bot v1.1.9")
	if !strings.Contains(staleReply, "远端最新版本已变更为 v1.2.0") {
		t.Fatalf("expected stale tag rejection, got:\n%s", staleReply)
	}

	// 3. Toggle auto-upgrade on and verify persistence in SQLite settings.
	autoReply, autoBtns := sendTG("/upgrade auto on")
	if !strings.Contains(autoReply, "已开启每日自动升级") || !hasButtonCommand(autoBtns, "/upgrade auto off") {
		t.Fatalf("unexpected /upgrade auto on reply: %s", autoReply)
	}
	if !svc.autoUpgradeEnabled(context.Background()) {
		t.Fatal("expected autoUpgradeEnabled to be true")
	}

	// 4. Execute combined upgrade via version-locked confirmation button.
	beforeMsgs := tgOut.count()
	runReply, _ := sendTG("/upgrade confirm all v1.2.0")
	if !strings.Contains(runReply, "0.118.0") || !strings.Contains(runReply, "0.120.0") || !strings.Contains(runReply, "v1.2.0") {
		t.Fatalf("unexpected /upgrade confirm all reply:\n%s", runReply)
	}
	if restartedExe != binPath {
		t.Fatalf("expected RequestRestart(%q), got %q", binPath, restartedExe)
	}
	if msgCountAtRestart <= beforeMsgs {
		t.Fatalf("expected reply message to be sent BEFORE RequestRestart fired (before=%d atRestart=%d)", beforeMsgs, msgCountAtRestart)
	}
	gotBinary, _ := os.ReadFile(binPath)
	if string(gotBinary) != string(payload) {
		t.Fatalf("binary was not replaced: %q", string(gotBinary))
	}
	backups, _ := os.ReadDir(filepath.Join(dataDir, "backups"))
	if len(backups) == 0 {
		t.Fatal("expected SQLite backup in BackupDir before upgrade")
	}

	// 5. While upgrading=true, incoming messages receive maintenance notice.
	maintReply, _ := sendTG("还在吗")
	if !strings.Contains(maintReply, "升级与重启") {
		t.Fatalf("expected maintenance message while upgrading, got: %s", maintReply)
	}

	// 6. Simulate newly started v1.2.0 process delivering the pending upgrade receipt.
	restartedOut := &buttonCaptureOut{}
	svcNew := &Service{
		Store:   st,
		Engine:  eng,
		Senders: map[string]platform.Replier{platform.Telegram: restartedOut},
		Version: "v1.2.0",
	}
	svcNew.SendPendingUpgradeNotice(context.Background())
	noticeText, _ := restartedOut.last()
	if !strings.Contains(noticeText, "✅ 升级已完成") || !strings.Contains(noticeText, "v1.2.0") || !strings.Contains(noticeText, "0.120.0") {
		t.Fatalf("unexpected post-restart notice:\n%s", noticeText)
	}
	if restartedOut.count() != 1 {
		t.Fatalf("expected 1 receipt, got %d", restartedOut.count())
	}
	svcNew.SendPendingUpgradeNotice(context.Background())
	if restartedOut.count() != 1 {
		t.Fatal("duplicate post-restart receipt sent")
	}
}

func TestPostRestartRollbackWarningNotice(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data"), "gpt-5.4")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	tgOut := &buttonCaptureOut{}
	svc := &Service{
		Store:   st,
		Version: "v1.1.0",
		Senders: map[string]platform.Replier{platform.Telegram: tgOut},
	}
	svc.savePendingNotice(context.Background(), pendingUpgradeNotice{
		Platform:      platform.Telegram,
		ChatID:        "tg-1",
		OldBotVersion: "v1.1.0",
		TargetBotVer:  "v1.2.0",
	})

	svc.SendPendingUpgradeNotice(context.Background())
	warnText, _ := tgOut.last()
	if !strings.Contains(warnText, "⚠️") || !strings.Contains(warnText, "未能切换至目标版本 `v1.2.0`") {
		t.Fatalf("expected rollback warning, got:\n%s", warnText)
	}
}

type blockingCLIUpgraderEngine struct {
	blockingEngine
	updateCalls int
}

func (b *blockingCLIUpgraderEngine) CheckCLIUpgrade(context.Context) engine.CLIStatus {
	return engine.CLIStatus{Current: "0.4.1", DirectUpdateReady: true, Writable: true}
}

func (b *blockingCLIUpgraderEngine) UpgradeCLI(context.Context) (engine.CLIUpgradeResult, error) {
	b.updateCalls++
	return engine.CLIUpgradeResult{Changed: true, OldVersion: "0.4.1", NewVersion: "0.4.2"}, nil
}

func TestUpgradeDefersWhenAnotherUserTurnActive(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "data"), "grok-4.7")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	blockEng := &blockingCLIUpgraderEngine{
		blockingEngine: blockingEngine{started: make(chan struct{}, 1)},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	svc := &Service{
		Store:        st,
		Engine:       blockEng,
		AllowedUsers: map[string]map[string]struct{}{platform.Telegram: {"admin": {}, "member": {}}},
		AdminUsers:   map[string]map[string]struct{}{platform.Telegram: {"admin": {}}},
		DrainTimeout: 120 * time.Millisecond,
	}
	memberOut := &syncOut{}
	svc.Handle(ctx, platform.Inbound{
		Platform: platform.Telegram, ChatID: "chat-member", UserID: "member", Text: "正在长对话", TextMessage: true,
	}, memberOut)
	<-blockEng.started

	var adminOut memCapture
	reply, _, _, _, err := svc.answerWithButtons(context.Background(), platform.Inbound{
		Platform: platform.Telegram, ChatID: "chat-admin", UserID: "admin", Text: "/upgrade confirm cli", TextMessage: true,
	}, &adminOut, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "暂缓升级") || blockEng.updateCalls != 0 {
		t.Fatalf("expected safe deferral while member chat is active, got reply=%q calls=%d", reply, blockEng.updateCalls)
	}
	if svc.upgrading.Load() {
		t.Fatal("upgrading flag remained stuck after deferral")
	}
}
