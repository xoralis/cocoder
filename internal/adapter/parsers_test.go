package adapter

import (
	"os"
	"strings"
	"testing"
)

func runParser(t *testing.T, p streamParser, fixture string) ([]Event, TaskResult, bool, string) {
	t.Helper()
	f, err := os.Open(fixture)
	if err != nil {
		t.Fatalf("open %s: %v", fixture, err)
	}
	defer f.Close()
	var events []Event
	res, saw, sid := p(f, func(e Event) { events = append(events, e) })
	return events, res, saw, sid
}

func kindsOf(events []Event) []EventKind {
	var ks []EventKind
	for _, e := range events {
		ks = append(ks, e.Kind)
	}
	return ks
}

func countKind(events []Event, k EventKind) int {
	n := 0
	for _, e := range events {
		if e.Kind == k {
			n++
		}
	}
	return n
}

func TestParseCodexStreamOK(t *testing.T) {
	events, res, saw, sid := runParser(t, parseCodexStream, "testdata/codex_ok.jsonl")
	if !saw || res.Status != StatusSucceeded {
		t.Fatalf("status=%q saw=%v", res.Status, saw)
	}
	if sid != "019f61d7-1dfb-77e2-a5dd-9d1dbded16aa" {
		t.Errorf("session=%q", sid)
	}
	if res.Summary != "DONE" {
		t.Errorf("summary=%q, want DONE (last agent_message)", res.Summary)
	}
	if res.Tokens != 27341+80 {
		t.Errorf("tokens=%d", res.Tokens)
	}
	if got := countKind(events, EvFileChanged); got != 1 {
		t.Errorf("file_changed count=%d, want 1", got)
	}
	if got := countKind(events, EvToolUse); got != 1 { // command_execution
		t.Errorf("tool_use count=%d, want 1", got)
	}
	for _, e := range events {
		if e.Kind == EvFileChanged && e.Path != `C:\demo\codex_probe.txt` {
			t.Errorf("file path=%q", e.Path)
		}
	}
}

func TestParseCodexStreamFailed(t *testing.T) {
	_, res, saw, _ := runParser(t, parseCodexStream, "testdata/codex_failed.jsonl")
	if !saw || res.Status != StatusFailed {
		t.Fatalf("status=%q saw=%v", res.Status, saw)
	}
	if res.ErrMsg == "" {
		t.Error("expected an error message")
	}
}

func TestParseCodexStructuredOutput(t *testing.T) {
	// A final agent_message that is a JSON object becomes StructuredOutput.
	const stream = `{"type":"thread.started","thread_id":"t"}
{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"{\"goal\":\"g\",\"tasks\":[]}"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`
	var events []Event
	res, saw, _ := parseCodexStream(strings.NewReader(stream), func(e Event) { events = append(events, e) })
	if !saw || len(res.StructuredOutput) == 0 {
		t.Fatalf("structured output not captured: %q", res.StructuredOutput)
	}
	_ = events
}

func TestParseGrokStreamOK(t *testing.T) {
	events, res, saw, sid := runParser(t, parseGrokStream, "testdata/grok_ok.jsonl")
	if !saw || res.Status != StatusSucceeded {
		t.Fatalf("status=%q saw=%v", res.Status, saw)
	}
	if sid != "019f61d2-12ad-77a0-a4cc-014918ed5aa4" {
		t.Errorf("session=%q", sid)
	}
	// Token deltas assembled into lines: "Created `grok_probe.txt` with the
	// content `banana`." then "Second line here."
	texts := 0
	for _, e := range events {
		if e.Kind == EvAgentText {
			texts++
		}
	}
	if texts == 0 {
		t.Error("no agent text emitted")
	}
	if events[0].Kind != EvAgentText || events[0].Text == "" {
		t.Errorf("first event=%+v", events[0])
	}
	if res.Summary == "" || !strings.Contains(res.Summary, "banana") {
		t.Errorf("summary=%q", res.Summary)
	}
	// thoughts must be skipped.
	for _, e := range events {
		if e.Kind == EvAgentText && strings.Contains(e.Text, "user wants a file") {
			t.Errorf("thought leaked into agent text: %q", e.Text)
		}
	}
}

func TestParseGeminiStreamOK(t *testing.T) {
	events, res, saw, sid := runParser(t, parseGeminiStream, "testdata/gemini_ok.jsonl")
	if !saw || res.Status != StatusSucceeded {
		t.Fatalf("status=%q saw=%v", res.Status, saw)
	}
	if sid != "g-sess-1" {
		t.Errorf("session=%q", sid)
	}
	if res.Summary != "Wrote docs/x.md with usage instructions." {
		t.Errorf("summary=%q", res.Summary)
	}
	if res.Tokens != 123 {
		t.Errorf("tokens=%d", res.Tokens)
	}
	if countKind(events, EvToolUse) != 1 {
		t.Errorf("tool_use kinds=%v", kindsOf(events))
	}
}

func TestParseTextStream(t *testing.T) {
	const out = "line one\nline two\n\nline three\n"
	var events []Event
	res, saw, sid := parseTextStream(strings.NewReader(out), func(e Event) { events = append(events, e) })
	if saw {
		t.Error("text parser must not claim a result (exit code decides)")
	}
	if sid != "" {
		t.Errorf("session=%q", sid)
	}
	if countKind(events, EvAgentText) != 3 {
		t.Errorf("agent text lines=%d, want 3", countKind(events, EvAgentText))
	}
	if !strings.Contains(res.Summary, "line three") {
		t.Errorf("summary=%q", res.Summary)
	}
}
