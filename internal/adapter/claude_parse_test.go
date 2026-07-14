package adapter

import (
	"os"
	"testing"
)

func collectParse(t *testing.T, fixture string) (events []Event, res TaskResult, saw bool, sid string) {
	t.Helper()
	f, err := os.Open(fixture)
	if err != nil {
		t.Fatalf("open fixture: %v", err)
	}
	defer f.Close()
	res, saw, sid = parseClaudeStream(f, func(e Event) { events = append(events, e) })
	return
}

func TestParseClaudeStreamOK(t *testing.T) {
	events, res, saw, sid := collectParse(t, "testdata/claude_ok.jsonl")

	if !saw {
		t.Fatal("expected a result event")
	}
	if sid != "sess-123" {
		t.Errorf("session id = %q, want sess-123", sid)
	}
	if res.Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded", res.Status)
	}
	if res.Summary != "Created hello.go with a main that prints hello." {
		t.Errorf("summary = %q", res.Summary)
	}
	if res.CostUSD != 0.0123 {
		t.Errorf("cost = %v, want 0.0123", res.CostUSD)
	}
	if res.NumTurns != 3 {
		t.Errorf("num_turns = %v, want 3", res.NumTurns)
	}
	if res.Tokens != 150 {
		t.Errorf("tokens = %v, want 150", res.Tokens)
	}

	var kinds []EventKind
	for _, e := range events {
		kinds = append(kinds, e.Kind)
	}
	want := []EventKind{EvAgentText, EvToolUse, EvFileChanged, EvAgentText}
	if len(kinds) != len(want) {
		t.Fatalf("event kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("event kinds = %v, want %v", kinds, want)
		}
	}
	if events[1].Tool != "Write" || events[1].Text != "file_path=hello.go" {
		t.Errorf("tool_use event = %+v", events[1])
	}
	if events[2].Path != "hello.go" {
		t.Errorf("file_changed path = %q, want hello.go", events[2].Path)
	}
}

func TestParseClaudeStreamError(t *testing.T) {
	events, res, saw, sid := collectParse(t, "testdata/claude_error.jsonl")

	if !saw {
		t.Fatal("expected a result event")
	}
	if sid != "sess-err" {
		t.Errorf("session id = %q, want sess-err", sid)
	}
	if res.Status != StatusFailed {
		t.Errorf("status = %q, want failed", res.Status)
	}
	if res.Summary != "Credit balance is too low" {
		t.Errorf("summary = %q", res.Summary)
	}
	if len(events) != 0 {
		t.Errorf("expected no intermediate events, got %v", events)
	}
}

func TestParseClaudeStreamEmpty(t *testing.T) {
	_, res, saw, sid := collectParse(t, os.DevNull)
	if saw || sid != "" || res.Status != "" {
		t.Errorf("empty stream should produce nothing, got saw=%v sid=%q res=%+v", saw, sid, res)
	}
}
