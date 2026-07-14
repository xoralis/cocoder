package adapter

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Claude Code stream-json (JSONL) line shapes we care about. Unknown lines
// and unknown fields are tolerated silently so format evolution degrades
// gracefully instead of breaking runs.
type claudeStreamLine struct {
	Type      string         `json:"type"`
	Subtype   string         `json:"subtype"`
	SessionID string         `json:"session_id"`
	Message   *claudeMessage `json:"message"`

	// "result" event fields.
	IsError      bool         `json:"is_error"`
	Result       string       `json:"result"`
	TotalCostUSD float64      `json:"total_cost_usd"`
	NumTurns     int          `json:"num_turns"`
	Usage        *claudeUsage `json:"usage"`
}

type claudeMessage struct {
	Content []claudeContent `json:"content"`
}

type claudeContent struct {
	Type  string          `json:"type"` // text | tool_use
	Text  string          `json:"text"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type claudeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// editTools are Claude Code tools whose input names a file being modified.
var editTools = map[string]bool{
	"Edit":         true,
	"Write":        true,
	"MultiEdit":    true,
	"NotebookEdit": true,
}

// parseClaudeStream is a pure function over the stream-json stdout of
// `claude -p --output-format stream-json --verbose`. Event shapes:
//
//	{"type":"system","subtype":"init","session_id":...}
//	{"type":"assistant","message":{"content":[{"type":"text"|"tool_use",...}]}}
//	{"type":"result","is_error":...,"result":...,"total_cost_usd":...,...}
func parseClaudeStream(r io.Reader, emit func(Event)) (res TaskResult, sawResult bool, sessionID string) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			handleClaudeLine(s, emit, &res, &sawResult, &sessionID)
		}
		if err != nil {
			return
		}
	}
}

func handleClaudeLine(s string, emit func(Event), res *TaskResult, saw *bool, sid *string) {
	var l claudeStreamLine
	if err := json.Unmarshal([]byte(s), &l); err != nil {
		return // tolerate non-JSON noise
	}
	switch l.Type {
	case "system":
		if l.Subtype == "init" && l.SessionID != "" {
			*sid = l.SessionID
		}
	case "assistant":
		if l.Message == nil {
			return
		}
		for _, c := range l.Message.Content {
			switch c.Type {
			case "text":
				if c.Text != "" {
					emit(Event{Kind: EvAgentText, Text: c.Text})
				}
			case "tool_use":
				emit(Event{Kind: EvToolUse, Tool: c.Name, Text: summarizeToolInput(c.Input)})
				if editTools[c.Name] {
					if fp := fileFromToolInput(c.Input); fp != "" {
						emit(Event{Kind: EvFileChanged, Path: fp})
					}
				}
			}
		}
	case "result":
		*saw = true
		if l.SessionID != "" {
			*sid = l.SessionID
		}
		res.Summary = l.Result
		res.CostUSD = l.TotalCostUSD
		res.NumTurns = l.NumTurns
		if l.Usage != nil {
			res.Tokens = l.Usage.InputTokens + l.Usage.OutputTokens
		}
		if l.IsError {
			res.Status = StatusFailed
		} else {
			res.Status = StatusSucceeded
		}
	}
}

// summaryKeys are tried in order to render a one-line tool-input summary.
var summaryKeys = []string{"file_path", "notebook_path", "path", "command", "pattern", "query", "url", "prompt", "description"}

func summarizeToolInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return truncate(string(raw), 100)
	}
	for _, k := range summaryKeys {
		if v, ok := m[k]; ok {
			if s, ok := v.(string); ok && s != "" {
				return truncate(fmt.Sprintf("%s=%s", k, firstLine(s)), 120)
			}
		}
	}
	return truncate(string(raw), 100)
}

func fileFromToolInput(raw json.RawMessage) string {
	var m struct {
		FilePath     string `json:"file_path"`
		NotebookPath string `json:"notebook_path"`
		Path         string `json:"path"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return ""
	}
	switch {
	case m.FilePath != "":
		return m.FilePath
	case m.NotebookPath != "":
		return m.NotebookPath
	default:
		return m.Path
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
