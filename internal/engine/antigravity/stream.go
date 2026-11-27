package antigravity

import (
	"bufio"
	"encoding/json"
	"io"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/rshun/agent-bot/internal/engine"
	"github.com/rshun/agent-bot/internal/store"
)

// ToolUse represents a single tool invocation emitted by Antigravity CLI during a turn.
type ToolUse = engine.ToolUse

// HeadlessResult is the normalized output from one Antigravity CLI turn.
type HeadlessResult = engine.HeadlessResult

// RunError represents an error reported by the Antigravity CLI.
type RunError = engine.RunError

var reSavedImagePath = regexp.MustCompile(`(?i)(?:[A-Za-z]:\\|/)[^\s"'` + "`" + `<>|]+?\.(?:png|jpe?g|webp|gif)`)

type streamEnvelope struct {
	Event          string          `json:"event"`
	ConversationID string          `json:"conversation_id"`
	Init           *initPayload    `json:"init"`
	StepUpdate     *stepPayload    `json:"step_update"`
	Result         *resultPayload  `json:"result"`
	Error          string          `json:"error"`
	Status         string          `json:"status"`
	Response       string          `json:"response"`
	Structured     json.RawMessage `json:"structured_output"`
	Usage          *agyUsage       `json:"usage"`
}

type initPayload struct {
	Cwd            string   `json:"cwd"`
	Tools          []string `json:"tools"`
	PermissionMode string   `json:"permission_mode"`
	Model          string   `json:"model"`
	Agent          string   `json:"agent"`
}

type stepPayload struct {
	ConversationID string    `json:"conversation_id"`
	StepIndex      int       `json:"step_index"`
	State          string    `json:"state"`
	StepType       string    `json:"step_type"`
	ToolName       string    `json:"tool_name"`
	TextDelta      string    `json:"text_delta"`
	Usage          *agyUsage `json:"usage"`
	ToolInfo       *toolInfo `json:"tool_info"`
}

type toolInfo struct {
	Name       string          `json:"name"`
	Parameters json.RawMessage `json:"parameters"`
	Output     string          `json:"output"`
	Error      *struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

type resultPayload struct {
	ConversationID   string          `json:"conversation_id"`
	Status           string          `json:"status"`
	Response         string          `json:"response"`
	Error            string          `json:"error"`
	StructuredOutput json.RawMessage `json:"structured_output"`
	Usage            *agyUsage       `json:"usage"`
}

type agyUsage struct {
	InputTokens     *int64 `json:"input_tokens"`
	OutputTokens    *int64 `json:"output_tokens"`
	ThinkingTokens  *int64 `json:"thinking_tokens"`
	CacheReadTokens *int64 `json:"cache_read_tokens"`
	TotalTokens     *int64 `json:"total_tokens"`
}

func (u *agyUsage) toStore() *store.UsageTokens {
	if u == nil {
		return nil
	}
	return engine.NormalizeUsage(&store.UsageTokens{
		InputTokens:           u.InputTokens,
		CachedInputTokens:     u.CacheReadTokens,
		OutputTokens:          u.OutputTokens,
		ReasoningOutputTokens: u.ThinkingTokens,
	})
}

// ParseStream consumes NDJSON (`--output-format stream-json`) or single-JSON
// (`--output-format json`) output from `agy`, emits normalized tool progress events,
// and returns the final HeadlessResult.
func ParseStream(r io.Reader, onTool func(ToolUse)) (HeadlessResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var (
		res        HeadlessResult
		textBuf    strings.Builder
		seenSteps  = make(map[int]bool)
		seenImages = make(map[string]bool)
		gotResult  bool
	)

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev streamEnvelope
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.ConversationID != "" {
			res.SessionID = ev.ConversationID
		}

		switch ev.Event {
		case "init":
			// Conversation ID already captured above if present.

		case "step_update":
			su := ev.StepUpdate
			if su == nil {
				continue
			}
			if su.ConversationID != "" {
				res.SessionID = su.ConversationID
			}
			if su.StepType == "agent_response" && su.TextDelta != "" {
				textBuf.WriteString(su.TextDelta)
			}
			if su.StepType == "tool" {
				tName := strings.TrimSpace(su.ToolName)
				var params json.RawMessage
				if su.ToolInfo != nil {
					if tName == "" {
						tName = strings.TrimSpace(su.ToolInfo.Name)
					}
					params = su.ToolInfo.Parameters
				}
				if onTool != nil && tName != "" && !seenSteps[su.StepIndex] {
					seenSteps[su.StepIndex] = true
					onTool(normalizeToolUse(tName, params))
				}
				if su.ToolInfo != nil && strings.EqualFold(su.State, "DONE") {
					if isImageGenTool(tName) && su.ToolInfo.Error == nil {
						for _, p := range extractGeneratedImages(su.ToolInfo.Output) {
							if !seenImages[p] {
								seenImages[p] = true
								res.GeneratedImages = append(res.GeneratedImages, p)
							}
						}
					}
				}
			}

		case "result":
			rp := ev.Result
			if rp == nil {
				continue
			}
			if rp.ConversationID != "" {
				res.SessionID = rp.ConversationID
			}
			if rp.Usage != nil {
				res.Usage = rp.Usage.toStore()
			}
			res.StopReason = rp.Status
			res.Structured = rp.StructuredOutput
			gotResult = true
			if rp.Status != "" && !strings.EqualFold(rp.Status, "SUCCESS") {
				msg := strings.TrimSpace(rp.Error)
				if msg == "" {
					msg = "Antigravity CLI failed with status " + rp.Status
				}
				partial := strings.TrimSpace(rp.Response)
				if partial == "" {
					partial = strings.TrimSpace(textBuf.String())
				}
				return HeadlessResult{}, &RunError{
					Message:     msg,
					SessionID:   res.SessionID,
					PartialText: partial,
				}
			}
			if rp.Response != "" {
				res.Text = rp.Response
			} else {
				res.Text = textBuf.String()
			}

		case "error":
			msg := strings.TrimSpace(ev.Error)
			if msg == "" {
				msg = "Antigravity CLI reported an error."
			}
			return HeadlessResult{}, &RunError{
				Message:     msg,
				SessionID:   res.SessionID,
				PartialText: strings.TrimSpace(textBuf.String()),
			}

		case "":
			// Fallback for flat `--output-format json` envelope.
			if ev.Status != "" {
				if ev.Usage != nil {
					res.Usage = ev.Usage.toStore()
				}
				res.StopReason = ev.Status
				res.Structured = ev.Structured
				gotResult = true
				if !strings.EqualFold(ev.Status, "SUCCESS") {
					msg := strings.TrimSpace(ev.Error)
					if msg == "" {
						msg = "Antigravity CLI failed with status " + ev.Status
					}
					return HeadlessResult{}, &RunError{
						Message:     msg,
						SessionID:   res.SessionID,
						PartialText: strings.TrimSpace(ev.Response),
					}
				}
				res.Text = ev.Response
			}
		}
	}

	if err := sc.Err(); err != nil && !gotResult {
		return res, &RunError{
			Message:     "Stream interrupted while reading Antigravity output.",
			SessionID:   res.SessionID,
			PartialText: strings.TrimSpace(textBuf.String()),
		}
	}
	if !gotResult && textBuf.Len() > 0 {
		res.Text = textBuf.String()
	}
	return res, nil
}

func normalizeToolUse(name string, params json.RawMessage) ToolUse {
	if strings.EqualFold(name, "call_mcp_tool") && len(params) > 0 {
		var call struct {
			ServerName string          `json:"ServerName"`
			ToolName   string          `json:"ToolName"`
			Arguments  json.RawMessage `json:"Arguments"`
		}
		if json.Unmarshal(params, &call) == nil && call.ToolName != "" {
			normName := call.ToolName
			if call.ServerName != "" {
				normName = call.ServerName + "__" + call.ToolName
			}
			inp := call.Arguments
			if len(inp) == 0 {
				inp = json.RawMessage(`{}`)
			}
			return ToolUse{Name: normName, Input: inp}
		}
	}
	if strings.HasPrefix(name, "mcp_agentbot_") {
		tool := strings.TrimPrefix(name, "mcp_agentbot_")
		return ToolUse{Name: "agentbot__" + tool, Input: params}
	}
	return ToolUse{Name: name, Input: params}
}

func isImageGenTool(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "generate_image", "image_gen", "image_edit":
		return true
	default:
		return false
	}
}

func extractGeneratedImages(output string) []string {
	output = strings.TrimSpace(output)
	if output == "" {
		return nil
	}
	var out []string
	seen := make(map[string]bool)
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if filepath.IsAbs(p) && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}

	// Structured JSON output check.
	if strings.HasPrefix(output, "{") {
		var payload struct {
			Path   string   `json:"path"`
			Paths  []string `json:"paths"`
			Images []struct {
				Path string `json:"path"`
			} `json:"images"`
		}
		if json.Unmarshal([]byte(output), &payload) == nil {
			add(payload.Path)
			for _, p := range payload.Paths {
				add(p)
			}
			for _, img := range payload.Images {
				add(img.Path)
			}
			if len(out) > 0 {
				return out
			}
		}
	}

	for _, match := range reSavedImagePath.FindAllString(output, -1) {
		add(match)
	}
	return out
}
