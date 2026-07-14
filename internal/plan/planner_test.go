package plan

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// --- claude stream-json fixture builders -------------------------------

func claudeInit(sess string) string {
	b, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "session_id": sess})
	return string(b)
}

func claudeText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}},
	})
	return string(b)
}

func claudeResult(sess string, isError bool, result string, structured any) string {
	m := map[string]any{
		"type": "result", "is_error": isError, "result": result,
		"session_id": sess, "total_cost_usd": 0.05, "num_turns": 1,
	}
	if structured != nil {
		m["structured_output"] = structured
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func jsonl(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// --- test scaffolding ----------------------------------------------------

func plannerConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Roles = map[string]*config.Role{
		"architect": {CLI: "claude", Permission: config.PermReadOnly},
		"backend":   {CLI: "claude", Permission: config.PermEdits, FileScope: []string{"server/"}},
		"docs":      {CLI: "claude", Permission: config.PermEdits, FileScope: []string{"docs/"}},
	}
	return cfg
}

// missing command -> argvSafeCommand is false -> the --json-schema flag is
// never appended, keeping arg assertions machine-independent.
func plannerAdapter(fr *execx.FakeRunner) adapter.Adapter {
	spec := &config.CLISpec{Name: "claude", Adapter: "claude", Command: "no-such-claude-xyz", Output: "jsonl", PromptVia: "stdin"}
	return adapter.NewClaude(spec, fr)
}

func drainQuiet(ch <-chan adapter.Event) (*adapter.TaskResult, string) {
	var final *adapter.TaskResult
	var sb strings.Builder
	for e := range ch {
		if e.Kind == adapter.EvAgentText {
			sb.WriteString(e.Text)
			sb.WriteString("\n")
		}
		if e.Kind == adapter.EvResult {
			final = e.Result
		}
	}
	return final, sb.String()
}

func deps(fr *execx.FakeRunner, cfg *config.Config, retries int) Deps {
	return Deps{
		Adapter:  plannerAdapter(fr),
		Cfg:      cfg,
		RoleName: "architect",
		WorkDir:  ".",
		Timeout:  30 * time.Second,
		Retries:  retries,
		Drain:    drainQuiet,
		SaveRaw:  func(int, string) {},
	}
}

const goodPlanJSON = `{"goal":"g","tasks":[
  {"id":"t1","role":"backend","title":"api","description":"build the api","depends_on":[],"file_scope":["server/"],"acceptance":"works"},
  {"id":"t2","role":"docs","title":"docs","description":"document it","depends_on":["t1"],"file_scope":["docs/"],"acceptance":"readme updated"}]}`

// --- tests ---------------------------------------------------------------

func TestGenerateFencePath(t *testing.T) {
	reply := "# Architecture\nGo HTTP server, /health returns 200.\n\n```json\n" + goodPlanJSON + "\n```"
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: jsonl(claudeInit("s1"), claudeText(reply), claudeResult("s1", false, "done", nil))},
	}}
	p, stats, err := Generate(context.Background(), "add /health", deps(fr, plannerConfig(), 2))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Tasks) != 2 || p.Tasks[0].ID != "t1" {
		t.Errorf("plan = %+v", p)
	}
	if !strings.Contains(p.Architecture, "# Architecture") {
		t.Errorf("architecture not captured: %q", p.Architecture)
	}
	if stats.Attempts != 1 || stats.CostUSD != 0.05 || stats.SessionID != "s1" {
		t.Errorf("stats = %+v", stats)
	}
}

func TestGenerateStructuredOutputPath(t *testing.T) {
	var structured map[string]any
	if err := json.Unmarshal([]byte(goodPlanJSON), &structured); err != nil {
		t.Fatal(err)
	}
	structured["architecture"] = "# Arch from schema"
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: jsonl(claudeInit("s1"), claudeResult("s1", false, "", structured))},
	}}
	p, _, err := Generate(context.Background(), "goal", deps(fr, plannerConfig(), 0))
	if err != nil {
		t.Fatal(err)
	}
	if p.Architecture != "# Arch from schema" || len(p.Tasks) != 2 {
		t.Errorf("plan = %+v", p)
	}
}

func TestGenerateRetryWithResume(t *testing.T) {
	badPlan := strings.Replace(goodPlanJSON, `"role":"backend"`, `"role":"devops"`, 1)
	firstReply := "# Arch v1\n\n```json\n" + badPlan + "\n```"
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: jsonl(claudeInit("s1"), claudeText(firstReply), claudeResult("s1", false, "", nil))},
		// Retry replies with bare corrected JSON (no fence, no architecture).
		{Stdout: jsonl(claudeInit("s2"), claudeText(goodPlanJSON), claudeResult("s2", false, "", nil))},
	}}
	p, stats, err := Generate(context.Background(), "goal", deps(fr, plannerConfig(), 2))
	if err != nil {
		t.Fatal(err)
	}
	if stats.Attempts != 2 {
		t.Errorf("attempts = %d, want 2", stats.Attempts)
	}
	// Architecture from attempt 1 must be preserved.
	if !strings.Contains(p.Architecture, "Arch v1") {
		t.Errorf("architecture lost on retry: %q", p.Architecture)
	}
	// Attempt 2 must resume the session and carry the validation feedback.
	second := fr.Calls[1]
	if !slices.Contains(second.Args, "--resume") || !slices.Contains(second.Args, "s1") {
		t.Errorf("retry did not resume: %v", second.Args)
	}
	if !strings.Contains(second.Stdin, "devops") {
		t.Errorf("feedback prompt missing the validation error: %q", second.Stdin)
	}
}

func TestGenerateGivesUpAfterRetries(t *testing.T) {
	junk := jsonl(claudeInit("s1"), claudeText("no plan here, sorry"), claudeResult("s1", false, "", nil))
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Stdout: junk}}} // reused for every attempt
	saved := 0
	d := deps(fr, plannerConfig(), 1)
	d.SaveRaw = func(int, string) { saved++ }
	_, stats, err := Generate(context.Background(), "goal", d)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "--plan") {
		t.Errorf("error should point at the --plan escape hatch: %v", err)
	}
	if stats.Attempts != 2 || saved != 2 {
		t.Errorf("attempts = %d saved = %d, want 2/2", stats.Attempts, saved)
	}
}

func TestGenerateArchitectRunFailed(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: "", Stderr: "not logged in\n", Exit: 1},
	}}
	_, _, err := Generate(context.Background(), "goal", deps(fr, plannerConfig(), 2))
	if err == nil || !strings.Contains(err.Error(), "architect run failed") {
		t.Fatalf("err = %v", err)
	}
	if len(fr.Calls) != 1 {
		t.Errorf("environmental failure must not retry, got %d calls", len(fr.Calls))
	}
}
