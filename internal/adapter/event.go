// Package adapter normalizes heterogeneous coding-CLI outputs into one
// internal event model and defines the Adapter interface every CLI driver
// implements.
package adapter

import (
	"encoding/json"
	"io"
	"time"

	"github.com/xoralis/cocoder/internal/config"
)

// EventKind enumerates normalized event types emitted by adapters.
type EventKind string

const (
	// EvTaskStarted marks the beginning of a task (emitted by the caller,
	// not adapters — the caller knows the task title).
	EvTaskStarted EventKind = "task_started"
	// EvAgentText is assistant-visible text (whole blocks, not deltas).
	EvAgentText EventKind = "agent_text"
	// EvToolUse is a tool invocation with a one-line summary.
	EvToolUse EventKind = "tool_use"
	// EvFileChanged is a best-effort file modification notice extracted
	// from edit-tool parameters.
	EvFileChanged EventKind = "file_changed"
	// EvStderrLine is one line of child stderr (log/verbose only).
	EvStderrLine EventKind = "stderr"
	// EvResult is the terminal event; exactly one per Run, then the
	// channel closes.
	EvResult EventKind = "result"
)

// TaskStatus is the lifecycle state of a task.
type TaskStatus string

const (
	StatusPending     TaskStatus = "pending"
	StatusRunning     TaskStatus = "running"
	StatusSucceeded   TaskStatus = "succeeded"
	StatusFailed      TaskStatus = "failed"
	StatusInterrupted TaskStatus = "interrupted"
	StatusBlocked     TaskStatus = "blocked"
)

// Event is one normalized occurrence in an agent run.
type Event struct {
	Kind   EventKind   `json:"kind"`
	TaskID string      `json:"task_id"`
	Role   string      `json:"role"`
	Time   time.Time   `json:"time"`
	Text   string      `json:"text,omitempty"` // AgentText / ToolUse summary / stderr line / task title
	Tool   string      `json:"tool,omitempty"`
	Path   string      `json:"path,omitempty"` // FileChanged
	Result *TaskResult `json:"result,omitempty"`
}

// TaskResult is the terminal outcome of one adapter Run.
type TaskResult struct {
	Status    TaskStatus `json:"status"` // succeeded | failed | interrupted
	Summary   string     `json:"summary,omitempty"`
	SessionID string     `json:"session_id,omitempty"`
	CostUSD   float64    `json:"cost_usd,omitempty"` // 0 = unknown (only claude reports reliably)
	Tokens    int        `json:"tokens,omitempty"`
	NumTurns  int        `json:"num_turns,omitempty"`
	ExitCode  int        `json:"exit_code"`
	ErrMsg    string     `json:"err_msg,omitempty"`
	// StructuredOutput carries the schema-conforming JSON object when the
	// run was started with TaskInput.JSONSchema and the CLI honored it.
	StructuredOutput json.RawMessage `json:"structured_output,omitempty"`
}

// TaskInput is everything an adapter needs to run one task.
type TaskInput struct {
	TaskID string
	Role   string
	// Prompt is the fully composed prompt (role briefing already folded in).
	Prompt  string
	WorkDir string
	Model   string // "" = CLI default
	// Permission is mapped to CLI-specific flags by each adapter.
	Permission config.Permission
	// ResumeSessionID, when non-empty, resumes a previous CLI session
	// (Resume is folded into Run rather than a separate method).
	ResumeSessionID string
	// JSONSchema, when non-empty, asks the CLI to constrain its final
	// output to this JSON schema (claude --json-schema, codex
	// --output-schema). Ignored by adapters that cannot enforce it —
	// callers must be ready to fall back to text extraction.
	JSONSchema string
	ExtraArgs  []string
	// RawLog, when non-nil, receives the raw interleaved stdout+stderr of
	// the child process (the per-task log file).
	RawLog io.Writer
}
