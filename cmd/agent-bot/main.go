package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/rshun/agent-bot/internal/actions"
	"github.com/rshun/agent-bot/internal/config"
	"github.com/rshun/agent-bot/internal/core"
	"github.com/rshun/agent-bot/internal/engine"
	"github.com/rshun/agent-bot/internal/engine/codex"
	"github.com/rshun/agent-bot/internal/engine/grok"
	"github.com/rshun/agent-bot/internal/media"
	"github.com/rshun/agent-bot/internal/platform"
	"github.com/rshun/agent-bot/internal/platform/discord"
	"github.com/rshun/agent-bot/internal/platform/telegram"
	"github.com/rshun/agent-bot/internal/sandbox"
	"github.com/rshun/agent-bot/internal/schedule"
	"github.com/rshun/agent-bot/internal/store"
	"github.com/rshun/agent-bot/internal/upgrade"
)

var version = "dev"

func main() {
	initialEnv := os.Environ()
	exePaths := upgrade.SnapshotExecutable()

	inv, err := parseCLI(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error()+"\n\n"+cliUsage)
		os.Exit(2)
	}
	switch inv.Mode {
	case modeVersion:
		fmt.Printf("agent-bot %s\n", version)
		return
	case modeHelp:
		fmt.Println(cliUsage)
		return
	case modePush:
		if isPushHelp(inv.PushArgs) {
			fmt.Println(pushUsage)
			return
		}
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	var taskLogFile *os.File
	if inv.Mode == modeRunTask && inv.DataDir != "" {
		taskLogFile = attachTaskSchedulerLog(inv.DataDir)
		if taskLogFile != nil {
			defer taskLogFile.Close()
		}
	}
	if inv.EnvFile != "" {
		if resolved := config.LoadEnvFile(inv.EnvFile); resolved != "" {
			_ = os.Setenv("BOT_ENV_FILE", resolved)
		}
	}
	if inv.DataDir != "" {
		_ = os.Setenv("DATA_DIR", inv.DataDir)
	}

	exeDirs := executableDirs()
	var loadedEnv string
	if os.Getenv("TELEGRAM_BOT_TOKEN") == "" && os.Getenv("DISCORD_BOT_TOKEN") == "" {
		loadedEnv = config.LoadEnvFile(envCandidatePaths(os.Getenv("BOT_ENV_FILE"), exeDirs)...)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		config.LoadUserProfile(home)
	}
	cfg, err := config.Load()
	if err != nil {
		slog.Error("config", "err", err)
		os.Exit(1)
	}
	cfg.Version = version
	if inv.DataDir != "" {
		cfg.DataDir = inv.DataDir
	} else if os.Getenv("DATA_DIR") == "" {
		cfg.DataDir = resolveDataDir(cfg.DataDir, loadedEnv, exeDirs)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		slog.Error("home dir", "err", err)
		os.Exit(1)
	}
	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		slog.Error("data dir", "err", err)
		os.Exit(1)
	}
	if inv.Mode == modeRunTask && taskLogFile == nil {
		taskLogFile = attachTaskSchedulerLog(dataDir)
		if taskLogFile != nil {
			defer taskLogFile.Close()
		}
	}
	activeEnvFile := resolveActiveEnvFile(loadedEnv, inv.EnvFile, cfg.EnvFile, exeDirs)
	if activeEnvFile != "" {
		cfg.EnvFile = activeEnvFile
	}
	engineHome := cfg.EngineHome
	if engineHome == "" {
		engineHome = filepath.Join(home, "."+cfg.Engine)
	}
	mediaDir := filepath.Join(dataDir, "media")
	policy := sandbox.Policy{
		Home:       home,
		EngineHome: engineHome,
		DataDir:    dataDir,
		MediaDir:   mediaDir,
		EnvFile:    cfg.EnvFile,
		AuthPath:   cfg.AuthPath,
		BotBin:     exePaths.TargetPath,
	}
	if inv.Mode == modePush {
		var db *store.Store
		if fileExists(filepath.Join(cfg.DataDir, "bot.db")) {
			if opened, openErr := store.Open(cfg.DataDir, cfg.DefaultModel); openErr == nil {
				db = opened
				defer db.Close()
			}
		}
		ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
		defer stop()
		if err := runPushCommand(ctx, cfg, db, policy, inv.PushArgs); err != nil {
			os.Exit(1)
		}
		return
	}

	isTaskRun := inv.Mode == modeRunTask
	workspaceName := "workspace"
	if isTaskRun {
		// Isolated workspace so one-shot cron runs do not overwrite the daemon's MCP port config.
		workspaceName = "task-workspace"
	}
	workspace := filepath.Join(cfg.DataDir, workspaceName)
	if err := os.MkdirAll(workspace, 0o750); err != nil {
		slog.Error("data dir", "err", err)
		os.Exit(1)
	}
	// Prompt files of turns with images live here only while the CLI runs; only the bot may read them.
	inbox := filepath.Join(cfg.DataDir, "inbox")
	if err := os.MkdirAll(inbox, 0o700); err != nil {
		slog.Error("inbox dir", "err", err)
		os.Exit(1)
	}
	if err := os.Chmod(inbox, 0o700); err != nil {
		slog.Warn("inbox dir mode", "err", err)
	}
	if err := os.MkdirAll(mediaDir, 0o750); err != nil {
		slog.Error("media dir", "err", err)
		os.Exit(1)
	}
	db, err := store.Open(cfg.DataDir, cfg.DefaultModel)
	if err != nil {
		slog.Error("store", "err", err)
		os.Exit(1)
	}
	defer db.Close()

	sandboxManager := &sandbox.Manager{
		Path:          filepath.Join(engineHome, "sandbox.toml"),
		Enabled:       sandbox.Supported(),
		DenyAvailable: sandbox.BwrapAvailable,
		Policy:        policy,
	}

	var eng engine.Engine
	if cfg.Engine == "codex" {
		eng = &codex.Client{
			Bin:          cfg.EngineBin,
			NPMBin:       cfg.NPMBin,
			Workspace:    workspace,
			AuthPath:     cfg.AuthPath,
			DefaultModel: cfg.DefaultModel,
			ModelsList:   cfg.CandidateModels,
			Timeout:      cfg.EngineTimeout,
			MaxTurns:     cfg.EngineMaxTurns,
			InboxDir:     inbox,
		}
	} else {
		eng = &grok.Client{
			Bin:          cfg.EngineBin,
			Workspace:    workspace,
			AuthPath:     cfg.AuthPath,
			DefaultModel: cfg.DefaultModel,
			ModelsList:   cfg.CandidateModels,
			BillingURL:   cfg.BillingURL,
			UserURL:      cfg.UserURL,
			Timeout:      cfg.EngineTimeout,
			MaxTurns:     cfg.EngineMaxTurns,
			InboxDir:     inbox,
		}
	}
	if !isTaskRun {
		if mcpEng, ok := eng.(engine.MCPEngine); ok {
			mcpEng.ResetLeader(context.Background())
		}
	}

	// The local MCP server lets the CLI act on the chat during a turn. Chat keeps working without it.
	var actionsForSvc *actions.Server
	if actionsSrv, err := actions.Listen("127.0.0.1:0"); err != nil {
		slog.Error("actions listen; chat actions are off", "err", err)
	} else {
		cfgPath := filepath.Join(workspace, "."+cfg.Engine, "config.toml")
		switch err := actions.WriteConfigForEngine(cfgPath, actionsSrv.Port(), cfg.Engine); {
		case err == nil:
			actionsForSvc = actionsSrv
		case errors.Is(err, actions.ErrUnmanaged):
			slog.Warn("workspace engine config.toml was not written by the bot; chat actions are off", "path", cfgPath)
		default:
			slog.Error("write engine config; chat actions are off", "err", err)
		}
		if actionsForSvc == nil {
			_ = actionsSrv.Close()
		}
	}
	if actionsForSvc != nil {
		defer actionsForSvc.Close()
	}

	schedBackend, schedInitErr := buildSystemScheduler(home, dataDir, activeEnvFile)
	if schedInitErr != nil && !isTaskRun {
		slog.Warn("os scheduler unavailable", "err", schedInitErr)
	}
	upgrader := &upgrade.Upgrader{
		CurrentVersion: cfg.Version,
		Paths:          exePaths,
		Repo:           cfg.UpgradeRepo,
	}
	svc := &core.Service{
		Actions:             actionsForSvc,
		Store:               db,
		Engine:              eng,
		AllowedUsers:        map[string]map[string]struct{}{},
		AdminUsers:          map[string]map[string]struct{}{},
		QueueLimit:          cfg.QueueLimit,
		Version:             cfg.Version,
		WebSearch:           cfg.WebSearch,
		DisableLinkPrefetch: !cfg.LinkPrefetch,
		AllowShell:          cfg.AllowShell,
		ShowProgress:        cfg.ShowProgress,
		UploadDir:           filepath.Join(dataDir, "uploads"),
		MediaDir:            mediaDir,
		ImageAuditor:        media.NoopImageAuditor{},
		Sandbox:             sandboxManager,
		Senders:             map[string]platform.Replier{},
		Location:            cfg.Location,
		TaskLimit:           cfg.TaskLimit,
		TaskMinInterval:     cfg.TaskMinInterval,
		ScriptDirs:          cfg.ScriptDirs,
		ScriptTimeout:       cfg.ScriptTimeout,
		ScriptLogDir:        filepath.Join(dataDir, "task-logs"),
		ScriptEnv:           scriptTaskEnv(dataDir, activeEnvFile),
		Scheduler:           schedBackend,
		SchedulerInitErr:    schedInitErr,
		BackupDir:           filepath.Join(dataDir, "backups"),
		Upgrader:            upgrader,
		AutoUpgrade:         cfg.AutoUpgrade,
		AutoUpgradeTime:     cfg.AutoUpgradeTime,
	}

	type runner struct {
		name string
		run  func(context.Context) error
	}
	var runners []runner
	logArgs := []any{"version", version, "engine", cfg.Engine}
	if schedBackend != nil {
		logArgs = append(logArgs, "scheduler", schedBackend.Name())
	}
	if cfg.TelegramToken != "" {
		bot, err := telegram.New(cfg.TelegramToken)
		if err != nil {
			slog.Error("telegram", "err", err)
			os.Exit(1)
		}
		svc.AllowedUsers[platform.Telegram] = cfg.TelegramUsers
		if len(cfg.TelegramAdmins) > 0 {
			svc.AdminUsers[platform.Telegram] = cfg.TelegramAdmins
		}
		svc.Senders[platform.Telegram] = bot
		logArgs = append(logArgs, "telegram", bot.Username())
		runners = append(runners, runner{name: "telegram", run: func(ctx context.Context) error {
			return bot.Run(ctx, svc.Handle)
		}})
	}
	if cfg.DiscordToken != "" {
		bot, err := discord.New(cfg.DiscordToken)
		if err != nil {
			slog.Error("discord", "err", err)
			os.Exit(1)
		}
		svc.AllowedUsers[platform.Discord] = cfg.DiscordUsers
		if len(cfg.DiscordAdmins) > 0 {
			svc.AdminUsers[platform.Discord] = cfg.DiscordAdmins
		}
		svc.Senders[platform.Discord] = bot
		logArgs = append(logArgs, "discord", bot.Username())
		runners = append(runners, runner{name: "discord", run: func(ctx context.Context) error {
			return bot.Run(ctx, svc.Handle)
		}})
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	var (
		restartOnce   sync.Once
		restartTarget string
	)
	svc.RequestRestart = func(exePath string) {
		restartOnce.Do(func() {
			restartTarget = exePath
			slog.Info("restarting into upgraded binary", "exe", exePath)
			stop()
		})
	}

	if actionsForSvc != nil {
		go func() {
			if err := actionsForSvc.Serve(ctx); err != nil {
				slog.Error("actions server", "err", err)
			}
		}()
	}
	if isTaskRun {
		if err := svc.RunTaskByID(ctx, inv.TaskID); err != nil {
			slog.Error("run-task", "id", inv.TaskID, "err", err)
			os.Exit(1)
		}
		return
	}

	svc.InitScheduler(ctx)
	svc.StartAutoUpgrader(ctx)
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(1500 * time.Millisecond):
			svc.SendPendingUpgradeNotice(ctx)
		}
	}()

	slog.Info("listening", logArgs...)
	errCh := make(chan error, len(runners))
	for _, item := range runners {
		go func(item runner) {
			err := item.run(ctx)
			if err != nil && ctx.Err() == nil {
				slog.Error(item.name, "err", err)
				stop()
				errCh <- err
				return
			}
			errCh <- nil
		}(item)
	}
	failed := false
	for range runners {
		if err := <-errCh; err != nil {
			failed = true
		}
	}

	if restartTarget != "" {
		if actionsForSvc != nil {
			_ = actionsForSvc.Close()
		}
		_ = db.Close()
		if taskLogFile != nil {
			_ = taskLogFile.Close()
		}
		if mcpEng, ok := eng.(engine.MCPEngine); ok {
			mcpEng.ResetLeader(context.Background())
		}
		if err := upgrade.RestartProcess(restartTarget, os.Args, initialEnv); err != nil {
			slog.Error("exec upgraded binary failed; rolling back and exiting for service manager", "err", err)
			rbTarget := exePaths.TargetPath
			if rbTarget == "" {
				rbTarget = restartTarget
			}
			if rbErr := upgrade.RollbackBinary(rbTarget); rbErr != nil {
				slog.Error("rollback binary failed", "err", rbErr)
			}
			os.Exit(upgrade.ExitCodeUpgrade)
		}
		return
	}
	if failed {
		os.Exit(1)
	}
}

func executableDirs() []string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return nil
	}
	dirs := []string{filepath.Dir(exe)}
	if real, err := filepath.EvalSymlinks(exe); err == nil && real != "" {
		realDir := filepath.Dir(real)
		if realDir != dirs[0] {
			dirs = append(dirs, realDir)
		}
	}
	return dirs
}

func envCandidatePaths(botEnvFile string, exeDirs []string) []string {
	paths := []string{botEnvFile}
	for _, hint := range []string{os.Getenv("BOT_ENGINE"), os.Getenv("USER"), os.Getenv("LOGNAME")} {
		name := strings.ToLower(strings.TrimSpace(hint))
		if name == "grok" || name == "codex" {
			paths = append(paths, "/etc/agent-bot/"+name+".env")
		}
	}
	paths = append(paths,
		"/etc/agent-bot.env",
		"/etc/grok-tg-bot.env",
		"/etc/codex-bot.env",
		".env",
	)
	for _, dir := range exeDirs {
		if dir != "" {
			paths = append(paths, filepath.Join(dir, ".env"))
		}
	}
	return paths
}

func resolveDataDir(defaultDir, loadedEnv string, exeDirs []string) string {
	if fileExists(filepath.Join(defaultDir, "bot.db")) {
		return defaultDir
	}
	if loadedEnv != "" {
		cand := filepath.Join(filepath.Dir(loadedEnv), "data")
		if fileExists(filepath.Join(cand, "bot.db")) {
			return cand
		}
	}
	for _, dir := range exeDirs {
		cand := filepath.Join(dir, "data")
		if fileExists(filepath.Join(cand, "bot.db")) {
			return cand
		}
	}
	return defaultDir
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func resolveActiveEnvFile(loadedEnv, flagEnv, cfgEnv string, exeDirs []string) string {
	candidates := []string{flagEnv, loadedEnv, os.Getenv("BOT_ENV_FILE"), cfgEnv}
	candidates = append(candidates, envCandidatePaths("", exeDirs)...)
	for _, p := range candidates {
		if p != "" && fileExists(p) {
			if abs, err := filepath.Abs(p); err == nil {
				return abs
			}
			return p
		}
	}
	return ""
}

// scriptTaskEnv tells script tasks where this agent-bot binary and its config live, so a script
// can push results with "$AGENT_BOT_BIN" send (or legacy "$GROK_BOT_BIN" / "$CODEX_BOT_BIN")
// without any token of its own (paths only, no secrets).
func scriptTaskEnv(dataDir, envFile string) []string {
	var env []string
	if exe, err := os.Executable(); err == nil && exe != "" {
		if real, err := filepath.EvalSymlinks(exe); err == nil && real != "" {
			exe = real
		}
		env = append(env, "AGENT_BOT_BIN="+exe, "CODEX_BOT_BIN="+exe, "GROK_BOT_BIN="+exe)
	}
	if envFile != "" {
		env = append(env, "BOT_ENV_FILE="+envFile)
	}
	if dataDir != "" {
		env = append(env, "DATA_DIR="+dataDir)
	}
	return env
}

func buildSystemScheduler(home, dataDir, envFile string) (schedule.Backend, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil && real != "" {
		exe = real
	}
	wd, _ := os.Getwd()
	return schedule.NewSystemScheduler(schedule.SystemConfig{
		BinPath: exe,
		WorkDir: wd,
		DataDir: dataDir,
		EnvFile: envFile,
		HomeDir: home,
	})
}

func attachTaskSchedulerLog(dataDir string) *os.File {
	if dataDir == "" {
		return nil
	}
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil
	}
	logPath := filepath.Join(dataDir, "task-scheduler.log")
	if info, err := os.Stat(logPath); err == nil && info.Size() >= 2<<20 {
		_ = os.Rename(logPath, logPath+".1")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil
	}
	if stderrInfo, sErr := os.Stderr.Stat(); sErr == nil {
		if logInfo, lErr := f.Stat(); lErr == nil && os.SameFile(stderrInfo, logInfo) {
			_ = f.Close()
			return nil
		}
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, f), nil)))
	return f
}
