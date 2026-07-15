package adapter

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

// Grok streaming-json shapes, captured from grok 0.2.93 (2026-07):
//
//	{"type":"thought","data":"<token>"}   reasoning deltas (skipped)
//	{"type":"text","data":"<token>"}      response text deltas
//	{"type":"end","stopReason":"EndTurn","sessionId":"...","requestId":"..."}
//
// Text arrives as token-level deltas, so the parser assembles complete
// lines before emitting EvAgentText. Tool calls are not present in this
// format at all.
type grokLine struct {
	Type       string `json:"type"`
	Data       string `json:"data"`
	StopReason string `json:"stopReason"`
	SessionID  string `json:"sessionId"`
}

func parseGrokStream(r io.Reader, emit func(Event)) (res TaskResult, sawResult bool, sessionID string) {
	br := bufio.NewReaderSize(r, 64*1024)
	var pending string       // partial line being assembled from deltas
	var full strings.Builder // whole response text for the summary
	flush := func(rest bool) {
		for {
			i := strings.IndexByte(pending, '\n')
			if i < 0 {
				break
			}
			if s := strings.TrimRight(pending[:i], "\r"); s != "" {
				emit(Event{Kind: EvAgentText, Text: s})
			}
			pending = pending[i+1:]
		}
		if rest && strings.TrimSpace(pending) != "" {
			emit(Event{Kind: EvAgentText, Text: pending})
			pending = ""
		}
	}
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimSpace(line); s != "" {
			var l grokLine
			if jerr := json.Unmarshal([]byte(s), &l); jerr == nil {
				switch l.Type {
				case "text":
					pending += l.Data
					full.WriteString(l.Data)
					flush(false)
				case "end":
					sawResult = true
					if l.SessionID != "" {
						sessionID = l.SessionID
					}
					if l.StopReason == "" || l.StopReason == "EndTurn" {
						res.Status = StatusSucceeded
					} else {
						res.Status = StatusFailed
						res.ErrMsg = "stopped early: " + l.StopReason
					}
				}
				// "thought" and unknown types are skipped.
			}
		}
		if err != nil {
			break
		}
	}
	flush(true)
	res.Summary = truncateTailRunes(strings.TrimSpace(full.String()), 4000)
	return
}

// truncateTailRunes keeps the last n runes (the conclusion of a reply
// carries the summary).
func truncateTailRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "..." + string(r[len(r)-n:])
}
