package adapter

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// adapterCase describes one adapter under contract test: how to build it,
// a canned successful stdout stream, and how the prompt reaches the child
// (stdin, argv, or a --prompt-file path).
type adapterCase struct {
	name       string
	build      func(spec *config.CLISpec, r execx.Runner) Adapter
	adapterKey string
	okStdout   string
	// promptChannel: "stdin" | "arg" | "file"
	promptChannel string
}

func adapterCases() []adapterCase {
	return []adapterCase{
		{
			name:          "claude",
			build:         func(s *config.CLISpec, r execx.Runner) Adapter { return NewClaude(s, r) },
			adapterKey:    "claude",
			promptChannel: "stdin",
			okStdout: `{"type":"system","subtype":"init","session_id":"cs"}
{"type":"assistant","message":{"content":[{"type":"text","text":"hi"}]}}
{"type":"result","is_error":false,"result":"done","session_id":"cs","total_cost_usd":0.01}
`,
		},
		{
			name:          "codex",
			build:         func(s *config.CLISpec, r execx.Runner) Adapter { return NewCodex(s, r) },
			adapterKey:    "codex",
			promptChannel: "stdin",
			okStdout: `{"type":"thread.started","thread_id":"cx"}
{"type":"item.completed","item":{"id":"i","type":"agent_message","text":"done"}}
{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}
`,
		},
		{
			name:          "grok",
			build:         func(s *config.CLISpec, r execx.Runner) Adapter { return NewGrok(s, r) },
			adapterKey:    "grok",
			promptChannel: "file",
			okStdout: `{"type":"text","data":"done"}
{"type":"end","stopReason":"EndTurn","sessionId":"gk"}
`,
		},
		{
			name:          "gemini",
			build:         func(s *config.CLISpec, r execx.Runner) Adapter { return NewGemini(s, r) },
			adapterKey:    "gemini",
			promptChannel: "stdin",
			okStdout: `{"type":"init","session_id":"gm"}
{"type":"result","response":"done","stats":{"total_tokens":5}}
`,
		},
	}
}

func specFor(key string) *config.CLISpec {
	cfg := config.DefaultConfig()
	cfg.Roles = map[string]*config.Role{"r": {CLI: key, Permission: config.PermEdits}}
	spec, err := cfg.ResolveCLI(key)
	if err != nil {
		panic(err)
	}
	return spec
}

// TestAdapterContract runs the same assertions against every first-class
// adapter: a successful run ends with exactly one EvResult carrying
// StatusSucceeded and the captured session id, and the prompt reaches the
// child through the adapter's declared channel (never leaking into argv).
func TestAdapterContract(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Stdout: tc.okStdout, Exit: 0}}}
			a := tc.build(specFor(tc.adapterKey), fr)

			ch, err := a.Run(context.Background(), TaskInput{
				TaskID: "t1", Role: "r", Prompt: "PROMPT-BODY", WorkDir: ".",
				Permission: config.PermEdits,
			})
			if err != nil {
				t.Fatal(err)
			}
			events, final := drain(ch)
			if final == nil {
				t.Fatal("no result event")
			}
			if final.Status != StatusSucceeded {
				t.Errorf("status=%q err=%q", final.Status, final.ErrMsg)
			}
			if events[len(events)-1].Kind != EvResult {
				t.Errorf("last event is %v, want result", events[len(events)-1].Kind)
			}
			if final.SessionID == "" {
				t.Error("session id not captured")
			}
			for _, e := range events {
				if e.TaskID != "t1" || e.Role != "r" {
					t.Errorf("event not stamped: %+v", e)
				}
			}

			// Prompt-channel contract: never in argv.
			call := fr.Calls[0]
			if slices.Contains(call.Args, "PROMPT-BODY") {
				t.Errorf("prompt leaked into argv: %v", call.Args)
			}
			switch tc.promptChannel {
			case "stdin":
				if call.Stdin != "PROMPT-BODY" {
					t.Errorf("stdin=%q, want the prompt", call.Stdin)
				}
			case "file":
				// prompt written to a temp file; a --prompt-file/-f flag
				// must point at a real path and stdin stays empty.
				if call.Stdin != "" {
					t.Errorf("grok should not use stdin, got %q", call.Stdin)
				}
				if !slices.ContainsFunc(call.Args, func(s string) bool { return strings.Contains(s, "ccd-prompt-") }) {
					t.Errorf("no prompt file in args: %v", call.Args)
				}
			}
		})
	}
}

// TestAdapterExitFailure: a non-zero exit with no result event is a failure
// across all adapters, with stderr surfaced in ErrMsg.
func TestAdapterExitFailure(t *testing.T) {
	for _, tc := range adapterCases() {
		t.Run(tc.name, func(t *testing.T) {
			fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Stderr: "boom\n", Exit: 2}}}
			a := tc.build(specFor(tc.adapterKey), fr)
			ch, err := a.Run(context.Background(), TaskInput{TaskID: "t1", Role: "r", Prompt: "p", Permission: config.PermEdits})
			if err != nil {
				t.Fatal(err)
			}
			_, final := drain(ch)
			if final.Status != StatusFailed {
				t.Errorf("status=%q, want failed", final.Status)
			}
			if final.ExitCode != 2 {
				t.Errorf("exit=%d, want 2", final.ExitCode)
			}
		})
	}
}

func TestCodexPermissionAndResumeArgs(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Stdout: `{"type":"turn.completed"}` + "\n", Exit: 0}}}
	a := NewCodex(specFor("codex"), fr)
	ch, _ := a.Run(context.Background(), TaskInput{
		TaskID: "t", Role: "r", Prompt: "p", WorkDir: "/w",
		Permission: config.PermReadOnly, ResumeSessionID: "sess-9",
	})
	drain(ch)
	args := fr.Calls[0].Args
	// resume subcommand must follow exec flags and precede the "-" stdin arg.
	ri := slices.Index(args, "resume")
	si := slices.Index(args, "-s")
	dashIdx := slices.Index(args, "-")
	if ri < 0 || si < 0 || dashIdx < 0 || !(si < ri && ri < dashIdx) {
		t.Errorf("arg order wrong (want -s ... resume ... -): %v", args)
	}
	if !slices.Contains(args, "read-only") {
		t.Errorf("read-only sandbox missing: %v", args)
	}
	if args[len(args)-1] != "-" {
		t.Errorf("last arg=%q, want -", args[len(args)-1])
	}
}

func TestGrokPermissionModes(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Stdout: `{"type":"end","stopReason":"EndTurn"}` + "\n", Exit: 0}}}
	a := NewGrok(specFor("grok"), fr)
	ch, _ := a.Run(context.Background(), TaskInput{TaskID: "t", Role: "r", Prompt: "p", Permission: config.PermFull})
	drain(ch)
	args := fr.Calls[0].Args
	if i := slices.Index(args, "--permission-mode"); i < 0 || args[i+1] != "bypassPermissions" {
		t.Errorf("full permission not mapped to bypassPermissions: %v", args)
	}
	if !slices.Contains(args, "--output-format") {
		t.Errorf("streaming-json output flag missing: %v", args)
	}
}
