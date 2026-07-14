package orchestrator

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
	"github.com/xoralis/cocoder/internal/state"
	"github.com/xoralis/cocoder/internal/ui"
)

// --- claude stream-json fixture builders --------------------------------

func cInit(sess string) string {
	b, _ := json.Marshal(map[string]any{"type": "system", "subtype": "init", "session_id": sess})
	return string(b)
}

func cText(text string) string {
	b, _ := json.Marshal(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}},
	})
	return string(b)
}

func cResult(sess string, isError bool, result string, cost float64) string {
	b, _ := json.Marshal(map[string]any{
		"type": "result", "is_error": isError, "result": result,
		"session_id": sess, "total_cost_usd": cost, "num_turns": 1,
	})
	return string(b)
}

func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

const planJSON = `{"goal":"build the feature","tasks":[
 {"id":"t1","role":"backend","title":"api","description":"build the api","depends_on":[],"file_scope":["server/"],"acceptance":"works"},
 {"id":"t2","role":"docs","title":"write docs","description":"document the api","depends_on":["t1"],"file_scope":["docs/"],"acceptance":"readme"}]}`

func testConfig() *config.Config {
	cfg := config.DefaultConfig()
	cfg.Defaults.TaskTimeout = config.Duration(time.Minute)
	cfg.Roles = map[string]*config.Role{
		"architect": {CLI: "claude", Permission: config.PermReadOnly},
		"backend":   {CLI: "claude", Permission: config.PermEdits, FileScope: []string{"server/"}},
		"docs":      {CLI: "claude", Permission: config.PermEdits, FileScope: []string{"docs/"}},
	}
	return cfg
}

// newOrch wires an Orchestrator against a FakeRunner-backed claude adapter.
// The spec command is "go": it always resolves on PATH (Probe passes) while
// every spawn still goes through the FakeRunner, never the real binary.
func newOrch(t *testing.T, fr *execx.FakeRunner) *Orchestrator {
	t.Helper()
	cfg := testConfig()
	cfg.CLIs = map[string]*config.CLISpec{"claude": {Command: "go"}}
	registry, err := adapter.BuildRegistry(cfg, fr)
	if err != nil {
		t.Fatal(err)
	}
	store, err := state.CreateRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	return &Orchestrator{
		Cfg:      cfg,
		Registry: registry,
		Store:    store,
		Console:  ui.NewConsole(io.Discard, false),
		WorkDir:  t.TempDir(), // not a git repo: snapshots/scope checks disabled
		Version:  "test",
	}
}

func TestRunHappyPathFlowsBlackboard(t *testing.T) {
	plannerReply := "# Arch\ncontract: GET /health -> 200\n\n```json\n" + planJSON + "\n```"
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("sp"), cText(plannerReply), cResult("sp", false, "", 0.10))},
		{Stdout: lines(cInit("s1"), cText("building api"), cResult("s1", false, "API DONE: endpoint at server/health.go", 0.20))},
		{Stdout: lines(cInit("s2"), cText("writing docs"), cResult("s2", false, "DOCS DONE", 0.30))},
	}}
	o := newOrch(t, fr)
	err := o.Run(context.Background(), RunOptions{Goal: "build the feature"})
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}

	meta, _ := o.Store.LoadMeta()
	if meta.Status != "completed" {
		t.Errorf("meta.status = %q", meta.Status)
	}
	states, _ := o.Store.LoadTaskStates()
	if states["t1"].Status != adapter.StatusSucceeded || states["t2"].Status != adapter.StatusSucceeded {
		t.Errorf("task states: %+v %+v", states["t1"], states["t2"])
	}

	// Three spawns: planner, t1, t2 - in topo order.
	if len(fr.Calls) != 3 {
		t.Fatalf("spawns = %d, want 3", len(fr.Calls))
	}
	// The blackboard hand-off: t2's prompt must carry t1's condensed result
	// and the architecture contract.
	t2prompt := fr.Calls[2].Stdin
	if !strings.Contains(t2prompt, "API DONE") {
		t.Errorf("t2 prompt missing t1's summary:\n%s", t2prompt)
	}
	if !strings.Contains(t2prompt, "GET /health") {
		t.Errorf("t2 prompt missing the architecture contract:\n%s", t2prompt)
	}
	if !strings.Contains(t2prompt, "docs/") {
		t.Errorf("t2 prompt missing its file scope:\n%s", t2prompt)
	}

	// Blackboard persisted with both notes.
	bb, err := o.Store.LoadBlackboard()
	if err != nil {
		t.Fatal(err)
	}
	if len(bb.TaskNotes) != 2 || !strings.Contains(bb.TaskNotes["t1"].Summary, "API DONE") {
		t.Errorf("blackboard notes: %+v", bb.TaskNotes)
	}
	// plan.json + architecture.md persisted.
	if pl, err := o.Store.LoadPlanFile(); err != nil || len(pl.Tasks) != 2 {
		t.Errorf("plan.json: %v %+v", err, pl)
	}
}

func TestRunTaskFailureBlocksDependents(t *testing.T) {
	plannerReply := "arch\n\n```json\n" + planJSON + "\n```"
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("sp"), cText(plannerReply), cResult("sp", false, "", 0.1))},
		{Stdout: lines(cInit("s1"), cResult("s1", true, "compile error", 0.1))}, // t1 attempt 1
		{Stdout: lines(cInit("s1"), cResult("s1", true, "still broken", 0.1))},  // t1 retry
	}}
	o := newOrch(t, fr)
	err := o.Run(context.Background(), RunOptions{Goal: "build"})
	if err == nil {
		t.Fatal("expected run failure")
	}

	states, _ := o.Store.LoadTaskStates()
	if states["t1"].Status != adapter.StatusFailed || states["t1"].Attempts != 2 {
		t.Errorf("t1 = %+v", states["t1"])
	}
	if states["t2"].Status != adapter.StatusBlocked {
		t.Errorf("t2 = %+v", states["t2"])
	}
	meta, _ := o.Store.LoadMeta()
	if meta.Status != "failed" {
		t.Errorf("meta.status = %q", meta.Status)
	}
	// Retry must resume the captured session with failure feedback.
	retry := fr.Calls[2]
	if !strings.Contains(strings.Join(retry.Args, " "), "--resume s1") {
		t.Errorf("retry args = %v", retry.Args)
	}
	if !strings.Contains(retry.Stdin, "compile error") {
		t.Errorf("retry prompt missing failure context: %q", retry.Stdin)
	}
}

func TestRunBudgetStopsBeforeNextTask(t *testing.T) {
	plannerReply := "arch\n\n```json\n" + planJSON + "\n```"
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("sp"), cText(plannerReply), cResult("sp", false, "", 0.30))},
		{Stdout: lines(cInit("s1"), cResult("s1", false, "done", 0.80))}, // t1 pushes over budget
	}}
	o := newOrch(t, fr)
	err := o.Run(context.Background(), RunOptions{Goal: "build", BudgetUSD: 1.0})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v", err)
	}
	if len(fr.Calls) != 2 {
		t.Errorf("spawns = %d, want 2 (t2 must not start)", len(fr.Calls))
	}
	states, _ := o.Store.LoadTaskStates()
	if states["t2"].Status != adapter.StatusPending {
		t.Errorf("t2 = %+v, want pending", states["t2"])
	}
}

func TestRunPlanFileSkipsPlanner(t *testing.T) {
	dir := t.TempDir()
	planPath := dir + "/plan.json"
	if err := writeFile(planPath, planJSON); err != nil {
		t.Fatal(err)
	}
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("s1"), cResult("s1", false, "t1 ok", 0))},
		{Stdout: lines(cInit("s2"), cResult("s2", false, "t2 ok", 0))},
	}}
	o := newOrch(t, fr)
	if err := o.Run(context.Background(), RunOptions{PlanFile: planPath, Goal: "from plan"}); err != nil {
		t.Fatal(err)
	}
	if len(fr.Calls) != 2 {
		t.Errorf("spawns = %d, want 2 (no planner call)", len(fr.Calls))
	}
}

func TestRunConfirmAborts(t *testing.T) {
	plannerReply := "arch\n\n```json\n" + planJSON + "\n```"
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("sp"), cText(plannerReply), cResult("sp", false, "", 0))},
	}}
	o := newOrch(t, fr)
	err := o.Run(context.Background(), RunOptions{Goal: "build", Confirm: true, Stdin: strings.NewReader("n\n")})
	if err != nil {
		t.Fatalf("aborting at confirm should not be an error: %v", err)
	}
	if len(fr.Calls) != 1 {
		t.Errorf("spawns = %d, want 1 (no tasks executed)", len(fr.Calls))
	}
	meta, _ := o.Store.LoadMeta()
	if meta.Status != "interrupted" {
		t.Errorf("meta.status = %q", meta.Status)
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
