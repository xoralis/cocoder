package adapter

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Codex `--json` JSONL shapes, captured from codex-cli 0.144 (2026-07):
//
//	{"type":"thread.started","thread_id":"019f..."}
//	{"type":"turn.started"}
//	{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"...","status":"in_progress"}}
//	{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"..."}}
//	{"type":"item.completed","item":{"id":"item_1","type":"file_change","changes":[{"path":"...","kind":"add"}],"status":"completed"}}
//	{"type":"turn.completed","usage":{"input_tokens":..,"cached_input_tokens":..,"output_tokens":..,"reasoning_output_tokens":..}}
//	{"type":"turn.failed","error":{"message":"..."}}
//	{"type":"error","message":"..."}
type codexLine struct {
	Type     string      `json:"type"`
	ThreadID string      `json:"thread_id"`
	Item     *codexItem  `json:"item"`
	Usage    *codexUsage `json:"usage"`
	Error    *codexError `json:"error"`
	Message  string      `json:"message"` // top-level "error" events
}

type codexItem struct {
	Type     string        `json:"type"` // agent_message | reasoning | command_execution | file_change | mcp_tool_call | web_search | todo_list
	Text     string        `json:"text"`
	Command  string        `json:"command"`
	Status   string        `json:"status"`
	ExitCode *int          `json:"exit_code"`
	Changes  []codexChange `json:"changes"`
}

type codexChange struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
	ReasoningOutputTokens int `json:"reasoning_output_tokens"`
}

type codexError struct {
	Message string `json:"message"`
}

// parseCodexStream normalizes the codex exec --json stream. The last
// agent_message doubles as the result summary (codex has no separate
// result payload), and it becomes StructuredOutput when it parses as a
// JSON object (--output-schema runs).
func parseCodexStream(r io.Reader, emit func(Event)) (res TaskResult, sawResult bool, sessionID string) {
	br := bufio.NewReaderSize(r, 64*1024)
	var lastMsg, lastErr string
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			handleCodexLine(s, emit, &res, &sawResult, &sessionID, &lastMsg, &lastErr)
		}
		if err != nil {
			break
		}
	}
	if res.Summary == "" {
		res.Summary = lastMsg
	}
	if res.ErrMsg == "" && res.Status == StatusFailed {
		res.ErrMsg = lastErr
	}
	return
}

func handleCodexLine(s string, emit func(Event), res *TaskResult, saw *bool, sid *string, lastMsg, lastErr *string) {
	var l codexLine
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return // tolerate non-JSON noise
	}
	switch l.Type {
	case "thread.started":
		if l.ThreadID != "" {
			*sid = l.ThreadID
		}
	case "item.started":
		if l.Item == nil {
			return
		}
		switch l.Item.Type {
		case "command_execution":
			emit(Event{Kind: EvToolUse, Tool: "exec", Text: truncate(firstLine(l.Item.Command), 120)})
		case "mcp_tool_call", "web_search":
			emit(Event{Kind: EvToolUse, Tool: l.Item.Type, Text: truncate(l.Item.Text, 120)})
		}
	case "item.completed":
		if l.Item == nil {
			return
		}
		switch l.Item.Type {
		case "agent_message":
			if l.Item.Text != "" {
				emit(Event{Kind: EvAgentText, Text: l.Item.Text})
				*lastMsg = l.Item.Text
			}
		case "file_change":
			for _, c := range l.Item.Changes {
				emit(Event{Kind: EvFileChanged, Path: c.Path})
			}
		}
	case "turn.completed":
		*saw = true
		res.Status = StatusSucceeded
		res.Summary = *lastMsg
		if l.Usage != nil {
			res.Tokens = l.Usage.InputTokens + l.Usage.OutputTokens
		}
		if so := strings.TrimSpace(*lastMsg); strings.HasPrefix(so, "{") && json.Valid([]byte(so)) {
			res.StructuredOutput = json.RawMessage(so)
		}
	case "turn.failed":
		*saw = true
		res.Status = StatusFailed
		if l.Error != nil {
			res.ErrMsg = truncate(l.Error.Message, 1000)
		}
	case "error":
		if l.Message != "" {
			*lastErr = truncate(l.Message, 1000)
			emit(Event{Kind: EvStderrLine, Text: truncate(l.Message, 300)})
		}
	}
}
