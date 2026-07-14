package plan

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ExtractJSON pulls the plan JSON out of noisy LLM output.
// Preference order: (1) the last ```json fenced block; (2) the last
// balanced top-level {...} object that mentions "tasks". Returns the JSON
// string and the text before it (used as the architecture document).
func ExtractJSON(text string) (jsonStr, before string, err error) {
	if j, b, ok := lastJSONFence(text); ok {
		return j, b, nil
	}
	if j, b, ok := lastBalancedObject(text); ok {
		return j, b, nil
	}
	return "", "", fmt.Errorf("no JSON plan found in the reply: expected a ```json fenced block (or a bare JSON object) containing a \"tasks\" array")
}

// Parse unmarshals a plan JSON string with an LLM-friendly error message.
func Parse(jsonStr string) (*Plan, error) {
	var p Plan
	if err := json.Unmarshal([]byte(jsonStr), &p); err != nil {
		return nil, fmt.Errorf("the plan JSON failed to parse: %v", err)
	}
	return &p, nil
}

func lastJSONFence(text string) (string, string, bool) {
	const open = "```json"
	idx := strings.LastIndex(text, open)
	if idx < 0 {
		return "", "", false
	}
	rest := text[idx+len(open):]
	nl := strings.IndexByte(rest, '\n')
	if nl < 0 {
		return "", "", false
	}
	body := rest[nl+1:]
	end := strings.Index(body, "```")
	if end < 0 {
		return "", "", false // unterminated fence; try the bracket scanner
	}
	return strings.TrimSpace(body[:end]), strings.TrimSpace(text[:idx]), true
}

// lastBalancedObject scans for top-level balanced {...} objects
// (string-aware) and returns the last one containing "tasks". Best effort:
// unbalanced braces in surrounding prose can defeat it, in which case the
// planner retry loop takes over.
func lastBalancedObject(text string) (string, string, bool) {
	bestStart, bestEnd := -1, -1
	depth, start := 0, -1
	inStr, esc := false, false
	for i, r := range text {
		if esc {
			esc = false
			continue
		}
		switch {
		case inStr:
			if r == '\\' {
				esc = true
			} else if r == '"' {
				inStr = false
			}
		case r == '"':
			if depth > 0 {
				inStr = true
			}
		case r == '{':
			if depth == 0 {
				start = i
			}
			depth++
		case r == '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					cand := text[start : i+1]
					if strings.Contains(cand, `"tasks"`) {
						bestStart, bestEnd = start, i+1
					}
					start = -1
				}
			}
		}
	}
	if bestStart < 0 {
		return "", "", false
	}
	return text[bestStart:bestEnd], strings.TrimSpace(text[:bestStart]), true
}
