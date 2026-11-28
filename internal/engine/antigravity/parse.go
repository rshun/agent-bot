package antigravity

import (
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/rshun/agent-bot/internal/engine"
)

// ModelList is the normalized model catalog returned by Models.
type ModelList = engine.ModelList

// Allowance holds parsed quota/credit state returned by Allowance.
type Allowance = engine.Allowance

// ValidModelName validates a model slug before passing it to `agy --model`.
var ValidModelName = engine.ValidModelName

var (
	ansiRE      = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)
	rePercent   = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*%`)
	reResetDate = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}(?::\d{2})?`)
)

func isAntigravityModelSlug(slug string) bool {
	if !ValidModelName(slug) {
		return false
	}
	// Real Antigravity CLI model slugs are lowercase identifiers containing at least
	// a hyphen or digit (e.g. "gemini-4-neon-high", "claude-opus-5-5-high", "gpt-oss-120b-medium").
	// Reject capitalized or plain status words such as "Fetching", "Loading", "Available", "Model".
	if slug != strings.ToLower(slug) {
		return false
	}
	return strings.Contains(slug, "-") || strings.ContainsAny(slug, "0123456789")
}

// ParseModels parses `agy models` stdout into a deduplicated ModelList.
func ParseModels(raw string) ModelList {
	cleaned := ansiRE.ReplaceAllString(raw, "")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	var (
		names   []string
		seen    = make(map[string]bool)
		curName string
		defName string
	)
	for _, rawLine := range strings.Split(cleaned, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "available models") ||
			strings.HasPrefix(lower, "model ") ||
			strings.HasPrefix(lower, "fetching") ||
			strings.HasPrefix(lower, "loading") ||
			strings.HasPrefix(lower, "checking") ||
			strings.HasPrefix(lower, "connecting") ||
			strings.HasPrefix(line, "---") ||
			strings.HasPrefix(line, "===") {
			continue
		}
		isCurrent := strings.HasPrefix(line, "*") || strings.Contains(lower, "(current)")
		isDefault := strings.Contains(lower, "(default)")
		line = strings.TrimLeft(line, "*-• ")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		slug := strings.TrimSpace(fields[0])
		if !isAntigravityModelSlug(slug) {
			continue
		}
		if !seen[slug] {
			seen[slug] = true
			names = append(names, slug)
		}
		if isCurrent && curName == "" {
			curName = slug
		}
		if isDefault && defName == "" {
			defName = slug
		}
	}
	if defName == "" && len(names) > 0 {
		defName = names[0]
	}
	if curName == "" {
		curName = defName
	}
	return ModelList{
		Default:   defName,
		Current:   curName,
		Names:     names,
		Available: append([]string(nil), names...),
	}
}

// ParseUsageOutput parses the plain-text response of `agy -p /usage` into Allowance.
func ParseUsageOutput(raw string) (Allowance, error) {
	cleaned := strings.TrimSpace(ansiRE.ReplaceAllString(raw, ""))
	if cleaned == "" {
		return Allowance{}, errors.New("agy /usage returned empty output")
	}
	lower := strings.ToLower(cleaned)
	if strings.Contains(lower, "gemini_api_key") || strings.Contains(lower, "no account quota") {
		return Allowance{}, errors.New("当前使用 GEMINI_API_KEY 模式，无账号订阅额度可查询")
	}
	if strings.Contains(lower, "not signed in") || strings.Contains(lower, "authentication required") || strings.Contains(lower, "sign in") {
		return Allowance{}, errors.New("antigravity auth required")
	}

	var (
		buckets []engine.QuotaBucket
		plan    string
		email   string
	)
	for _, rawLine := range strings.Split(cleaned, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		l := strings.ToLower(line)
		if strings.Contains(l, "@") && email == "" {
			for _, tok := range strings.Fields(line) {
				if strings.Contains(tok, "@") && strings.Contains(tok, ".") {
					email = strings.Trim(tok, "<>()[],.;:")
					break
				}
			}
		}
		if strings.HasPrefix(l, "plan:") || strings.HasPrefix(l, "tier:") {
			if _, v, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(v) != "" {
				plan = strings.TrimSpace(v)
			}
		}
		pctMatch := rePercent.FindStringSubmatch(line)
		if len(pctMatch) < 2 {
			continue
		}
		val, err := strconv.ParseFloat(pctMatch[1], 64)
		if err != nil {
			continue
		}
		pct := int(val + 0.5)
		if pct < 0 {
			pct = 0
		}
		if pct > 100 {
			pct = 100
		}
		var rem, used int
		if strings.Contains(l, "used") || strings.Contains(l, "已用") {
			used = pct
			rem = 100 - pct
		} else {
			rem = pct
			used = 100 - pct
		}
		label := strings.TrimSpace(strings.SplitN(line, ":", 2)[0])
		if label == "" || label == line {
			label = "Quota"
		}
		win := &engine.QuotaWindow{
			UsedPercent:      used,
			RemainingPercent: rem,
		}
		if strings.Contains(l, "5h") || strings.Contains(l, "5-hour") || strings.Contains(l, "5 hour") {
			win.WindowDurationMins = 5 * 60
		} else if strings.Contains(l, "7d") || strings.Contains(l, "weekly") || strings.Contains(l, "week") {
			win.WindowDurationMins = 7 * 24 * 60
		}
		if tsRaw := reResetDate.FindString(line); tsRaw != "" {
			if ts, err := parseFlexibleTime(tsRaw); err == nil {
				win.ResetsAt = ts
			}
		}
		buckets = append(buckets, engine.QuotaBucket{
			LimitID:   strings.ToLower(strings.ReplaceAll(label, " ", "_")),
			LimitName: label,
			PlanType:  plan,
			Primary:   win,
		})
	}

	if len(buckets) == 0 {
		return Allowance{}, errors.New("无法从 agy /usage 输出解析额度信息：" + firstLine(cleaned))
	}
	return Allowance{
		PlanType:     plan,
		AccountEmail: email,
		Buckets:      buckets,
		QueriedAt:    time.Now(),
	}, nil
}

func parseFlexibleTime(s string) (time.Time, error) {
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02T15:04",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, errors.New("unrecognized timestamp")
}
