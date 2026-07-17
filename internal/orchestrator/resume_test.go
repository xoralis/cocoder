package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/execx"
	"github.com/xoralis/cocoder/internal/plan"
	"github.com/xoralis/cocoder/internal/state"
)

// seedInterruptedRun simulates a run that stopped after t1 succeeded:
// meta interrupted, plan.json + t1 state on disk (as a crash/budget-stop
// would leave them).
func seedInterruptedRun(t *testing.T, o *Orchestrator) {
	t.Helper()
	pl, err := plan.Parse(planJSON)
	if err != nil {
		t.Fatal(err)
	}
	pl.Architecture = "# Arch\ncontract: shared interfaces here"
	if err := o.Store.SavePlanFile(pl); err != nil {
		t.Fatal(err)
	}
	meta := &state.Meta{
		RunID: o.Store.RunID(), Mode: "run", Goal: "build the feature",
		Status: "interrupted", CreatedAt: time.Now(), PlannerCostUSD: 0.10, PlannerAttempts: 1,
	}
	if err := o.Store.SaveMeta(meta); err != nil {
		t.Fatal(err)
	}
	if err := o.Store.SaveTaskState(&state.TaskState{
		ID: "t1", Role: "backend", Title: "api", Status: adapter.StatusSucceeded,
		Attempts: 1, SessionID: "s1", Summary: "API DONE: endpoint ready",
		CostUSD: 0.20, ChangedFiles: []string{"server/api.go"},
		StartedAt: time.Now().Add(-time.Minute), EndedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if err := o.Store.SaveTaskState(&state.TaskState{
		ID: "t2", Role: "docs", Title: "write docs", Status: adapter.StatusPending,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestResumeRunsOnlyRemainingTasks(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		// Only t2 should spawn.
		{Stdout: lines(cInit("s2"), cResult("s2", false, "DOCS DONE", 0.30))},
	}}
	o := newOrch(t, fr)
	seedInterruptedRun(t, o)

	if err := o.Resume(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	if len(fr.Calls) != 1 {
		t.Fatalf("spawns = %d, want 1 (t1 must not re-run, no re-planning)", len(fr.Calls))
	}
	// t2's prompt must carry t1's blackboard note (rebuilt from disk).
	if !strings.Contains(fr.Calls[0].Stdin, "API DONE") {
		t.Errorf("t2 prompt missing t1's summary:\n%s", fr.Calls[0].Stdin)
	}
	if !strings.Contains(fr.Calls[0].Stdin, "contract: shared interfaces") {
		t.Errorf("t2 prompt missing the architecture from plan.json:\n%s", fr.Calls[0].Stdin)
	}

	meta, _ := o.Store.LoadMeta()
	if meta.Status != "completed" || meta.Resumes != 1 {
		t.Errorf("meta = %+v", meta)
	}
	states, _ := o.Store.LoadTaskStates()
	if states["t2"].Status != adapter.StatusSucceeded {
		t.Errorf("t2 = %+v", states["t2"])
	}
	if states["t1"].Summary != "API DONE: endpoint ready" {
		t.Errorf("t1 state must be untouched: %+v", states["t1"])
	}
}

func TestResumeFailedTaskGetsFailureContext(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("s2b"), cResult("s2b", false, "fixed now", 0.1))},
	}}
	o := newOrch(t, fr)
	seedInterruptedRun(t, o)
	// Overwrite t2 as failed with an error message.
	_ = o.Store.SaveTaskState(&state.TaskState{
		ID: "t2", Role: "docs", Title: "write docs", Status: adapter.StatusFailed,
		Attempts: 2, LastError: "could not find the schema file",
	})

	if err := o.Resume(context.Background(), RunOptions{}); err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	prompt := fr.Calls[0].Stdin
	if !strings.Contains(prompt, "Resume context") || !strings.Contains(prompt, "could not find the schema file") {
		t.Errorf("failed-task resume prompt missing failure context:\n%s", prompt)
	}
	states, _ := o.Store.LoadTaskStates()
	// Attempts must be cumulative: 2 prior + 1 new.
	if states["t2"].Attempts != 3 {
		t.Errorf("attempts = %d, want 3 (cumulative)", states["t2"].Attempts)
	}
}

func TestResumeInterruptedTaskToldToInspect(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("s2c"), cResult("s2c", false, "done", 0))},
	}}
	o := newOrch(t, fr)
	seedInterruptedRun(t, o)
	_ = o.Store.SaveTaskState(&state.TaskState{
		ID: "t2", Role: "docs", Title: "write docs", Status: adapter.StatusInterrupted, Attempts: 1,
	})

	if err := o.Resume(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fr.Calls[0].Stdin, "interrupted midway") {
		t.Errorf("interrupted-task prompt missing inspect note:\n%s", fr.Calls[0].Stdin)
	}
}

func TestResumeHonorsHandEditedPlan(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("s2"), cResult("s2", false, "done", 0))}, // edited t2
		{Stdout: lines(cInit("s3"), cResult("s3", false, "done", 0))}, // new t3
	}}
	o := newOrch(t, fr)
	seedInterruptedRun(t, o)
	// Hand-edit: change t2's description and add t3.
	pl, _ := o.Store.LoadPlanFile()
	pl.Tasks[1].Description = "EDITED-DESCRIPTION for docs"
	pl.Tasks = append(pl.Tasks, plan.Task{
		ID: "t3", Role: "backend", Title: "extra", Description: "one more thing",
		DependsOn: []string{"t2"}, FileScope: []string{"server/extra/"},
	})
	_ = o.Store.SavePlanFile(pl)

	if err := o.Resume(context.Background(), RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(fr.Calls) != 2 {
		t.Fatalf("spawns = %d, want 2 (edited t2 + new t3)", len(fr.Calls))
	}
	if !strings.Contains(fr.Calls[0].Stdin, "EDITED-DESCRIPTION") {
		t.Errorf("edited plan not honored:\n%s", fr.Calls[0].Stdin)
	}
	states, _ := o.Store.LoadTaskStates()
	if states["t3"] == nil || states["t3"].Status != adapter.StatusSucceeded {
		t.Errorf("t3 = %+v", states["t3"])
	}
}

func TestResumeBudgetCountsLifetimeSpend(t *testing.T) {
	fr := &execx.FakeRunner{}
	o := newOrch(t, fr)
	seedInterruptedRun(t, o) // prior spend: planner 0.10 + t1 0.20 = 0.30

	err := o.Resume(context.Background(), RunOptions{BudgetUSD: 0.25})
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("err = %v, want budget interruption", err)
	}
	if len(fr.Calls) != 0 {
		t.Errorf("spawns = %d, want 0 (lifetime spend already over budget)", len(fr.Calls))
	}
}

func TestResumeRejectsAssignRuns(t *testing.T) {
	o := newOrch(t, &execx.FakeRunner{})
	_ = o.Store.SaveMeta(&state.Meta{RunID: o.Store.RunID(), Mode: "assign", Goal: "g", Status: "failed", CreatedAt: time.Now()})
	if err := o.Resume(context.Background(), RunOptions{}); err == nil || !strings.Contains(err.Error(), "assign") {
		t.Fatalf("err = %v, want mode rejection", err)
	}
}

func TestFollowupResumesStoredSession(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{
		{Stdout: lines(cInit("s1-new"), cResult("s1-new", false, "amended the endpoint", 0.15))},
	}}
	o := newOrch(t, fr)
	seedInterruptedRun(t, o)
	bb := rebuildBlackboardForTest(t, o)
	_ = o.Store.SaveBlackboard(bb)

	if err := o.Followup(context.Background(), "t1", "also add a unit test", 0); err != nil {
		t.Fatalf("followup failed: %v", err)
	}
	call := fr.Calls[0]
	if !strings.Contains(strings.Join(call.Args, " "), "--resume s1") {
		t.Errorf("followup did not resume stored session: %v", call.Args)
	}
	if !strings.Contains(call.Stdin, "also add a unit test") {
		t.Errorf("instruction missing from prompt: %q", call.Stdin)
	}
	if !strings.Contains(call.Stdin, "server/") { // plan task scope enforced
		t.Errorf("scope missing from followup prompt: %q", call.Stdin)
	}

	states, _ := o.Store.LoadTaskStates()
	ts := states["t1"]
	if ts.Attempts != 2 || ts.SessionID != "s1-new" {
		t.Errorf("state not updated: %+v", ts)
	}
	if !strings.Contains(ts.Summary, "amended the endpoint") {
		t.Errorf("summary not refreshed: %q", ts.Summary)
	}
	// Blackboard note refreshed for future resumes.
	bb2, _ := o.Store.LoadBlackboard()
	if !strings.Contains(bb2.TaskNotes["t1"].Summary, "amended") {
		t.Errorf("blackboard note not refreshed: %+v", bb2.TaskNotes["t1"])
	}
}

func TestFollowupRequiresStoredSession(t *testing.T) {
	o := newOrch(t, &execx.FakeRunner{})
	seedInterruptedRun(t, o)
	_ = o.Store.SaveTaskState(&state.TaskState{ID: "t9", Role: "docs", Status: adapter.StatusSucceeded})
	if err := o.Followup(context.Background(), "t9", "do more", 0); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("err = %v, want no-session error", err)
	}
}

func rebuildBlackboardForTest(t *testing.T, o *Orchestrator) *state.Blackboard {
	t.Helper()
	pl, err := o.Store.LoadPlanFile()
	if err != nil {
		t.Fatal(err)
	}
	states, err := o.Store.LoadTaskStates()
	if err != nil {
		t.Fatal(err)
	}
	return rebuildBlackboard(pl, "build the feature", states)
}

func TestComputeResumeTable(t *testing.T) {
	pl, _ := plan.Parse(planJSON)
	states := map[string]*state.TaskState{
		"t1": {ID: "t1", Status: adapter.StatusSucceeded},
		"t2": {ID: "t2", Status: adapter.StatusRunning, Attempts: 1}, // crash leftover
	}
	prep := computeResume(pl, states)
	if prep.initial["t1"] != adapter.StatusSucceeded {
		t.Errorf("t1 = %v", prep.initial["t1"])
	}
	if prep.initial["t2"] != adapter.StatusPending {
		t.Errorf("t2 = %v", prep.initial["t2"])
	}
	if prep.attempts["t2"] != 1 || prep.notes["t2"] == "" {
		t.Errorf("t2 prep = attempts %d note %q", prep.attempts["t2"], prep.notes["t2"])
	}
}
