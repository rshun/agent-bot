package antigravity

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// PolicyEnv passes the serialized TurnPolicy JSON to the `agent-bot agy-hook` subprocess.
const PolicyEnv = "AGENTBOT_AGY_POLICY"

const (
	managedHookName   = "agentbot-managed-policy"
	managedRulesHead  = "<!-- managed by agent-bot; do not edit -->"
	maxHookInputBytes = 1 << 20
)

// ErrUnmanagedConfig indicates an existing workspace hook or rule file is user-managed.
var ErrUnmanagedConfig = errors.New("workspace antigravity config is not managed by agent-bot")

// TurnPolicy describes the per-turn tool permissions enforced by PreToolUse hooks.
type TurnPolicy struct {
	JSONSchemaOnly bool     `json:"json_schema_only,omitempty"`
	ShellTools     bool     `json:"shell_tools,omitempty"`
	WebSearch      bool     `json:"web_search,omitempty"`
	WriteDirs      []string `json:"write_dirs,omitempty"`
	InboxDir       string   `json:"inbox_dir,omitempty"`
	MediaDir       string   `json:"media_dir,omitempty"`
	DenyPaths      []string `json:"deny_paths,omitempty"`
}

// EnsureWorkspaceHooks writes <workspaceDir>/.agents/hooks.json so every Antigravity turn
// routes tool calls through `agent-bot agy-hook` for strict per-turn enforcement.
func EnsureWorkspaceHooks(workspaceDir, botBin string) error {
	if strings.TrimSpace(workspaceDir) == "" || strings.TrimSpace(botBin) == "" {
		return nil
	}
	path := filepath.Join(workspaceDir, ".agents", "hooks.json")
	current, err := os.ReadFile(path)
	switch {
	case err == nil:
		if !isManagedHooksFile(current) {
			return ErrUnmanagedConfig
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	payload := map[string]any{
		managedHookName: map[string]any{
			"PreToolUse": []map[string]any{
				{
					"matcher": ".*",
					"hooks": []map[string]any{
						{
							"type":    "command",
							"command": hookCommand(botBin),
							"timeout": 10,
						},
					},
				},
			},
		},
	}
	encoded, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	content := string(encoded) + "\n"
	if string(current) == content {
		return nil
	}
	return writeAtomicBytes(path, []byte(content), 0o600)
}

func isManagedHooksFile(raw []byte) bool {
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parsed); err != nil || parsed == nil {
		return false
	}
	for k := range parsed {
		if k != managedHookName {
			return false
		}
	}
	return true
}

func hookCommand(botBin string) string {
	if runtime.GOOS == "windows" {
		escaped := strings.ReplaceAll(botBin, `"`, `\"`)
		return `"` + escaped + `" agy-hook`
	}
	escaped := strings.ReplaceAll(botBin, `'`, `'\''`)
	return `'` + escaped + `' agy-hook`
}

// EnsureWorkspaceRules writes <workspaceDir>/.agents/GEMINI.md with baseline bot boundaries.
func EnsureWorkspaceRules(workspaceDir string) error {
	if strings.TrimSpace(workspaceDir) == "" {
		return nil
	}
	path := filepath.Join(workspaceDir, ".agents", "GEMINI.md")
	current, err := os.ReadFile(path)
	switch {
	case err == nil:
		s := string(current)
		if !strings.HasPrefix(s, managedRulesHead+"\n") && !strings.HasPrefix(s, managedRulesHead+"\r\n") {
			return ErrUnmanagedConfig
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}

	content := managedRulesHead + "\n" +
		"You are operating as a private assistant inside agent-bot.\n" +
		"- Never read, print, or expose bot secrets, `.env` files, `auth.json`, SSH keys, or `bot.db`.\n" +
		"- Never suggest `/auth on` or `/search on`; those commands do not exist in agent-bot.\n" +
		"- File writes require user authorization via `/dir add rw /absolute/path` (or saying \"允许写入 /absolute/path\").\n" +
		"- Server shell execution requires `/shell on` (or saying \"开启命令执行\").\n"
	if string(current) == content {
		return nil
	}
	return writeAtomicBytes(path, []byte(content), 0o600)
}

type hookInput struct {
	ToolCall struct {
		Name string          `json:"name"`
		Args json.RawMessage `json:"args"`
	} `json:"toolCall"`
}

type hookOutput struct {
	Decision string `json:"decision"`
	Reason   string `json:"reason,omitempty"`
}

// RunPreToolHook evaluates one Antigravity CLI PreToolUse JSON payload from stdin
// against rawPolicy (from AGENTBOT_AGY_POLICY) and writes the decision JSON to stdout.
func RunPreToolHook(stdin io.Reader, stdout io.Writer, rawPolicy string) error {
	rawIn, err := io.ReadAll(io.LimitReader(stdin, maxHookInputBytes))
	if err != nil {
		return err
	}
	var in hookInput
	if err := json.Unmarshal(rawIn, &in); err != nil {
		out := hookOutput{Decision: "deny", Reason: "invalid PreToolUse payload"}
		return writeHookDecision(stdout, out)
	}

	var policy TurnPolicy
	if strings.TrimSpace(rawPolicy) == "" || json.Unmarshal([]byte(rawPolicy), &policy) != nil {
		out := hookOutput{Decision: "deny", Reason: "missing or invalid agent-bot turn policy"}
		return writeHookDecision(stdout, out)
	}

	out := evaluatePreTool(policy, in.ToolCall.Name, in.ToolCall.Args)
	return writeHookDecision(stdout, out)
}

func writeHookDecision(w io.Writer, out hookOutput) error {
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}

func evaluatePreTool(policy TurnPolicy, toolName string, rawArgs json.RawMessage) hookOutput {
	if policy.JSONSchemaOnly {
		return hookOutput{
			Decision: "deny",
			Reason:   "Tool calls are disabled during structured-output extraction.",
		}
	}

	name := strings.ToLower(strings.TrimSpace(toolName))
	switch name {
	case "run_command", "run_terminal_cmd", "run_terminal_command", "manage_task", "schedule":
		if !policy.ShellTools {
			return hookOutput{
				Decision: "deny",
				Reason:   "Shell command execution is disabled. Ask the user to enable it via /shell on (or say \"开启命令执行\" in chat).",
			}
		}
		return hookOutput{Decision: "auto_approve"}

	case "search_web", "read_url_content", "web_search", "web_fetch":
		if !policy.WebSearch {
			return hookOutput{
				Decision: "deny",
				Reason:   "Web search is disabled in this bot instance (controlled by WEB_SEARCH=true in server config; there is no /search command).",
			}
		}
		return hookOutput{Decision: "auto_approve"}

	case "write_to_file", "replace_file_content", "multi_replace_file_content", "write_file", "edit_file", "write", "search_replace":
		target := extractArgPath(rawArgs, "TargetFile", "target_file", "FilePath", "file_path", "Path", "path")
		if target == "" {
			return hookOutput{
				Decision: "deny",
				Reason:   "Target path is required for file modification.",
			}
		}
		if matchesDenyList(policy.DenyPaths, target) {
			return hookOutput{
				Decision: "deny",
				Reason:   "Access to protected path is denied.",
			}
		}
		allowedDirs := append([]string(nil), policy.WriteDirs...)
		if strings.TrimSpace(policy.MediaDir) != "" {
			allowedDirs = append(allowedDirs, policy.MediaDir)
		}
		if isWithinAnyDir(target, allowedDirs) {
			return hookOutput{Decision: "auto_approve"}
		}
		return hookOutput{
			Decision: "deny",
			Reason:   "File modification outside authorized writable directories is blocked. Never suggest /auth on. Ask the user to authorize an absolute directory via /dir add rw /absolute/path (or say \"允许写入 /absolute/path\" in chat).",
		}

	case "view_file", "read_file", "list_dir", "find_by_name", "grep_search":
		target := extractArgPath(rawArgs, "AbsolutePath", "DirectoryPath", "SearchDirectory", "SearchPath", "Path", "path", "TargetDirectory", "target_directory", "FilePath", "file_path")
		if target != "" && matchesDenyList(policy.DenyPaths, target) {
			return hookOutput{
				Decision: "deny",
				Reason:   "Access to protected path is denied.",
			}
		}
		return hookOutput{Decision: "auto_approve"}

	case "ask_permission":
		return hookOutput{
			Decision: "deny",
			Reason:   "Interactive permission elevation is disabled in agent-bot; ask the user via /dir or /shell.",
		}

	case "list_permissions", "update_focus", "update_steps", "ask_question",
		"call_mcp_tool", "list_resources", "read_resource",
		"generate_image", "image_gen", "image_edit":
		return hookOutput{Decision: "auto_approve"}
	}

	if strings.HasPrefix(name, "mcp_") {
		return hookOutput{Decision: "auto_approve"}
	}
	return hookOutput{Decision: "allow"}
}

func extractArgPath(raw json.RawMessage, keys ...string) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return ""
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	for _, k := range keys {
		for mk, mv := range m {
			if strings.EqualFold(mk, k) {
				if s, ok := mv.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

func resolvePathClean(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		p = abs
	}
	p = filepath.Clean(p)
	if real, err := filepath.EvalSymlinks(p); err == nil && real != "" {
		return real
	}
	// Resolve existing ancestor if target file does not exist yet.
	dir := filepath.Dir(p)
	base := filepath.Base(p)
	if dir != "" && dir != p {
		if realDir, err := filepath.EvalSymlinks(dir); err == nil && realDir != "" {
			return filepath.Join(realDir, base)
		}
	}
	return p
}

func isWithinDir(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	rp := resolvePathClean(path)
	rd := resolvePathClean(dir)
	if runtime.GOOS == "windows" {
		rp = strings.ToLower(rp)
		rd = strings.ToLower(rd)
	}
	rel, err := filepath.Rel(rd, rp)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func isWithinAnyDir(path string, dirs []string) bool {
	for _, d := range dirs {
		if strings.TrimSpace(d) == "" {
			continue
		}
		if isWithinDir(path, d) {
			return true
		}
	}
	return false
}

func matchesDenyList(denyPaths []string, target string) bool {
	cleaned := resolvePathClean(target)
	if cleaned == "" {
		return false
	}
	for _, entry := range denyPaths {
		entry = strings.TrimSpace(entry)
		if entry == "" {
			continue
		}
		if pattern, ok := strings.CutPrefix(entry, "**/"); ok {
			if pathSegmentMatches(strings.ToLower(pattern), cleaned) {
				return true
			}
			continue
		}
		if isWithinDir(cleaned, entry) {
			return true
		}
	}
	return false
}

func pathSegmentMatches(pattern, path string) bool {
	for cur := path; ; {
		if match, err := filepath.Match(pattern, strings.ToLower(filepath.Base(cur))); err != nil || match {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

func writeAtomicBytes(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
