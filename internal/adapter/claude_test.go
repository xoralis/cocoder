package adapter

import (
	"bytes"
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

func claudeSpec() *config.CLISpec {
	return &config.CLISpec{Name: "claude", Adapter: "claude", Command: "claude", Output: "jsonl", PromptVia: "stdin"}
}

func drain(ch <-chan Event) (events []Event, final *TaskResult) {
	for e := range ch {
		events = append(events, e)
		if e.Kind == EvResult {
			final = e.Result
		}
	}
	return
}

func TestClaudeRunHappyPath(t *testing.T) {
	fixture, err := os.ReadFile("testdata/claude_ok.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: string(fixture), Stderr: "note: something on stderr\n", Exit: 0},
	}}
	a := NewClaude(claudeSpec(), fr)

	var raw bytes.Buffer
	ch, err := a.Run(context.Background(), TaskInput{
		TaskID: "t1", Role: "backend", Prompt: "do the thing",
		Permission: config.PermEdits, RawLog: &raw,
	})
	if err != nil {
		t.Fatal(err)
	}
	events, final := drain(ch)

	if final == nil {
		t.Fatal("no result event")
	}
	if final.Status != StatusSucceeded {
		t.Errorf("status = %q, want succeeded (err=%q)", final.Status, final.ErrMsg)
	}
	if final.SessionID != "sess-123" {
		t.Errorf("session = %q, want sess-123", final.SessionID)
	}
	if final.CostUSD != 0.0123 {
		t.Errorf("cost = %v", final.CostUSD)
	}
	if final.ExitCode != 0 {
		t.Errorf("exit = %d", final.ExitCode)
	}

	// Last event must be the result; every event stamped with task/role.
	if events[len(events)-1].Kind != EvResult {
		t.Errorf("last event = %v, want result", events[len(events)-1].Kind)
	}
	for _, e := range events {
		if e.TaskID != "t1" || e.Role != "backend" {
			t.Errorf("event not stamped: %+v", e)
		}
	}

	// Spawn spec: prompt via stdin, headless flags, permission mapping.
	call := fr.Calls[0]
	if call.Stdin != "do the thing" {
		t.Errorf("stdin = %q", call.Stdin)
	}
	for _, want := range []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "acceptEdits"} {
		if !slices.Contains(call.Args, want) {
			t.Errorf("args missing %q: %v", want, call.Args)
		}
	}
	if slices.Contains(call.Args, "do the thing") {
		t.Errorf("prompt leaked into argv: %v", call.Args)
	}

	// Raw log received both stdout and stderr.
	if !bytes.Contains(raw.Bytes(), []byte("sess-123")) {
		t.Error("raw log missing stdout")
	}
	if !bytes.Contains(raw.Bytes(), []byte("something on stderr")) {
		t.Error("raw log missing stderr")
	}
}

func TestClaudeRunPermissionAndResumeArgs(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Exit: 0}}}
	a := NewClaude(claudeSpec(), fr)
	ch, err := a.Run(context.Background(), TaskInput{
		TaskID: "t1", Role: "r", Prompt: "p",
		Permission: config.PermFull, ResumeSessionID: "old-sess", Model: "opus",
	})
	if err != nil {
		t.Fatal(err)
	}
	drain(ch)
	args := fr.Calls[0].Args
	for _, want := range []string{"--dangerously-skip-permissions", "--resume", "old-sess", "--model", "opus"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q: %v", want, args)
		}
	}
}

func TestClaudeReadOnlyIsHardLocked(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Exit: 0}}}
	a := NewClaude(claudeSpec(), fr)
	ch, _ := a.Run(context.Background(), TaskInput{TaskID: "t", Role: "r", Prompt: "p", Permission: config.PermReadOnly})
	drain(ch)
	args := fr.Calls[0].Args
	// dontAsk must be explicit so a permissive user-global defaultMode
	// (bypassPermissions) cannot leak write access into read-only roles.
	for _, want := range []string{"--permission-mode", "dontAsk", "--allowedTools", "--disallowedTools"} {
		if !slices.Contains(args, want) {
			t.Errorf("read-only args missing %q: %v", want, args)
		}
	}
}

func TestClaudeRunPermissionOverrideFromConfig(t *testing.T) {
	spec := claudeSpec()
	spec.PermissionArgs = map[string][]string{"edits": {"--custom-flag"}}
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Exit: 0}}}
	a := NewClaude(spec, fr)
	ch, _ := a.Run(context.Background(), TaskInput{TaskID: "t", Role: "r", Prompt: "p", Permission: config.PermEdits})
	drain(ch)
	args := fr.Calls[0].Args
	if !slices.Contains(args, "--custom-flag") || slices.Contains(args, "acceptEdits") {
		t.Errorf("permission override not applied: %v", args)
	}
}

func TestClaudeRunNonZeroExitWithoutResult(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: "", Stderr: "boom: not logged in\n", Exit: 2},
	}}
	a := NewClaude(claudeSpec(), fr)
	ch, _ := a.Run(context.Background(), TaskInput{TaskID: "t1", Role: "r", Prompt: "p", Permission: config.PermEdits})
	_, final := drain(ch)
	if final.Status != StatusFailed {
		t.Errorf("status = %q, want failed", final.Status)
	}
	if final.ExitCode != 2 {
		t.Errorf("exit = %d, want 2", final.ExitCode)
	}
	if want := "boom: not logged in"; !bytes.Contains([]byte(final.ErrMsg), []byte(want)) {
		t.Errorf("errmsg = %q, want to contain %q", final.ErrMsg, want)
	}
}

func TestClaudeRunCancelled(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: "", BlockUntilKilled: true},
	}}
	a := NewClaude(claudeSpec(), fr)
	ctx, cancel := context.WithCancel(context.Background())
	ch, err := a.Run(ctx, TaskInput{TaskID: "t1", Role: "r", Prompt: "p", Permission: config.PermEdits})
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	_, final := drain(ch)
	if final == nil {
		t.Fatal("no result event")
	}
	if final.Status != StatusInterrupted {
		t.Errorf("status = %q, want interrupted", final.Status)
	}
}

func TestClaudeRunTimeout(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: "", BlockUntilKilled: true},
	}}
	a := NewClaude(claudeSpec(), fr)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	ch, err := a.Run(ctx, TaskInput{TaskID: "t1", Role: "r", Prompt: "p", Permission: config.PermEdits})
	if err != nil {
		t.Fatal(err)
	}
	_, final := drain(ch)
	if final.Status != StatusFailed {
		t.Errorf("status = %q, want failed (timeout)", final.Status)
	}
	if final.ErrMsg != "task timed out" {
		t.Errorf("errmsg = %q", final.ErrMsg)
	}
}
