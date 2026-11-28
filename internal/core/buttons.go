package core

import (
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rshun/agent-bot/internal/engine"
	"github.com/rshun/agent-bot/internal/platform"
	"github.com/rshun/agent-bot/internal/store"
	"github.com/rshun/agent-bot/internal/upgrade"
)

func shortPlatformName(plat string) string {
	switch plat {
	case platform.Telegram:
		return "TG"
	case platform.Discord:
		return "Discord"
	default:
		return plat
	}
}

func fullPlatformName(plat string) string {
	switch plat {
	case platform.Telegram:
		return "Telegram"
	case platform.Discord:
		return "Discord"
	default:
		return plat
	}
}

func shortButtonTitle(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	r := []rune(s)
	return string(r[:maxRunes]) + "…"
}

func statusButtons() [][]platform.Button {
	return [][]platform.Button{
		{
			{Label: "📊 剩余额度", Command: "/usage", Style: platform.ButtonSecondary},
			{Label: "💬 历史会话", Command: "/sessions", Style: platform.ButtonSecondary},
			{Label: "⏰ 定时任务", Command: "/task list", Style: platform.ButtonSecondary},
		},
		{
			{Label: "🧠 长期记忆", Command: "/memory", Style: platform.ButtonSecondary},
			{Label: "🔌 MCP 诊断", Command: "/mcp doctor", Style: platform.ButtonSecondary},
			{Label: "🤖 切换模型", Command: "/model", Style: platform.ButtonPrimary},
		},
		{
			{Label: "🔄 检查更新", Command: "/upgrade", Style: platform.ButtonSecondary},
		},
	}
}

const (
	modelButtonsPerRow = 2
	modelMaxSinglePage = 10 // <= 10 models fit in 5 rows x 2 buttons with no pagination bar
	modelPageSize      = 8  // > 10 models use 4 rows x 2 buttons + 1 bottom nav row (5 rows max for Discord)
)

func modelPageBounds(total, page int) (start, end, curPage, totalPages int) {
	if total <= 0 {
		return 0, 0, 1, 1
	}
	if total <= modelMaxSinglePage {
		return 0, total, 1, 1
	}
	totalPages = (total + modelPageSize - 1) / modelPageSize
	if page < 1 {
		page = 1
	}
	if page > totalPages {
		page = totalPages
	}
	start = (page - 1) * modelPageSize
	end = start + modelPageSize
	if end > total {
		end = total
	}
	return start, end, page, totalPages
}

func modelButtons(current string, list engine.ModelList) [][]platform.Button {
	return modelButtonsPage(current, list, 1)
}

func modelButtonsPage(current string, list engine.ModelList, page int) [][]platform.Button {
	if len(list.Names) == 0 {
		return nil
	}
	eligible := make([]string, 0, len(list.Names))
	for _, name := range list.Names {
		if len("/model "+name) <= 64 {
			eligible = append(eligible, name)
		}
	}
	if len(eligible) == 0 {
		return nil
	}
	start, end, curPage, totalPages := modelPageBounds(len(eligible), page)
	var rows [][]platform.Button
	var curRow []platform.Button
	for _, name := range eligible[start:end] {
		btn := platform.Button{
			Label:   name,
			Command: "/model " + name,
			Style:   platform.ButtonSecondary,
		}
		if name == current {
			btn.Label = "✓ " + name
			btn.Style = platform.ButtonSuccess
		}
		curRow = append(curRow, btn)
		if len(curRow) == modelButtonsPerRow {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 {
		rows = append(rows, curRow)
	}
	if totalPages > 1 {
		var navRow []platform.Button
		if curPage > 1 {
			navRow = append(navRow, platform.Button{
				Label:   fmt.Sprintf("⬅️ 上一页 (%d/%d)", curPage-1, totalPages),
				Command: fmt.Sprintf("/model page %d", curPage-1),
				Style:   platform.ButtonPrimary,
			})
		}
		if curPage < totalPages {
			navRow = append(navRow, platform.Button{
				Label:   fmt.Sprintf("下一页 (%d/%d) ➡️", curPage+1, totalPages),
				Command: fmt.Sprintf("/model page %d", curPage+1),
				Style:   platform.ButtonPrimary,
			})
		}
		if len(navRow) > 0 {
			rows = append(rows, navRow)
		}
	}
	return rows
}

func sessionsButtons(currentPlat, activeSessionID string, list []store.SavedSession) [][]platform.Button {
	if len(list) == 0 {
		return [][]platform.Button{{
			{Label: "➕ 开启新话题", Command: "/new", Style: platform.ButtonPrimary},
		}}
	}
	const perRow = 2
	const maxSessionRows = 4
	var rows [][]platform.Button
	var curRow []platform.Button
	for _, item := range list {
		if len(rows) >= maxSessionRows {
			break
		}
		title := item.Title
		if title == "" {
			title = "未命名对话"
		}
		shortTitle := shortButtonTitle(title, 10)
		btn := platform.Button{
			Label:   fmt.Sprintf("#%d %s", item.ID, shortTitle),
			Command: fmt.Sprintf("/sessions %d", item.ID),
			Style:   platform.ButtonSecondary,
		}
		switch {
		case activeSessionID != "" && item.SessionID == activeSessionID:
			btn.Label = fmt.Sprintf("✓ #%d %s", item.ID, shortTitle)
			btn.Style = platform.ButtonSuccess
		case item.Platform != "" && item.Platform != currentPlat:
			btn.Label = fmt.Sprintf("🔄 #%d [%s] %s", item.ID, shortPlatformName(item.Platform), shortTitle)
			btn.Style = platform.ButtonPrimary
		}
		curRow = append(curRow, btn)
		if len(curRow) == perRow {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 && len(rows) < maxSessionRows {
		rows = append(rows, curRow)
	}
	rows = append(rows, []platform.Button{
		{Label: "➕ 开启新话题", Command: "/new", Style: platform.ButtonPrimary},
	})
	return rows
}

func newTopicButtons(archivedID int64) [][]platform.Button {
	if archivedID <= 0 {
		return nil
	}
	return [][]platform.Button{{
		{Label: fmt.Sprintf("↩️ 切回 #%d", archivedID), Command: fmt.Sprintf("/sessions %d", archivedID), Style: platform.ButtonSecondary},
		{Label: "📋 全部会话", Command: "/sessions", Style: platform.ButtonSecondary},
	}}
}

func taskListButtons(tasks []store.Task) [][]platform.Button {
	if len(tasks) == 0 {
		return nil
	}
	const maxTaskRows = 5
	var rows [][]platform.Button
	for _, t := range tasks {
		if len(rows) >= maxTaskRows {
			break
		}
		toggleBtn := platform.Button{
			Label:   fmt.Sprintf("⏸ 暂停 #%d", t.ID),
			Command: fmt.Sprintf("/task pause %d", t.ID),
			Style:   platform.ButtonSecondary,
		}
		if t.Paused {
			toggleBtn = platform.Button{
				Label:   fmt.Sprintf("🔔 恢复 #%d", t.ID),
				Command: fmt.Sprintf("/task resume %d", t.ID),
				Style:   platform.ButtonSuccess,
			}
		}
		rows = append(rows, []platform.Button{
			{Label: fmt.Sprintf("▶️ 试跑 #%d", t.ID), Command: fmt.Sprintf("/task run %d", t.ID), Style: platform.ButtonPrimary},
			toggleBtn,
			{Label: fmt.Sprintf("🗑 删除 #%d", t.ID), Command: fmt.Sprintf("/task del %d", t.ID), Style: platform.ButtonDanger},
		})
	}
	return rows
}

func shellButtons(enabled bool) [][]platform.Button {
	if enabled {
		return [][]platform.Button{{
			{Label: "🔴 关闭命令执行", Command: "/shell off", Style: platform.ButtonDanger},
		}}
	}
	return [][]platform.Button{{
		{Label: "🟢 开启命令执行", Command: "/shell on", Style: platform.ButtonSuccess},
	}}
}

func memoryListButtons(list []store.Memory) [][]platform.Button {
	if len(list) == 0 {
		return nil
	}
	const perRow = 3
	const maxDelRows = 3
	var rows [][]platform.Button
	var curRow []platform.Button
	for _, m := range list {
		if len(rows) >= maxDelRows {
			break
		}
		curRow = append(curRow, platform.Button{
			Label:   fmt.Sprintf("🗑 删 #%d", m.ID),
			Command: fmt.Sprintf("/memory del %d", m.ID),
			Style:   platform.ButtonSecondary,
		})
		if len(curRow) == perRow {
			rows = append(rows, curRow)
			curRow = nil
		}
	}
	if len(curRow) > 0 && len(rows) < maxDelRows {
		rows = append(rows, curRow)
	}
	rows = append(rows, []platform.Button{
		{Label: "🧹 清空全部记忆", Command: "/memory clear", Style: platform.ButtonDanger},
	})
	return rows
}

func upgradeButtons(botSt upgrade.BotStatus, cliSt engine.CLIStatus, hasCLI bool, focus string, autoEnabled bool) [][]platform.Button {
	var rows [][]platform.Button
	botReady := (focus == "" || focus == "bot") && botSt.UpdateAvailable && botSt.Writable && botSt.Latest != ""
	cliReady := (focus == "" || focus == "cli") && hasCLI && (cliSt.UpdateAvailable || cliSt.DirectUpdateReady) && cliSt.Writable

	var actionRow []platform.Button
	if botReady {
		cmd := "/upgrade confirm bot " + botSt.Latest
		if len(cmd) > 64 {
			cmd = "/upgrade confirm bot"
		}
		actionRow = append(actionRow, platform.Button{
			Label:   fmt.Sprintf("⬆️ 升级 Bot (%s)", botSt.Latest),
			Command: cmd,
			Style:   platform.ButtonPrimary,
		})
	}
	if cliReady {
		cliLabel := "🔄 更新 CLI"
		if cliSt.Latest != "" {
			cliLabel = fmt.Sprintf("⬆️ 升级 CLI (%s)", cliSt.Latest)
		}
		style := platform.ButtonPrimary
		if botReady {
			style = platform.ButtonSecondary
		}
		actionRow = append(actionRow, platform.Button{
			Label:   cliLabel,
			Command: "/upgrade confirm cli",
			Style:   style,
		})
	}
	if len(actionRow) > 0 {
		rows = append(rows, actionRow)
	}

	if focus == "" && botReady && cliReady {
		allCmd := "/upgrade confirm all " + botSt.Latest
		if len(allCmd) > 64 {
			allCmd = "/upgrade confirm all"
		}
		rows = append(rows, []platform.Button{
			{Label: "🚀 两者一起升级", Command: allCmd, Style: platform.ButtonSuccess},
			{Label: "取消", Command: "/upgrade cancel", Style: platform.ButtonSecondary},
		})
	}

	autoBtn := platform.Button{
		Label:   "⏰ 开启自动升级",
		Command: "/upgrade auto on",
		Style:   platform.ButtonSuccess,
	}
	if autoEnabled {
		autoBtn = platform.Button{
			Label:   "⏸ 关闭自动升级",
			Command: "/upgrade auto off",
			Style:   platform.ButtonSecondary,
		}
	}
	utilRow := []platform.Button{
		{Label: "🔄 重新检查", Command: "/upgrade check", Style: platform.ButtonSecondary},
		autoBtn,
	}
	if len(actionRow) > 0 && !(focus == "" && botReady && cliReady) {
		utilRow = append(utilRow, platform.Button{
			Label: "取消", Command: "/upgrade cancel", Style: platform.ButtonSecondary,
		})
	}
	rows = append(rows, utilRow)
	return rows
}

func autoUpgradeToggleButtons(enabled bool) [][]platform.Button {
	if enabled {
		return [][]platform.Button{{
			{Label: "⏸ 关闭自动升级", Command: "/upgrade auto off", Style: platform.ButtonDanger},
			{Label: "🔄 检查当前更新", Command: "/upgrade check", Style: platform.ButtonSecondary},
		}}
	}
	return [][]platform.Button{{
		{Label: "⏰ 开启自动升级", Command: "/upgrade auto on", Style: platform.ButtonSuccess},
		{Label: "🔄 检查当前更新", Command: "/upgrade check", Style: platform.ButtonSecondary},
	}}
}
