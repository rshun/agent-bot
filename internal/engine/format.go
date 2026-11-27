package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rshun/agent-bot/internal/store"
)

var (
	reBearerSecret = regexp.MustCompile(`(?i)\b(Bearer\s+)([^\s"',;]+)`)
	reTokenPrefix  = regexp.MustCompile(`\b(sk|xai|ghp|gho|github_pat|glpat|xoxb|xoxp)-[A-Za-z0-9_\-]{6,}`)
	reBotURLToken  = regexp.MustCompile(`\b(bot\d+:)[A-Za-z0-9_\-]{10,}`)
	reSecretKV     = regexp.MustCompile(`(?i)\b([A-Za-z0-9_]*(?:password|passwd|pwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization|cookie|credential)[A-Za-z0-9_]*)\s*([=:])\s*([^\s"',;\n]+)`)
	reParenURL     = regexp.MustCompile(`(?i)\(\s*(?:https?:\s*)?https?://[^)\s]+\s*\)`)
	reHTTPURL      = regexp.MustCompile(`https?://\S+`)
	reMultiSpace   = regexp.MustCompile(`\s{2,}`)
)

// RedactSensitive masks tokens, API keys, passwords, and auth headers in CLI outputs.
func RedactSensitive(text string) string {
	if text == "" {
		return ""
	}
	out := reBearerSecret.ReplaceAllString(text, "${1}****")
	out = reTokenPrefix.ReplaceAllString(out, "${1}-****")
	out = reBotURLToken.ReplaceAllString(out, "${1}****")
	out = reSecretKV.ReplaceAllStringFunc(out, func(m string) string {
		sub := reSecretKV.FindStringSubmatch(m)
		if len(sub) != 4 {
			return m
		}
		if strings.EqualFold(sub[3], "bearer") {
			return m
		}
		return sub[1] + sub[2] + " ****"
	})
	return out
}

// FormatAllowance renders subscription remaining quota or rate-limit windows for chat display.
func FormatAllowance(a Allowance) string {
	if len(a.Buckets) > 0 {
		lines := []string{"当前账号剩余额度："}
		plan := a.PlanType
		if plan == "" {
			plan = a.Tier
		}
		if plan != "" {
			lines = append(lines, fmt.Sprintf("套餐：%s", plan))
		}
		multi := len(a.Buckets) > 1
		for _, b := range a.Buckets {
			prefix := ""
			if multi {
				label := b.LimitName
				if label == "" {
					label = b.LimitID
				}
				prefix = fmt.Sprintf("[%s] ", label)
			}
			if b.Primary != nil {
				lines = append(lines, prefix+formatQuotaWindowLine("主窗口", b.Primary))
			}
			if b.Secondary != nil {
				lines = append(lines, prefix+formatQuotaWindowLine("次窗口", b.Secondary))
			}
		}
		if a.Truncated {
			lines = append(lines, "其余额度桶已省略。")
		}
		if !a.QueriedAt.IsZero() {
			lines = append(lines, fmt.Sprintf("查询时间：%s", a.QueriedAt.Local().Format("2006-01-02 15:04:05")))
		}
		return strings.Join(lines, "\n")
	}

	var b strings.Builder
	if a.Tier != "" {
		fmt.Fprintf(&b, "套餐：%s\n", a.Tier)
	}
	if !a.HasUsedPercent {
		b.WriteString("账单里没有剩余额度。")
		return strings.TrimSpace(b.String())
	}
	remaining := 100 - a.UsedPercent
	if remaining < 0 {
		remaining = 0
	}
	if remaining > 100 {
		remaining = 100
	}
	fmt.Fprintf(&b, "本期剩余：%s（已用 %s）\n", formatPercent(remaining), formatPercent(a.UsedPercent))
	if kind := periodKind(a.PeriodKind); kind != "" {
		fmt.Fprintf(&b, "周期：%s\n", kind)
	}
	if !a.PeriodEnd.IsZero() {
		fmt.Fprintf(&b, "重置时间：%s\n", a.PeriodEnd.In(time.Local).Format("2006-01-02 15:04 MST"))
	}
	for _, product := range a.Products {
		fmt.Fprintf(&b, "%s 已用 %s\n", productLabel(product.Name), formatPercent(product.UsagePercent))
	}
	return strings.TrimSpace(b.String())
}

func formatQuotaWindowLine(label string, w *QuotaWindow) string {
	if w == nil {
		return ""
	}
	dur := formatWindowDuration(w.WindowDurationMins)
	title := label
	if dur != "" {
		title = fmt.Sprintf("%s（%s）", label, dur)
	}
	line := fmt.Sprintf("%s：剩余 %d%%（已用 %d%%）", title, w.RemainingPercent, w.UsedPercent)
	if !w.ResetsAt.IsZero() {
		line += fmt.Sprintf("，重置于 %s", w.ResetsAt.Local().Format("2006-01-02 15:04:05"))
	}
	return line
}

func formatWindowDuration(mins int) string {
	if mins <= 0 {
		return ""
	}
	if mins%(60*24) == 0 {
		return fmt.Sprintf("%d天", mins/(60*24))
	}
	if mins%60 == 0 {
		return fmt.Sprintf("%d小时", mins/60)
	}
	return fmt.Sprintf("%d分钟", mins)
}

func formatPercent(v float64) string {
	if v < 0 {
		v = 0
	}
	text := strconv.FormatFloat(v, 'f', 1, 64)
	text = strings.TrimSuffix(text, ".0")
	return text + "%"
}

func periodKind(raw string) string {
	switch raw {
	case "USAGE_PERIOD_TYPE_WEEKLY":
		return "每周"
	case "USAGE_PERIOD_TYPE_MONTHLY":
		return "每月"
	case "":
		return ""
	default:
		return raw
	}
}

func productLabel(name string) string {
	switch name {
	case "GrokBuild":
		return "Grok Build"
	case "GrokChat":
		return "Grok Chat"
	default:
		return name
	}
}

// NormalizeUsage clamps any negative token counts in a UsageTokens struct to 0.
func NormalizeUsage(raw *store.UsageTokens) *store.UsageTokens {
	if raw == nil {
		return nil
	}
	norm := func(p *int64) *int64 {
		if p == nil {
			return nil
		}
		v := max(int64(0), *p)
		return &v
	}
	return &store.UsageTokens{
		InputTokens:           norm(raw.InputTokens),
		CachedInputTokens:     norm(raw.CachedInputTokens),
		OutputTokens:          norm(raw.OutputTokens),
		ReasoningOutputTokens: norm(raw.ReasoningOutputTokens),
	}
}

func formatTokenCount(p *int64) string {
	if p == nil {
		return "未知"
	}
	return formatIntWithCommas(*p) + " tokens"
}

func formatIntWithCommas(n int64) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
		if len(s) > rem {
			out = append(out, ',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		out = append(out, s[i:i+3]...)
		if i+3 < len(s) {
			out = append(out, ',')
		}
	}
	return string(out)
}

// FormatUsage renders the last turn's CLI token usage snapshot in Chinese.
func FormatUsage(report *store.UsageReport, busy bool) string {
	const unavailableNote = "说明：如需查询账户订阅或窗口剩余额度，也可使用 /quota。"
	if report == nil {
		if busy {
			return strings.Join([]string{
				"当前正在处理上一条消息，本次用量尚未生成。",
				"请稍后再试。",
				unavailableNote,
			}, "\n")
		}
		return strings.Join([]string{
			"暂无最近一轮 Token 用量记录。",
			"先发送一条普通消息后再执行 /usage。",
			unavailableNote,
		}, "\n")
	}

	var lines []string
	if busy {
		lines = append(lines, "当前有任务正在执行，以下为最近一次已完成请求的数据：")
	}
	switch report.Status {
	case "succeeded", "completed":
		lines = append(lines, "最近一次请求 Token 用量：")
	case "cancelled":
		lines = append(lines, "最近一次请求已取消。")
	default:
		lines = append(lines, "最近一次请求执行失败。")
	}
	if !report.At.IsZero() {
		lines = append(lines, fmt.Sprintf("时间：%s", report.At.Local().Format("2006-01-02 15:04:05")))
	}
	if report.Tokens != nil {
		u := NormalizeUsage(report.Tokens)
		var total *int64
		if u.InputTokens != nil || u.OutputTokens != nil {
			var sum int64
			if u.InputTokens != nil {
				sum += *u.InputTokens
			}
			if u.OutputTokens != nil {
				sum += *u.OutputTokens
			}
			total = &sum
		}
		lines = append(lines,
			fmt.Sprintf("输入：%s", formatTokenCount(u.InputTokens)),
			fmt.Sprintf("缓存输入：%s", formatTokenCount(u.CachedInputTokens)),
			fmt.Sprintf("输出：%s", formatTokenCount(u.OutputTokens)),
			fmt.Sprintf("推理输出：%s", formatTokenCount(u.ReasoningOutputTokens)),
			fmt.Sprintf("输入 + 输出：%s", formatTokenCount(total)),
		)
	} else {
		lines = append(lines, "CLI 未返回 token 统计。")
	}
	return strings.Join(lines, "\n")
}

// UserFacingFor converts a CLI failure into a user-facing chat message tailored to engineName.
func UserFacingFor(err error, engineName string) string {
	if err == nil {
		return ""
	}
	cliCmd := "grok"
	loginCmd := "grok login"
	switch strings.ToLower(strings.TrimSpace(engineName)) {
	case "codex":
		cliCmd = "codex"
		loginCmd = "codex login"
	case "antigravity", "agy":
		cliCmd = "agy"
		loginCmd = "agy"
	}
	msg := err.Error()
	lower := strings.ToLower(msg)
	switch {
	case strings.Contains(lower, "credit limit"),
		strings.Contains(lower, "usage limit"),
		strings.Contains(lower, "out of credits"),
		strings.Contains(lower, "spending cap"),
		strings.Contains(lower, "spending limit"),
		strings.Contains(lower, "free-usage-exhausted"),
		strings.Contains(lower, "usage_limit_reached"),
		strings.Contains(lower, "usage_pool_exhausted"),
		strings.Contains(lower, "quota"):
		return "订阅额度已用完。可以用 /usage 或 /quota 查看剩余情况。"
	case strings.Contains(lower, "grok auth"),
		strings.Contains(lower, "codex auth"),
		strings.Contains(lower, "antigravity auth"),
		strings.Contains(lower, "agy auth"),
		strings.Contains(lower, "session token"):
		return fmt.Sprintf("读不到 %s 登录信息。在运行 bot 的这个用户下执行 %s 登录后再试。", cliCmd, loginCmd)
	case strings.Contains(lower, "login"),
		strings.Contains(lower, "unauthorized"),
		strings.Contains(lower, "authentication"),
		strings.Contains(lower, "billing http 401"),
		strings.Contains(lower, "billing http 403"),
		strings.Contains(lower, "auth"):
		return fmt.Sprintf("%s 未登录或登录已过期。在运行 bot 的这个用户下执行 %s 登录后再试。", cliCmd, loginCmd)
	case IsMaxTurns(err):
		var runErr *RunError
		if errors.As(err, &runErr) && strings.TrimSpace(runErr.PartialText) != "" {
			return RedactSensitive(strings.TrimSpace(runErr.PartialText)) + "\n\n（本轮工具调用次数已达上限，当前进度已保存在会话中，直接回复「继续」即可接着往下执行。）"
		}
		return "本轮工具调用次数已达上限，当前进度已保存在会话中，直接回复「继续」即可接着往下执行。"
	case strings.Contains(lower, "deadline") || strings.Contains(lower, "timeout") || strings.Contains(lower, "context canceled"):
		return "处理超时。请再发一次。"
	default:
		flat := RedactSensitive(strings.Join(strings.Fields(msg), " "))
		if len([]rune(flat)) > 500 {
			flat = string([]rune(flat)[:500]) + "…"
		}
		return "处理失败：" + flat
	}
}

// UserFacing formats a CLI error using default heuristics.
func UserFacing(err error) string {
	return UserFacingFor(err, "grok")
}

type mcpListEntry struct {
	Name    string `json:"name"`
	Enabled *bool  `json:"enabled"`
	Status  string `json:"status"`
}

// FormatMCPList extracts MCP server names from JSON, table, or text CLI output,
// filtering reserved internal channels and stripping all URLs and command arguments.
func FormatMCPList(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.HasPrefix(trimmed, "[") {
		var entries []mcpListEntry
		if err := json.Unmarshal([]byte(trimmed), &entries); err == nil {
			return formatMCPEntries(entries)
		}
	}
	if strings.HasPrefix(trimmed, "{") {
		var wrapped struct {
			Servers []mcpListEntry `json:"servers"`
		}
		if err := json.Unmarshal([]byte(trimmed), &wrapped); err == nil && wrapped.Servers != nil {
			return formatMCPEntries(wrapped.Servers)
		}
	}
	lowerTrimmed := strings.ToLower(trimmed)
	if strings.HasPrefix(lowerTrimmed, "no mcp servers") || strings.HasPrefix(lowerTrimmed, "no mcp server") {
		return ""
	}

	var lines []string
	inSpaceTable := false
	for _, rawLine := range strings.Split(trimmed, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || isTableRuleLine(line) {
			continue
		}
		if strings.HasSuffix(line, ":") {
			inSpaceTable = false
			continue
		}
		if strings.HasPrefix(line, "|") || strings.HasPrefix(line, "│") {
			name, disabled, header := parsePipeTableRow(line)
			if header || name == "" || IsReservedMCPName(name) {
				continue
			}
			lines = append(lines, formatMCPName(name, disabled))
			continue
		}
		if cols := reMultiSpace.Split(line, -1); len(cols) >= 2 {
			if isSpaceTableHeader(cols) {
				inSpaceTable = true
				continue
			}
			if inSpaceTable {
				name := strings.TrimSpace(cols[0])
				if name == "" || IsReservedMCPName(name) {
					continue
				}
				disabled := false
				for _, col := range cols[1:] {
					c := strings.Trim(strings.TrimSpace(col), "()")
					if strings.EqualFold(c, "disabled") {
						disabled = true
						break
					}
				}
				lines = append(lines, formatMCPName(name, disabled))
				continue
			}
		}
		if idx := strings.IndexByte(line, ':'); idx > 0 && !strings.ContainsAny(line[:idx], " \t(") {
			name := strings.TrimSpace(line[:idx])
			rest := strings.TrimSpace(line[idx+1:])
			if name == "" || rest == "" || IsReservedMCPName(name) {
				continue
			}
			disabled := strings.HasSuffix(strings.ToLower(rest), "(disabled)") || strings.EqualFold(rest, "disabled")
			lines = append(lines, formatMCPName(name, disabled))
			continue
		}
		cleaned := reParenURL.ReplaceAllString(line, "")
		cleaned = reHTTPURL.ReplaceAllString(cleaned, "")
		fields := strings.Fields(cleaned)
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		if name == "" || IsReservedMCPName(name) {
			continue
		}
		disabled := false
		for _, tok := range fields[1:] {
			if strings.EqualFold(strings.Trim(tok, "()"), "disabled") {
				disabled = true
				break
			}
		}
		lines = append(lines, formatMCPName(name, disabled))
	}
	return strings.Join(lines, "\n")
}

func formatMCPEntries(entries []mcpListEntry) string {
	var lines []string
	for _, e := range entries {
		name := strings.TrimSpace(e.Name)
		if name == "" || IsReservedMCPName(name) {
			continue
		}
		disabled := (e.Enabled != nil && !*e.Enabled) || strings.EqualFold(strings.TrimSpace(e.Status), "disabled")
		lines = append(lines, formatMCPName(name, disabled))
	}
	return strings.Join(lines, "\n")
}

func formatMCPName(name string, disabled bool) string {
	if disabled {
		return name + " (disabled)"
	}
	return name
}

func isTableRuleLine(line string) bool {
	for _, r := range line {
		switch r {
		case '+', '-', '=', '_', '|', '│', '─', '━', '┼', '├', '┤', '┌', '┐', '└', '┘', '┬', '┴', ' ':
		default:
			return false
		}
	}
	return true
}

func parsePipeTableRow(line string) (name string, disabled bool, isHeader bool) {
	normalized := strings.ReplaceAll(line, "│", "|")
	rawCols := strings.Split(normalized, "|")
	var cols []string
	for _, c := range rawCols {
		c = strings.TrimSpace(c)
		if c != "" {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return "", false, false
	}
	if isSpaceTableHeader(cols) {
		return "", false, true
	}
	for _, col := range cols[1:] {
		if strings.EqualFold(strings.Trim(col, "()"), "disabled") {
			disabled = true
			break
		}
	}
	return cols[0], disabled, false
}

func isSpaceTableHeader(cols []string) bool {
	if len(cols) < 2 || !strings.EqualFold(strings.TrimSpace(cols[0]), "name") {
		return false
	}
	for _, col := range cols[1:] {
		switch strings.ToLower(strings.TrimSpace(col)) {
		case "status", "url", "command", "args", "env", "cwd", "transport", "enabled", "auth", "working directory":
			return true
		}
	}
	return false
}
