// Package engine defines the unified headless AI CLI interface, shared turn
// types, and polymorphic MCP / self-upgrade contracts implemented by each
// backend driver (such as Grok CLI and Codex CLI).
package engine

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode"

	"github.com/rshun/agent-bot/internal/store"
)

// ReservedMCPName is the internal per-turn MCP server name managed by agent-bot.
const ReservedMCPName = "agentbot"

// IsReservedMCPName reports whether name refers to an internal bot-managed MCP channel.
func IsReservedMCPName(name string) bool {
	n := strings.TrimSpace(name)
	return strings.EqualFold(n, ReservedMCPName) ||
		strings.EqualFold(n, "grokbot") ||
		strings.EqualFold(n, "codexbot")
}

// Engine is the core AI subscription CLI surface used by the chat service.
type Engine interface {
	Reply(ctx context.Context, opts ReplyOptions) (HeadlessResult, error)
	Models(ctx context.Context) (ModelList, error)
	Allowance(ctx context.Context) (Allowance, error)
}

// NamedEngine is optionally implemented by Engine drivers to identify themselves.
type NamedEngine interface {
	EngineName() string
	DisplayName() string
}

// MCPEngine is an optional extension implemented by CLI drivers that support
// MCP server management and version inspection.
type MCPEngine interface {
	Version(ctx context.Context) (string, error)
	ResetLeader(ctx context.Context)
	MCPList(ctx context.Context) (string, error)
	MCPDoctor(ctx context.Context, name string) (string, error)
	MCPAdd(ctx context.Context, rawArgs []string) (string, error)
	MCPRemove(ctx context.Context, name string) (string, error)
	MCPEnable(ctx context.Context, name string, enable bool) (string, error)
}

// CLIStatus describes the current version, remote update availability, and
// write permission state of the underlying AI CLI binary/package.
type CLIStatus struct {
	Current         string
	Latest          string
	UpdateAvailable bool
	// DirectUpdateReady is true when the CLI has a built-in self-updater (e.g. `grok update`)
	// whose remote latest version cannot be queried without running the update itself.
	DirectUpdateReady bool
	Path              string
	Writable          bool
	WriteHint         string
	Error             string
}

// CLIUpgradeResult describes the outcome of upgrading the underlying AI CLI.
type CLIUpgradeResult struct {
	Changed    bool
	OldVersion string
	NewVersion string
	TargetPath string
	Output     string
}

// CLIUpgrader is an optional extension implemented by Engine drivers that support
// privilege-free in-chat and scheduled CLI updates.
type CLIUpgrader interface {
	CheckCLIUpgrade(ctx context.Context) CLIStatus
	UpgradeCLI(ctx context.Context) (CLIUpgradeResult, error)
}

// Image represents an inbound user picture passed to multimodal turns.
type Image struct {
	MIME string
	Data []byte
}

// ToolUse represents one tool call streamed by the CLI during a turn.
type ToolUse struct {
	Name  string
	Input json.RawMessage
}

// ReplyOptions controls a single headless chat or structured-output turn.
type ReplyOptions struct {
	SessionID     string
	NewSession    bool
	Model         string
	Prompt        string
	WebSearch     bool
	Sandbox       string
	FileTools     bool
	WriteDirs     []string
	ShellTools    bool
	JSONSchema    string
	OnTool        func(ToolUse)
	Env           []string
	Actions       bool
	React         bool
	SendFile      bool
	Pronounce     bool
	ImageGen      bool
	SendWebImage  bool
	Images        []Image
	Memories      []string
	ScheduledTask bool
	MaxTurns      int
	OnSession     func(string)
}

// HeadlessResult is the normalized output from one CLI turn.
type HeadlessResult struct {
	Text            string
	SessionID       string
	StopReason      string
	Structured      json.RawMessage
	GeneratedImages []string
	Usage           *store.UsageTokens
}

// ModelList holds the discovered model catalog.
type ModelList struct {
	Default   string
	Current   string
	Names     []string
	Available []string
}

// ProductUsage is one product row inside a Grok billing payload.
type ProductUsage struct {
	Name         string
	UsagePercent float64
}

// QuotaWindow holds one rate-limit window (e.g., primary 5h or secondary 7d).
type QuotaWindow struct {
	UsedPercent        int
	RemainingPercent   int
	WindowDurationMins int
	ResetsAt           time.Time
}

// QuotaBucket holds the rate-limit windows for a specific limit ID.
type QuotaBucket struct {
	LimitID   string
	LimitName string
	PlanType  string
	Primary   *QuotaWindow
	Secondary *QuotaWindow
}

// Allowance holds account subscription quota / rate-limit state across engines.
type Allowance struct {
	Tier           string
	PlanType       string
	AccountEmail   string
	UsedPercent    float64
	HasUsedPercent bool
	PeriodKind     string
	PeriodStart    time.Time
	PeriodEnd      time.Time
	Products       []ProductUsage
	Buckets        []QuotaBucket
	Truncated      bool
	QueriedAt      time.Time
}

// RunError represents an error reported by the underlying AI CLI.
type RunError struct {
	Message     string
	SessionID   string
	PartialText string
}

func (e *RunError) Error() string {
	return e.Message
}

// ValidModelName reports whether a user-supplied model ID is safe to pass as a CLI argument.
func ValidModelName(name string) bool {
	if name == "" || len(name) > 64 || strings.HasPrefix(name, "-") {
		return false
	}
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '.' || r == '_' || r == '+' || r == ':' {
			continue
		}
		return false
	}
	return true
}

// IsMaxTurns reports whether err indicates the CLI hit its max-turns cap.
func IsMaxTurns(err error) bool {
	if err == nil {
		return false
	}
	lower := strings.ToLower(err.Error())
	return strings.Contains(lower, "maximum number of turns") ||
		strings.Contains(lower, "maximum turns") ||
		strings.Contains(lower, "max turns") ||
		strings.Contains(lower, "max_turns") ||
		strings.Contains(lower, "error_max_turns")
}
