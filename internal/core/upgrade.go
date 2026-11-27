package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rshun/agent-bot/internal/engine"
	"github.com/rshun/agent-bot/internal/platform"
	"github.com/rshun/agent-bot/internal/store"
	"github.com/rshun/agent-bot/internal/upgrade"
)

const (
	settingAutoUpgrade          = "auto_upgrade"
	settingPendingUpgradeNotice = "pending_upgrade_notice"
	defaultAutoUpgradeClock     = "04:30"
)

const upgradeUsage = `用法：
/upgrade 或 /upgrade check
    检查 agent-bot 与底层 CLI 版本状态及目录权限（附带版本锁确认按钮）
/upgrade bot / /upgrade cli
    单独检查 agent-bot 自身或底层 CLI 引擎
/upgrade run [all|bot|cli]
    直接执行一键升级（自动在线备份数据库、常量时间校验 SHA-256 并在失败时自动回滚）
/upgrade auto [on|off]
    查看、开启或关闭每日定时自动升级`

type pendingUpgradeNotice struct {
	Platform      string `json:"platform,omitempty"`
	ChatID        string `json:"chat_id,omitempty"`
	OldBotVersion string `json:"old_bot_version"`
	TargetBotVer  string `json:"target_bot_version"`
	CLIOldVersion string `json:"cli_old_version,omitempty"`
	CLINewVersion string `json:"cli_new_version,omitempty"`
	Auto          bool   `json:"auto,omitempty"`
	CreatedAt     string `json:"created_at,omitempty"`
}

func (s *Service) checkUpgradeAdmin(in platform.Inbound) (bool, string) {
	hasExplicitAdmins := false
	for _, admins := range s.AdminUsers {
		if len(admins) > 0 {
			hasExplicitAdmins = true
			break
		}
	}
	if hasExplicitAdmins {
		if _, ok := s.AdminUsers[in.Platform][in.UserID]; ok {
			return true, ""
		}
		return false, "没有权限：仅管理员可执行版本升级。"
	}
	for _, allowed := range s.AllowedUsers {
		if len(allowed) > 1 {
			return false, "当前配置了多个白名单用户，请先在环境配置文件中设置 ADMIN_USER_IDS（或 DISCORD_ADMIN_USER_IDS）指定管理员后再执行升级。"
		}
	}
	return true, ""
}

func (s *Service) botUpgrader() *upgrade.Upgrader {
	if s.Upgrader != nil {
		return s.Upgrader
	}
	return &upgrade.Upgrader{
		CurrentVersion: s.Version,
		Paths:          upgrade.SnapshotExecutable(),
	}
}

func (s *Service) drainTimeout() time.Duration {
	if s.DrainTimeout > 0 {
		return s.DrainTimeout
	}
	return 60 * time.Second
}

func (s *Service) autoUpgradeEnabled(ctx context.Context) bool {
	if s.Store != nil {
		if val, ok, err := s.Store.Setting(ctx, settingAutoUpgrade); err == nil && ok {
			return val == "true"
		}
	}
	return s.AutoUpgrade
}

func (s *Service) autoUpgradeClock() string {
	t := strings.TrimSpace(s.AutoUpgradeTime)
	if t == "" {
		return defaultAutoUpgradeClock
	}
	return t
}

func (s *Service) upgradeCommand(ctx context.Context, in platform.Inbound, arg string) (string, [][]platform.Button, string, error) {
	if ok, denyMsg := s.checkUpgradeAdmin(in); !ok {
		return denyMsg, nil, "", nil
	}

	fields := strings.Fields(strings.TrimSpace(arg))
	sub := "check"
	if len(fields) > 0 {
		sub = strings.ToLower(fields[0])
	}

	switch sub {
	case "check", "status", "all":
		text, btns, err := s.upgradePanel(ctx, "")
		return text, btns, "", err

	case "bot", "agent-bot", "grok-bot", "codex-bot", "self":
		text, btns, err := s.upgradePanel(ctx, "bot")
		return text, btns, "", err

	case "cli", "grok", "codex", "antigravity", "agy", "engine":
		text, btns, err := s.upgradePanel(ctx, "cli")
		return text, btns, "", err

	case "cancel":
		return "已取消升级操作。", nil, "", nil

	case "auto":
		return s.upgradeAutoCommand(ctx, fields[1:])

	case "confirm":
		if len(fields) < 2 {
			return upgradeUsage, nil, "", nil
		}
		target := strings.ToLower(fields[1])
		if target != "all" && target != "bot" && target != "cli" {
			return upgradeUsage, nil, "", nil
		}
		expectedTag := ""
		if len(fields) >= 3 {
			expectedTag = fields[2]
		}
		return s.executeManualUpgrade(ctx, in, target, expectedTag)

	case "run", "now":
		target := "all"
		if sub == "run" && len(fields) >= 2 {
			target = strings.ToLower(fields[1])
		}
		if target != "all" && target != "bot" && target != "cli" {
			return upgradeUsage, nil, "", nil
		}
		return s.executeManualUpgrade(ctx, in, target, "")

	case "help":
		return upgradeUsage, nil, "", nil

	default:
		return upgradeUsage, nil, "", nil
	}
}

func (s *Service) upgradeAutoCommand(ctx context.Context, args []string) (string, [][]platform.Button, string, error) {
	clock := s.autoUpgradeClock()
	cliName := s.cliDisplayName()
	if len(args) == 0 {
		enabled := s.autoUpgradeEnabled(ctx)
		state := "已关闭"
		if enabled {
			state = "已开启"
		}
		text := fmt.Sprintf("每日自动升级当前状态：%s（检查时间：每天 %s %s）。\n用法：`/upgrade auto on` 开启，`/upgrade auto off` 关闭。",
			state, clock, s.location().String())
		return text, autoUpgradeToggleButtons(enabled), "", nil
	}

	switch strings.ToLower(strings.TrimSpace(args[0])) {
	case "on", "true", "1", "enable":
		if s.Store != nil {
			if err := s.Store.PutSetting(ctx, settingAutoUpgrade, "true"); err != nil {
				return "", nil, "", err
			}
		}
		return fmt.Sprintf("已开启每日自动升级（每天 %s 自动检查并平滑升级 %s 与 agent-bot）。", clock, cliName), autoUpgradeToggleButtons(true), "", nil
	case "off", "false", "0", "disable":
		if s.Store != nil {
			if err := s.Store.PutSetting(ctx, settingAutoUpgrade, "false"); err != nil {
				return "", nil, "", err
			}
		}
		return "已关闭每日自动升级（仍可随时发送 `/upgrade` 手动检查并一键升级）。", autoUpgradeToggleButtons(false), "", nil
	default:
		return "用法：`/upgrade auto on` 开启，或 `/upgrade auto off` 关闭。", nil, "", nil
	}
}

func (s *Service) upgradePanel(ctx context.Context, focus string) (string, [][]platform.Button, error) {
	autoOn := s.autoUpgradeEnabled(ctx)
	cliName := s.cliDisplayName()

	var botSt upgrade.BotStatus
	if focus == "" || focus == "bot" {
		botSt = s.botUpgrader().CheckBot(ctx)
	}

	var cliSt engine.CLIStatus
	hasCLI := false
	if focus == "" || focus == "cli" {
		if cliUpg, ok := s.Engine.(engine.CLIUpgrader); ok {
			hasCLI = true
			cliSt = cliUpg.CheckCLIUpgrade(ctx)
		} else if mcpEng, mcpOK := s.Engine.(MCPEngine); mcpOK {
			ver, _ := mcpEng.Version(ctx)
			cliSt.Current = ver
		}
	}

	var b strings.Builder
	b.WriteString("📦 版本与升级检查\n\n")

	if focus == "" || focus == "bot" {
		b.WriteString("• agent-bot：")
		switch {
		case botSt.Error != "":
			fmt.Fprintf(&b, "当前 `%s`（⚠️ 检查更新失败：%s）\n", botSt.Current, botSt.Error)
		case botSt.IsDev:
			latestNote := ""
			if botSt.Latest != "" {
				latestNote = fmt.Sprintf("，远端最新正式版 `%s`", botSt.Latest)
			}
			fmt.Fprintf(&b, "当前 `%s`（开发构建不自动覆盖%s）\n", botSt.Current, latestNote)
		case botSt.UpdateAvailable:
			if botSt.Writable {
				fmt.Fprintf(&b, "`%s` → 发现新版本 `%s` ✨（支持零特权原地升级）\n", botSt.Current, botSt.Latest)
			} else {
				fmt.Fprintf(&b, "`%s` → 发现新版本 `%s` ⚠️（当前安装目录无写权限）\n", botSt.Current, botSt.Latest)
			}
		default:
			latest := botSt.Latest
			if latest == "" {
				latest = botSt.Current
			}
			fmt.Fprintf(&b, "已是最新版本 (`%s`)\n", latest)
		}
		if botSt.TargetPath != "" && botSt.Writable {
			fmt.Fprintf(&b, "  程序路径：`%s`（SHA-256 校验原子替换就绪）\n", botSt.TargetPath)
		}
	}

	if focus == "" || focus == "cli" {
		fmt.Fprintf(&b, "• %s：", cliName)
		switch {
		case !hasCLI:
			cur := cliSt.Current
			if cur == "" {
				cur = "未知"
			}
			fmt.Fprintf(&b, "当前 `%s`（当前运行引擎未启用在线升级）\n", cur)
		case cliSt.Error != "":
			if cliSt.Current != "" {
				fmt.Fprintf(&b, "当前 `%s`（⚠️ 检查更新失败：%s）\n", cliSt.Current, cliSt.Error)
			} else {
				fmt.Fprintf(&b, "⚠️ %s\n", cliSt.Error)
			}
		case cliSt.UpdateAvailable:
			if cliSt.Writable {
				fmt.Fprintf(&b, "`%s` → 发现新版本 `%s` ✨（支持直接升级）\n", cliSt.Current, cliSt.Latest)
			} else {
				fmt.Fprintf(&b, "`%s` → 发现新版本 `%s` ⚠️（安装目录无写权限）\n", cliSt.Current, cliSt.Latest)
			}
		case cliSt.DirectUpdateReady:
			fmt.Fprintf(&b, "当前 `%s`（内置自更新就绪：`%s`）\n", cliSt.Current, cliSt.Path)
		default:
			if !cliSt.Writable && cliSt.WriteHint != "" {
				fmt.Fprintf(&b, "当前 `%s` ⚠️（安装目录无写权限）\n", cliSt.Current)
			} else {
				fmt.Fprintf(&b, "已是最新版本 (`%s`)\n", cliSt.Current)
			}
		}
	}

	autoState := "已关闭"
	if autoOn {
		autoState = "已开启"
	}
	fmt.Fprintf(&b, "• 自动升级：%s（每天 %s）", autoState, s.autoUpgradeClock())

	var hints []string
	if (focus == "" || focus == "bot") && !botSt.IsDev && !botSt.Writable && botSt.WriteHint != "" {
		hints = append(hints, botSt.WriteHint)
	}
	if (focus == "" || focus == "cli") && hasCLI && cliSt.Error == "" && !cliSt.Writable && cliSt.WriteHint != "" {
		hints = append(hints, cliSt.WriteHint)
	}
	if len(hints) > 0 {
		b.WriteString("\n\n💡 权限提示：\n")
		b.WriteString(strings.Join(hints, "\n\n"))
	}

	btns := upgradeButtons(botSt, cliSt, hasCLI, focus, autoOn)
	return strings.TrimSpace(b.String()), btns, nil
}

func (s *Service) backupStoreBeforeUpgrade(ctx context.Context, versionTag string) error {
	if s.Store == nil {
		return nil
	}
	if dir := strings.TrimSpace(s.BackupDir); dir != "" {
		_, err := s.Store.Backup(ctx, dir, versionTag)
		return err
	}
	_, err := s.Store.BackupBeforeUpgrade(ctx, versionTag)
	return err
}

func (s *Service) executeManualUpgrade(ctx context.Context, in platform.Inbound, target, expectedTag string) (string, [][]platform.Button, string, error) {
	if !s.upgradeMu.TryLock() {
		return "⏳ 当前已有升级任务正在进行中，请稍候再试。", nil, "", nil
	}
	defer s.upgradeMu.Unlock()

	excludeKey := in.Platform + "\x00" + in.ChatID
	if drained, rem := s.waitOtherLanesIdle(ctx, excludeKey, s.drainTimeout()); !drained {
		return fmt.Sprintf("⚠️ 当前仍有 %d 个进行中的对话或定时任务尚未结束，为避免中断他人会话已暂缓升级，请稍后再试。", rem), nil, "", nil
	}

	cliName := s.cliDisplayName()
	wantCLI := target == "all" || target == "cli"
	wantBot := target == "all" || target == "bot"

	var notes []string
	var cliChanged engine.CLIUpgradeResult
	var btns [][]platform.Button

	if wantCLI {
		cliUpg, ok := s.Engine.(engine.CLIUpgrader)
		if !ok {
			if target == "cli" {
				return fmt.Sprintf("当前运行引擎不支持 %s 在线升级。", cliName), nil, "", nil
			}
		} else {
			res, err := cliUpg.UpgradeCLI(ctx)
			if err != nil {
				redacted := engine.RedactSensitive(err.Error())
				if target == "cli" {
					return fmt.Sprintf("❌ %s 升级失败：\n%s", cliName, redacted), nil, "", nil
				}
				notes = append(notes, fmt.Sprintf("⚠️ %s 升级未完成：%s", cliName, redacted))
			} else {
				if mcpEng, ok := s.Engine.(MCPEngine); ok {
					mcpEng.ResetLeader(ctx)
				}
				if res.Changed {
					cliChanged = res
					notes = append(notes, fmt.Sprintf("✅ %s 已升级：`%s` → `%s`", cliName, res.OldVersion, res.NewVersion))
				} else if target == "cli" {
					ver := res.NewVersion
					if ver == "" {
						ver = res.OldVersion
					}
					notes = append(notes, fmt.Sprintf("✅ %s 已是最新版本（`%s`），后台守护进程已刷新。", cliName, ver))
				}
				if target == "cli" && s.Store != nil {
					if chat, chatErr := s.Store.Chat(ctx, in.Platform, in.ChatID); chatErr == nil && chat.Model != "" {
						if list, mErr := s.Engine.Models(ctx); mErr == nil && len(list.Names) > 0 && !containsModel(list, chat.Model) {
							notes = append(notes, fmt.Sprintf("⚠️ 注意：当前会话使用的模型 `%s` 在新版 %s 中已下线或更名，请点选下方可用模型切换（上下文将继续保留）：", chat.Model, cliName))
							btns = modelButtons(chat.Model, list)
						}
					}
				}
			}
		}
	}

	if wantBot {
		upg := s.botUpgrader()
		staged, err := upg.StageBotUpgrade(ctx, expectedTag)
		if err != nil {
			redacted := engine.RedactSensitive(err.Error())
			if target == "bot" && len(notes) == 0 {
				return "❌ agent-bot 升级失败：\n" + redacted, nil, "", nil
			}
			notes = append(notes, "⚠️ agent-bot 升级未完成："+redacted)
		} else if staged.Changed {
			if bakErr := s.backupStoreBeforeUpgrade(ctx, staged.OldVersion); bakErr != nil {
				notes = append(notes, "❌ 升级前备份数据库失败，已安全中止替换二进制："+engine.RedactSensitive(bakErr.Error()))
				return strings.Join(notes, "\n\n"), nil, "", nil
			}
			if _, commitErr := upgrade.CommitBinary(staged.TargetPath, staged.StagedPath); commitErr != nil {
				notes = append(notes, "❌ 替换 agent-bot 二进制失败（原程序未受影响）："+engine.RedactSensitive(commitErr.Error()))
				return strings.Join(notes, "\n\n"), nil, "", nil
			}
			s.savePendingNotice(ctx, pendingUpgradeNotice{
				Platform:      in.Platform,
				ChatID:        in.ChatID,
				OldBotVersion: staged.OldVersion,
				TargetBotVer:  staged.NewVersion,
				CLIOldVersion: cliChanged.OldVersion,
				CLINewVersion: cliChanged.NewVersion,
				Auto:          false,
				CreatedAt:     s.now().UTC().Format(time.RFC3339),
			})
			s.upgrading.Store(true)
			restartExe := staged.LaunchPath
			if restartExe == "" {
				restartExe = staged.TargetPath
			}
			notes = append(notes, fmt.Sprintf("📦 数据库与旧版程序已备份且 SHA-256 校验通过，正在将 agent-bot (`%s` → `%s`) 平滑重启切换，完成后会自动通知你…",
				staged.OldVersion, staged.NewVersion))
			return strings.Join(notes, "\n\n"), nil, restartExe, nil
		} else if target == "bot" {
			notes = append(notes, fmt.Sprintf("✅ agent-bot 已是最新版本（`%s`），无需升级。", staged.NewVersion))
		}
	}

	if len(notes) == 0 {
		botSt := s.botUpgrader().CheckBot(ctx)
		cliVer := ""
		if cliUpg, ok := s.Engine.(engine.CLIUpgrader); ok {
			cliVer = cliUpg.CheckCLIUpgrade(ctx).Current
		}
		if cliVer != "" {
			return fmt.Sprintf("✅ agent-bot (`%s`) 与 %s (`%s`) 均已是最新版本，无需升级。", botSt.Current, cliName, cliVer), nil, "", nil
		}
		return fmt.Sprintf("✅ agent-bot (`%s`) 已是最新版本，无需升级。", botSt.Current), nil, "", nil
	}
	return strings.Join(notes, "\n\n"), btns, "", nil
}

// StartAutoUpgrader runs a background daily timer that checks and upgrades the underlying CLI
// and agent-bot when auto-upgrade is enabled and all chat lanes are idle.
func (s *Service) StartAutoUpgrader(ctx context.Context) {
	_, hasCLIUpg := s.Engine.(engine.CLIUpgrader)
	if s.Upgrader == nil && !hasCLIUpg {
		return
	}
	go func() {
		for {
			delay := nextClockDelay(s.now(), s.autoUpgradeClock(), s.location())
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			if !s.autoUpgradeEnabled(ctx) {
				continue
			}
			if s.runScheduledAutoUpgrade(ctx) {
				return
			}
		}
	}()
}

func (s *Service) runScheduledAutoUpgrade(ctx context.Context) bool {
	if !s.upgradeMu.TryLock() {
		return false
	}
	defer s.upgradeMu.Unlock()

	upg := s.botUpgrader()
	botSt := upg.CheckBot(ctx)
	var cliSt engine.CLIStatus
	cliUpg, hasCLI := s.Engine.(engine.CLIUpgrader)
	if hasCLI {
		cliSt = cliUpg.CheckCLIUpgrade(ctx)
	}

	cliCandidate := hasCLI && cliSt.Writable && (cliSt.UpdateAvailable || cliSt.DirectUpdateReady)
	botCandidate := botSt.UpdateAvailable && botSt.Writable
	if !botCandidate && !cliCandidate {
		slog.Info("auto-upgrade check completed: nothing actionable", "bot", botSt.Current, "cli", cliSt.Current)
		return false
	}

	if drained, _ := s.waitOtherLanesIdle(ctx, "", 10*time.Minute); !drained {
		slog.Warn("auto-upgrade skipped: chat lanes remained busy")
		return false
	}

	cliName := s.cliDisplayName()
	var msgs []string
	var cliRes engine.CLIUpgradeResult

	if cliCandidate {
		res, err := cliUpg.UpgradeCLI(ctx)
		if err != nil {
			msgs = append(msgs, fmt.Sprintf("⚠️ %s 自动升级失败：%s", cliName, engine.RedactSensitive(err.Error())))
		} else if res.Changed {
			cliRes = res
			if mcpEng, ok := s.Engine.(MCPEngine); ok {
				mcpEng.ResetLeader(ctx)
			}
			msgs = append(msgs, fmt.Sprintf("✅ %s：`%s` → `%s`", cliName, res.OldVersion, res.NewVersion))
		}
	}

	if botCandidate {
		staged, err := upg.StageBotUpgrade(ctx, "")
		if err != nil {
			msgs = append(msgs, "⚠️ agent-bot 自动升级失败："+engine.RedactSensitive(err.Error()))
		} else if staged.Changed {
			if bakErr := s.backupStoreBeforeUpgrade(ctx, staged.OldVersion); bakErr != nil {
				msgs = append(msgs, "⚠️ 备份数据库失败，已中止 agent-bot 自动升级："+engine.RedactSensitive(bakErr.Error()))
			} else if _, commitErr := upgrade.CommitBinary(staged.TargetPath, staged.StagedPath); commitErr != nil {
				msgs = append(msgs, "⚠️ 替换 agent-bot 二进制失败："+engine.RedactSensitive(commitErr.Error()))
			} else {
				s.savePendingNotice(ctx, pendingUpgradeNotice{
					OldBotVersion: staged.OldVersion,
					TargetBotVer:  staged.NewVersion,
					CLIOldVersion: cliRes.OldVersion,
					CLINewVersion: cliRes.NewVersion,
					Auto:          true,
					CreatedAt:     s.now().UTC().Format(time.RFC3339),
				})
				s.upgrading.Store(true)
				restartExe := staged.LaunchPath
				if restartExe == "" {
					restartExe = staged.TargetPath
				}
				if s.RequestRestart != nil {
					s.RequestRestart(restartExe)
					return true
				}
			}
		}
	}

	if len(msgs) > 0 {
		s.broadcastUpgradeNotice(ctx, "🔄 自动升级报告\n\n"+strings.Join(msgs, "\n"))
	}
	return false
}

// SendPendingUpgradeNotice checks whether the previous instance recorded a binary upgrade notice,
// clears it immediately, and sends a post-restart confirmation (or rollback warning) to the admin chat.
func (s *Service) SendPendingUpgradeNotice(ctx context.Context) {
	if s.Store == nil {
		return
	}
	raw, ok, err := s.Store.Setting(ctx, settingPendingUpgradeNotice)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	_ = s.Store.DeleteSetting(ctx, settingPendingUpgradeNotice)

	var notice pendingUpgradeNotice
	if err := json.Unmarshal([]byte(raw), &notice); err != nil || notice.TargetBotVer == "" {
		return
	}
	if notice.CreatedAt != "" {
		if ts, tErr := time.Parse(time.RFC3339, notice.CreatedAt); tErr == nil {
			if s.now().Sub(ts) > 30*time.Minute {
				return
			}
		}
	}

	curVer := strings.TrimSpace(s.Version)
	if curVer == "" {
		curVer = "dev"
	}
	matched := strings.EqualFold(curVer, notice.TargetBotVer)
	if !matched {
		if cmp, valid := engine.CompareSemver(curVer, notice.TargetBotVer); valid && cmp == 0 {
			matched = true
		}
	}

	var b strings.Builder
	if matched {
		if notice.Auto {
			b.WriteString("✅ 自动升级已完成\n\n")
		} else {
			b.WriteString("✅ 升级已完成，所有会话与定时任务已恢复正常\n\n")
		}
		fmt.Fprintf(&b, "• agent-bot：`%s` → `%s`\n", notice.OldBotVersion, curVer)
		if notice.CLINewVersion != "" {
			fmt.Fprintf(&b, "• %s：`%s` → `%s`\n", s.cliDisplayName(), notice.CLIOldVersion, notice.CLINewVersion)
		}
	} else {
		fmt.Fprintf(&b, "⚠️ agent-bot 升级重启后未能切换至目标版本 `%s`（当前运行版本：`%s`），请发送 `/status` 或检查服务日志。", notice.TargetBotVer, curVer)
	}
	msg := strings.TrimSpace(b.String())

	if !notice.Auto && notice.Platform != "" && notice.ChatID != "" {
		if sender := s.Senders[notice.Platform]; sender != nil {
			s.send(ctx, sender, notice.ChatID, msg)
			return
		}
	}
	s.broadcastUpgradeNotice(ctx, msg)
}

// DeliverPendingUpgradeNotice is an alias for SendPendingUpgradeNotice kept for caller symmetry.
func (s *Service) DeliverPendingUpgradeNotice(ctx context.Context) {
	s.SendPendingUpgradeNotice(ctx)
}

func (s *Service) savePendingNotice(ctx context.Context, notice pendingUpgradeNotice) {
	if s.Store == nil {
		return
	}
	if data, err := json.Marshal(notice); err == nil {
		if putErr := s.Store.PutSetting(ctx, settingPendingUpgradeNotice, string(data)); putErr != nil {
			slog.Warn("save pending upgrade notice failed", "err", putErr)
		}
	}
}

func (s *Service) broadcastUpgradeNotice(ctx context.Context, text string) {
	for _, target := range s.taskTargets(ctx, store.Task{}) {
		s.send(ctx, target.Sender, target.ChatID, text)
	}
}

func (s *Service) waitOtherLanesIdle(ctx context.Context, excludeKey string, maxWait time.Duration) (bool, int) {
	countBusy := func() int {
		s.mu.Lock()
		lanes := make(map[string]*lane, len(s.lanes))
		for k, v := range s.lanes {
			lanes[k] = v
		}
		s.mu.Unlock()

		busy := 0
		for key, ln := range lanes {
			ln.mu.Lock()
			d := ln.depth
			ln.mu.Unlock()
			if key == excludeKey {
				if d > 1 {
					busy++
				}
				continue
			}
			if d > 0 {
				busy++
			}
		}
		return busy
	}

	if rem := countBusy(); rem == 0 {
		return true, 0
	}
	deadline := time.Now().Add(maxWait)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, countBusy()
		case <-ticker.C:
			rem := countBusy()
			if rem == 0 {
				return true, 0
			}
			if time.Now().After(deadline) {
				return false, rem
			}
		}
	}
}

func nextClockDelay(now time.Time, hhmm string, loc *time.Location) time.Duration {
	if loc == nil {
		loc = time.Local
	}
	hour, minute := 4, 30
	if parts := strings.Split(strings.TrimSpace(hhmm), ":"); len(parts) == 2 {
		if h, err1 := strconv.Atoi(parts[0]); err1 == nil && h >= 0 && h <= 23 {
			if m, err2 := strconv.Atoi(parts[1]); err2 == nil && m >= 0 && m <= 59 {
				hour, minute = h, m
			}
		}
	}
	localNow := now.In(loc)
	target := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), hour, minute, 0, 0, loc)
	if !target.After(localNow) {
		target = target.AddDate(0, 0, 1)
	}
	d := target.Sub(localNow)
	if d < time.Second {
		return time.Second
	}
	return d
}

func containsModel(list engine.ModelList, target string) bool {
	for _, m := range list.Names {
		if m == target {
			return true
		}
	}
	return false
}

// parseNaturalUpgradeIntent detects short, standalone user messages asking to check or upgrade
// agent-bot or the underlying CLI, returning the panel focus ("", "bot", or "cli") without auto-executing.
func parseNaturalUpgradeIntent(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	if strings.ContainsAny(trimmed, "\n\r`") || utf8.RuneCountInString(trimmed) > 28 {
		return "", false
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return "", false
	}
	for _, skip := range []string{
		"怎么", "如何", "为什么", "是什么", "帮我写", "写一个", "写个", "脚本", "代码",
		"how to", "why ", "write a", "script",
		"ubuntu", "debian", "centos", "linux", "windows", "macos", "python", "pip",
		"node", "npm", "docker", "apt", "brew", "内核", "系统包",
	} {
		if strings.Contains(lower, skip) {
			return "", false
		}
	}

	norm := strings.Trim(lower, " \t。，！？.,!?~～")
	normCompact := strings.NewReplacer(" ", "", "\t", "").Replace(norm)

	for _, cliPhrase := range []string{
		"升级cli", "更新cli", "升级grokcli", "更新grokcli", "grokcli升级", "grokcli更新",
		"升级codexcli", "更新codexcli", "codexcli升级", "codexcli更新",
		"升级antigravitycli", "更新antigravitycli", "antigravitycli升级", "antigravitycli更新",
		"升级agy", "更新agy", "agy升级", "agy更新", "升级agycli", "更新agycli",
		"升级底层引擎", "更新底层引擎", "检查cli更新",
		"upgradecli", "updatecli", "upgradegrokcli", "updategrokcli", "upgradecodexcli", "updatecodexcli",
		"upgradeantigravitycli", "updateantigravitycli", "upgradeagycli", "updateagycli", "upgradeagy", "updateagy",
		"checkcliupdate",
	} {
		if normCompact == cliPhrase {
			return "cli", true
		}
	}
	for _, botPhrase := range []string{
		"升级机器人", "更新机器人", "机器人升级", "机器人更新",
		"升级agent-bot", "更新agent-bot", "agent-bot升级", "agent-bot更新",
		"升级agentbot", "更新agentbot",
		"升级grok-bot", "更新grok-bot", "grok-bot升级", "grok-bot更新",
		"升级grokbot", "更新grokbot",
		"升级codex-bot", "更新codex-bot", "codex-bot升级", "codex-bot更新",
		"升级codexbot", "更新codexbot",
		"升级bot", "更新bot", "检查机器人更新",
		"upgradebot", "updatebot", "upgradeagent-bot", "updateagent-bot", "upgradeagentbot", "updateagentbot",
	} {
		if normCompact == botPhrase {
			return "bot", true
		}
	}
	for _, allPhrase := range []string{
		"检查更新", "检查升级", "查看更新", "有没有新版本", "有新版本吗",
		"升级一下", "更新一下", "自动升级", "自我升级", "升级版本", "更新版本", "升级程序", "更新程序",
		"checkforupdates", "checkforupdate", "checkupdates", "checkupdate",
	} {
		if normCompact == allPhrase {
			return "", true
		}
	}
	return "", false
}
