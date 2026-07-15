package adapter

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Gemini stream-json shapes per the 2026-07 docs (live-unverified; parser
// is deliberately tolerant of alternative field names):
//
//	{"type":"init","session_id":"..."}                       session start
//	{"type":"message","content":"..."}                       assistant text
//	{"type":"tool_use","name":"...", ...}                    tool call
//	{"type":"tool_result", ...}                              (skipped)
//	{"type":"error","message":"..."}
//	{"type":"result","response":"...","stats":{...}}         terminal event
type geminiLine struct {
	Type       string          `json:"type"`
	SessionID  string          `json:"session_id"`
	SessionID2 string          `json:"sessionId"`
	Content    string          `json:"content"`
	Text       string          `json:"text"`
	Data       string          `json:"data"`
	Name       string          `json:"name"`
	ToolName   string          `json:"tool_name"`
	Message    string          `json:"message"`
	Response   string          `json:"response"`
	Error      json.RawMessage `json:"error"`
	Stats      *geminiStats    `json:"stats"`
}

type geminiStats struct {
	TotalTokens  int `json:"total_tokens"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func parseGeminiStream(r io.Reader, emit func(Event)) (res TaskResult, sawResult bool, sessionID string) {
	br := bufio.NewReaderSize(r, 64*1024)
	var lastErr string
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			var l geminiLine
			if jerr := json.Unmarshal([]byte(s), &l); jerr == nil {
				handleGeminiLine(&l, emit, &res, &sawResult, &sessionID, &lastErr)
			}
		}
		if err != nil {
			break
		}
	}
	if res.Status == StatusFailed && res.ErrMsg == "" {
		res.ErrMsg = lastErr
	}
	return
}

func handleGeminiLine(l *geminiLine, emit func(Event), res *TaskResult, saw *bool, sid *string, lastErr *string) {
	session := l.SessionID
	if session == "" {
		session = l.SessionID2
	}
	switch l.Type {
	case "init":
		if session != "" {
			*sid = session
		}
	case "message", "assistant":
		if t := firstNonEmpty(l.Content, l.Text, l.Data); t != "" {
			emit(Event{Kind: EvAgentText, Text: t})
		}
	case "tool_use", "tool_call":
		name := firstNonEmpty(l.Name, l.ToolName)
		emit(Event{Kind: EvToolUse, Tool: name})
	case "error":
		msg := l.Message
		if msg == "" && len(l.Error) > 0 {
			msg = string(l.Error)
		}
		if msg != "" {
			*lastErr = truncate(msg, 1000)
			emit(Event{Kind: EvStderrLine, Text: truncate(msg, 300)})
		}
	case "result":
		*saw = true
		if session != "" {
			*sid = session
		}
		res.Summary = l.Response
		if l.Stats != nil {
			if l.Stats.TotalTokens > 0 {
				res.Tokens = l.Stats.TotalTokens
			} else {
				res.Tokens = l.Stats.InputTokens + l.Stats.OutputTokens
			}
		}
		if len(l.Error) > 0 && string(l.Error) != "null" {
			res.Status = StatusFailed
			res.ErrMsg = truncate(string(l.Error), 1000)
		} else {
			res.Status = StatusSucceeded
		}
	}
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
