package orchestrator

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/gitx"
	"github.com/xoralis/cocoder/internal/plan"
	"github.com/xoralis/cocoder/internal/state"
	"github.com/xoralis/cocoder/internal/ui"
)

// Orchestrator runs the plan -> schedule -> execute -> report pipeline.
// All state mutation happens on the calling goroutine (single writer).
type Orchestrator struct {
	Cfg      *config.Config
	Registry map[string]adapter.Adapter
	Store    *state.RunStore
	Console  *ui.Console
	WorkDir  string
	Version  string
}

// RunOptions parameterizes one `ccd run` (and, minus Confirm/PlanFile,
// one `ccd resume`).
type RunOptions struct {
	Goal      string
	Confirm   bool          // pause for plan approval
	PlanFile  string        // pre-supplied plan.json, skips the planner
	BudgetUSD float64       // 0 = use config default
	Timeout   time.Duration // per-task override (0 = role/config default)
	Degrade   bool          // fall back to a single builder-role task if planning fails
	Stdin     io.Reader     // confirm-gate input (defaults to nothing = auto-abort)
}

const (
	maxArchitectureRunes = 12000
	plannerTaskID        = "plan"
)

// Run executes one full orchestration. The returned error describes why the
// run did not complete (planning failure, task failure, interruption).
func (o *Orchestrator) Run(ctx context.Context, opts RunOptions) error {
	meta := &state.Meta{
		RunID:      o.Store.RunID(),
		Mode:       "run",
		Goal:       opts.Goal,
		Status:     "running",
		CcdVersion: o.Version,
		CreatedAt:  time.Now(),
	}
	o.noteStartHead(meta)
	if err := o.Store.SaveMeta(meta); err != nil {
		return err
	}

	// ---- 1. Acquire the plan --------------------------------------------
	pl, plannerCost, plannerAttempts, err := o.acquirePlan(ctx, opts)
	meta.PlannerCostUSD += plannerCost
	meta.PlannerAttempts += plannerAttempts
	if err != nil {
		o.finishMeta(meta, "failed")
		return err
	}
	if pl.Goal == "" {
		pl.Goal = opts.Goal
	}
	if meta.Goal == "" {
		meta.Goal = pl.Goal
	}
	_ = o.Store.SaveMeta(meta)
	if err := o.Store.SavePlanFile(pl); err != nil {
		return err
	}
	_ = o.Store.SaveArchitecture(pl.Architecture)

	// Every task role must have a working adapter before we start.
	if err := o.checkTaskRoles(ctx, pl); err != nil {
		o.finishMeta(meta, "failed")
		return err
	}

	bb := &state.Blackboard{Goal: pl.Goal, Architecture: pl.Architecture, TaskNotes: map[string]state.TaskNote{}}
	_ = o.Store.SaveBlackboard(bb)

	order, err := pl.TopoSort()
	if err != nil {
		o.finishMeta(meta, "failed")
		return fmt.Errorf("plan is not executable: %w", err)
	}

	// ---- 2. Confirm gate -------------------------------------------------
	o.printPlanTable(pl, order)
	if opts.Confirm {
		if !o.askConfirm(opts.Stdin) {
			o.finishMeta(meta, "interrupted")
			o.Console.Printf("aborted. edit the plan at %s then continue with:\n  ccd resume %s",
				o.Store.PlanPath(), meta.RunID)
			return nil
		}
	}

	// ---- 3+4. Execute and report -----------------------------------------
	return o.execute(ctx, meta, pl, bb, nil, opts, meta.PlannerCostUSD, nil)
}

// Resume continues an interrupted/failed/crashed run from its persisted
// state: succeeded tasks keep their results (their summaries return to the
// blackboard), failed tasks get a fresh round with the failure as context,
// and interrupted ones are told to inspect partial changes first. plan.json
// is reloaded, so hand edits between runs are honored.
func (o *Orchestrator) Resume(ctx context.Context, opts RunOptions) error {
	meta, err := o.Store.LoadMeta()
	if err != nil {
		return fmt.Errorf("read run meta: %w", err)
	}
	if meta.Mode != "run" {
		return fmt.Errorf("run %s is mode %q; only 'run' mode runs can be resumed", meta.RunID, meta.Mode)
	}
	o.noteStartHead(meta) // keeps the original StartHead when already set

	// Reload the plan (hand-edited versions win); re-plan if it never got
	// written (planner failed on the original attempt).
	pl, err := o.Store.LoadPlanFile()
	if err != nil {
		o.Console.Warnf("run %s has no plan.json - re-planning from the recorded goal", meta.RunID)
		if opts.Goal == "" {
			opts.Goal = meta.Goal
		}
		var cost float64
		var attempts int
		pl, cost, attempts, err = o.acquirePlan(ctx, opts)
		meta.PlannerCostUSD += cost
		meta.PlannerAttempts += attempts
		if err != nil {
			o.finishMeta(meta, "failed")
			return err
		}
		if pl.Goal == "" {
			pl.Goal = meta.Goal
		}
		if err := o.Store.SavePlanFile(pl); err != nil {
			return err
		}
		_ = o.Store.SaveArchitecture(pl.Architecture)
	}
	if errs := pl.Validate(o.roleSet()); len(errs) > 0 {
		return fmt.Errorf("plan.json in run %s is invalid (hand edits?):\n%s", meta.RunID, bulletize(errs))
	}
	if err := o.checkTaskRoles(ctx, pl); err != nil {
		return err
	}
	order, err := pl.TopoSort()
	if err != nil {
		return fmt.Errorf("plan is not executable: %w", err)
	}

	states, err := o.Store.LoadTaskStates()
	if err != nil {
		return err
	}
	prep := computeResume(pl, states)
	bb := rebuildBlackboard(pl, meta.Goal, states)
	_ = o.Store.SaveBlackboard(bb)

	done := 0
	for _, st := range prep.initial {
		if st == adapter.StatusSucceeded {
			done++
		}
	}
	meta.Resumes++
	meta.Status = "running"
	meta.EndedAt = nil
	_ = o.Store.SaveMeta(meta)
	o.Console.Printf("resuming run %s: %d/%d task(s) already done", meta.RunID, done, len(pl.Tasks))
	o.printPlanTable(pl, order)

	// Budget covers the run's lifetime spend, not just this session.
	priorSpend := meta.PlannerCostUSD
	for _, ts := range states {
		priorSpend += ts.CostUSD
	}
	return o.execute(ctx, meta, pl, bb, prep, opts, priorSpend, states)
}

// resumePrep carries the restart state computed from persisted task states.
type resumePrep struct {
	initial  map[string]adapter.TaskStatus // scheduler starting statuses
	notes    map[string]string             // per-task context appended to the prompt
	attempts map[string]int                // prior attempt counts (kept cumulative)
}

// computeResume maps persisted task states onto scheduler starting states:
//
//	succeeded            -> succeeded (kept)
//	failed               -> pending, with the failure as prompt context
//	running/interrupted  -> pending, told to inspect partial changes first
//	blocked/pending/none -> pending
func computeResume(pl *plan.Plan, states map[string]*state.TaskState) *resumePrep {
	prep := &resumePrep{
		initial:  map[string]adapter.TaskStatus{},
		notes:    map[string]string{},
		attempts: map[string]int{},
	}
	for _, t := range pl.Tasks {
		st := states[t.ID]
		if st == nil {
			prep.initial[t.ID] = adapter.StatusPending
			continue
		}
		switch st.Status {
		case adapter.StatusSucceeded:
			prep.initial[t.ID] = adapter.StatusSucceeded
		case adapter.StatusFailed:
			prep.initial[t.ID] = adapter.StatusPending
			prep.attempts[t.ID] = st.Attempts
			note := "A previous attempt of this task failed"
			if st.LastError != "" {
				note += ":\n" + clip(st.LastError, 1000)
			}
			prep.notes[t.ID] = note + "\nInspect the current state of the files, fix the problem, and complete the task."
		case adapter.StatusRunning, adapter.StatusInterrupted:
			prep.initial[t.ID] = adapter.StatusPending
			prep.attempts[t.ID] = st.Attempts
			prep.notes[t.ID] = "A previous attempt of this task was interrupted midway. Partial changes may already exist - inspect the current state of the files first, then complete the task."
		default:
			prep.initial[t.ID] = adapter.StatusPending
		}
	}
	return prep
}

// rebuildBlackboard reconstructs the shared context from persisted task
// states (the single source of truth), rather than trusting a possibly
// stale blackboard.json.
func rebuildBlackboard(pl *plan.Plan, goal string, states map[string]*state.TaskState) *state.Blackboard {
	bb := &state.Blackboard{Goal: goal, Architecture: pl.Architecture, TaskNotes: map[string]state.TaskNote{}}
	if bb.Goal == "" {
		bb.Goal = pl.Goal
	}
	for _, t := range pl.Tasks {
		st := states[t.ID]
		if st == nil || st.Status != adapter.StatusSucceeded {
			continue
		}
		bb.TaskNotes[t.ID] = state.TaskNote{
			ID: t.ID, Title: t.Title, Role: t.Role,
			Summary: st.Summary, ChangedFiles: st.ChangedFiles,
		}
	}
	return bb
}

// execute is the single-writer main loop shared by Run and Resume.
// priorSpend seeds the budget accounting (planner + previous sessions);
// prior seeds the report table with already-finished task states.
func (o *Orchestrator) execute(ctx context.Context, meta *state.Meta, pl *plan.Plan, bb *state.Blackboard,
	prep *resumePrep, opts RunOptions, priorSpend float64, prior map[string]*state.TaskState) error {

	budget := opts.BudgetUSD
	if budget == 0 {
		budget = o.Cfg.Defaults.BudgetUSD
	}
	totalCost := priorSpend

	order, err := pl.TopoSort()
	if err != nil {
		o.finishMeta(meta, "failed")
		return fmt.Errorf("plan is not executable: %w", err)
	}

	var initial map[string]adapter.TaskStatus
	notes := map[string]string{}
	attempts := map[string]int{}
	if prep != nil {
		initial, notes, attempts = prep.initial, prep.notes, prep.attempts
	}

	sched := newScheduler(pl, order, initial)
	states := map[string]*state.TaskState{}
	for id, ts := range prior {
		states[id] = ts
	}
	stopReason := ""

	for {
		if ctx.Err() != nil {
			meta.Status = "interrupted"
			stopReason = "interrupted - continue with: ccd resume " + meta.RunID
			break
		}
		t := sched.nextReady()
		if t == nil {
			break
		}
		if budget > 0 && totalCost >= budget {
			meta.Status = "interrupted"
			stopReason = fmt.Sprintf("budget $%.2f reached (spent $%.4f) - stopped before task %s; continue with: ccd resume %s",
				budget, totalCost, t.ID, meta.RunID)
			break
		}

		ts := o.runTask(ctx, t, bb, opts, notes[t.ID], attempts[t.ID])
		states[t.ID] = ts
		totalCost += ts.CostUSD
		sched.mark(t.ID, ts.Status)

		if ts.Status != adapter.StatusSucceeded {
			sched.blockDependents(t.ID)
			if ts.Status == adapter.StatusInterrupted {
				meta.Status = "interrupted"
			} else {
				meta.Status = "failed"
			}
			break
		}
	}

	// Persist never-run tasks so `ccd status` explains them.
	for id, st := range sched.status {
		if _, ran := states[id]; ran {
			continue
		}
		if st == adapter.StatusPending || st == adapter.StatusBlocked {
			t := pl.Task(id)
			ts := &state.TaskState{ID: id, Role: t.Role, Title: t.Title, Status: st}
			_ = o.Store.SaveTaskState(ts)
			states[id] = ts
		}
	}

	if meta.Status == "running" {
		meta.Status = "completed"
	}
	o.finishMeta(meta, meta.Status)

	o.report(meta, pl, order, states, totalCost, budget, stopReason)

	switch meta.Status {
	case "completed":
		return nil
	case "interrupted":
		return errors.New("run interrupted - continue with: ccd resume " + meta.RunID)
	default:
		return fmt.Errorf("run failed - see the report above; retry with: ccd resume %s", meta.RunID)
	}
}

// Followup sends one more instruction into a finished task's stored CLI
// session: same working context, no re-planning. A successful followup
// refreshes the task's summary on the blackboard (and can flip a failed
// task back to succeeded, unblocking dependents on the next resume).
func (o *Orchestrator) Followup(ctx context.Context, taskID, instruction string, timeout time.Duration) error {
	states, err := o.Store.LoadTaskStates()
	if err != nil {
		return err
	}
	ts := states[taskID]
	if ts == nil {
		return fmt.Errorf("task %q not found in run %s (see: ccd status %s)", taskID, o.Store.RunID(), o.Store.RunID())
	}
	if ts.SessionID == "" {
		return fmt.Errorf("task %s has no stored session to resume", taskID)
	}
	role := o.Cfg.Roles[ts.Role]
	if role == nil {
		return fmt.Errorf("role %q (from the task state) is no longer defined in the config", ts.Role)
	}
	ad, ok := o.Registry[role.CLI]
	if !ok {
		return fmt.Errorf("no adapter for cli %q", role.CLI)
	}
	if !ad.Caps().Resume {
		return fmt.Errorf("cli %q cannot resume sessions", role.CLI)
	}
	if pr := ad.Probe(ctx); !pr.Found {
		return fmt.Errorf("cli %q not found in PATH - run 'ccd doctor'", role.CLI)
	}

	// Scope: prefer the plan task's scope, fall back to the role default.
	scope := role.FileScope
	if pl, perr := o.Store.LoadPlanFile(); perr == nil {
		if t := pl.Task(taskID); t != nil && len(t.FileScope) > 0 {
			scope = t.FileScope
		}
	}
	if timeout == 0 {
		timeout = role.Timeout.D()
	}
	if timeout == 0 {
		timeout = o.Cfg.Defaults.TaskTimeout.D()
	}

	var snap *gitx.Snapshot
	if gitx.IsRepo(o.WorkDir) {
		snap, _ = gitx.Take(o.WorkDir)
	}

	logW, lerr := o.Store.TaskLog(taskID) // append mode: same log file
	if lerr != nil {
		logW = nil
	}
	started := adapter.Event{
		Kind: adapter.EvTaskStarted, TaskID: taskID, Role: ts.Role,
		Time: time.Now(), Text: "followup: " + clip(instruction, 100),
	}
	o.Console.Handle(started)
	o.Store.AppendEvent(started)

	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	startAt := time.Now()
	ch, err := ad.Run(tctx, adapter.TaskInput{
		TaskID:          taskID,
		Role:            ts.Role,
		Prompt:          buildFollowupPrompt(instruction, scope),
		WorkDir:         o.WorkDir,
		Model:           role.Model,
		Permission:      role.Permission,
		ResumeSessionID: ts.SessionID,
		ExtraArgs:       role.ExtraArgs,
		RawLog:          logW,
	})
	if err != nil {
		if logW != nil {
			logW.Close()
		}
		return err
	}
	final, text := o.drain(ch)
	if logW != nil {
		logW.Close()
	}

	var changed []string
	if snap != nil {
		changed, _ = snap.ChangedSince()
	}
	if len(scope) > 0 && len(changed) > 0 {
		if v := plan.ScopeViolations(changed, scope); len(v) > 0 {
			o.Console.Warnf("followup touched files outside the task's file_scope: %s", strings.Join(v, ", "))
			ts.ScopeViolations = mergeUnique(ts.ScopeViolations, v)
		}
	}

	ts.Attempts++
	ts.EndedAt = time.Now()
	ts.CostUSD += final.CostUSD
	ts.NumTurns += final.NumTurns
	if final.SessionID != "" {
		ts.SessionID = final.SessionID
	}
	ts.ChangedFiles = mergeUnique(ts.ChangedFiles, changed)
	if final.Status == adapter.StatusSucceeded {
		ts.Status = adapter.StatusSucceeded
		ts.Summary = condenseResult(final, text, changed)
		ts.LastError = ""
		if bb, berr := o.Store.LoadBlackboard(); berr == nil {
			bb.TaskNotes[taskID] = state.TaskNote{
				ID: taskID, Title: ts.Title, Role: ts.Role,
				Summary: ts.Summary, ChangedFiles: ts.ChangedFiles,
			}
			_ = o.Store.SaveBlackboard(bb)
		}
	} else {
		ts.LastError = final.ErrMsg
	}
	_ = o.Store.SaveTaskState(ts)

	dur := time.Since(startAt).Round(time.Second)
	switch final.Status {
	case adapter.StatusSucceeded:
		o.Console.Successf("followup on %s succeeded in %s", taskID, dur)
		if final.CostUSD > 0 {
			o.Console.Printf("cost: $%.4f (%d turns)", final.CostUSD, final.NumTurns)
		}
		for _, f := range changed {
			o.Console.Printf("  ~ %s", f)
		}
		return nil
	case adapter.StatusInterrupted:
		o.Console.Warnf("followup on %s interrupted after %s", taskID, dur)
		return errors.New("followup interrupted")
	default:
		o.Console.Errorf("followup on %s failed after %s", taskID, dur)
		return fmt.Errorf("followup failed: %s", clip(final.ErrMsg, 200))
	}
}

func buildFollowupPrompt(instruction string, scope []string) string {
	var b strings.Builder
	b.WriteString(instruction)
	b.WriteString("\n\n## Constraints\n")
	if len(scope) > 0 {
		b.WriteString("You may ONLY create or modify files under:\n")
		for _, s := range scope {
			fmt.Fprintf(&b, "- %s\n", s)
		}
	}
	b.WriteString("Do not run git commit or push.\nEnd your reply with a short summary of what you changed.")
	return b.String()
}

func mergeUnique(base, add []string) []string {
	seen := map[string]bool{}
	for _, s := range base {
		seen[s] = true
	}
	out := base
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// noteStartHead records the repo head at run start (kept across resumes so
// the final report diffs the whole run's lifetime).
func (o *Orchestrator) noteStartHead(meta *state.Meta) {
	if !gitx.IsRepo(o.WorkDir) {
		o.Console.Warnf("not a git repository - diffs and scope checks are disabled")
		return
	}
	if meta.StartHead == "" {
		meta.StartHead = gitx.Head(o.WorkDir)
	}
}

// acquirePlan loads --plan or invokes the architect (with --degrade
// fallback). Returns the plan plus planner cost/attempt accounting.
func (o *Orchestrator) acquirePlan(ctx context.Context, opts RunOptions) (*plan.Plan, float64, int, error) {
	if opts.PlanFile != "" {
		pl, err := loadPlanFile(opts.PlanFile)
		if err != nil {
			return nil, 0, 0, err
		}
		if errs := pl.Validate(o.roleSet()); len(errs) > 0 {
			return nil, 0, 0, fmt.Errorf("plan file %s is invalid:\n%s", opts.PlanFile, bulletize(errs))
		}
		o.Console.Printf("using plan from %s (planner skipped)", opts.PlanFile)
		return pl, 0, 0, nil
	}

	roleName := o.plannerRoleName()
	role, ok := o.Cfg.Roles[roleName]
	if !ok {
		return nil, 0, 0, fmt.Errorf("planner role %q is not defined in the config (set defaults.planner_role or add the role)", roleName)
	}
	ad, ok := o.Registry[role.CLI]
	if !ok {
		return nil, 0, 0, fmt.Errorf("no adapter available for the planner role's cli %q", role.CLI)
	}
	if pr := ad.Probe(ctx); !pr.Found {
		return nil, 0, 0, fmt.Errorf("planner cli %q not found in PATH - run 'ccd doctor'", role.CLI)
	}

	o.Console.Printf("planning with role %s (%s)...", roleName, role.CLI)
	timeout := role.Timeout.D()
	if timeout == 0 {
		timeout = o.Cfg.Defaults.TaskTimeout.D()
	}
	logW, _ := o.Store.TaskLog(plannerTaskID)
	defer func() {
		if logW != nil {
			logW.Close()
		}
	}()

	pl, stats, err := plan.Generate(ctx, opts.Goal, plan.Deps{
		Adapter:  o.adapterWithLog(ad, logW),
		Cfg:      o.Cfg,
		RoleName: roleName,
		WorkDir:  o.WorkDir,
		Timeout:  timeout,
		Retries:  o.Cfg.Defaults.PlanRetries,
		Drain:    o.drain,
		SaveRaw:  o.Store.SavePlannerRaw,
	})
	if err != nil && opts.Degrade && ctx.Err() == nil {
		o.Console.Warnf("PLANNING FAILED (%v)", err)
		o.Console.Warnf("--degrade: falling back to a SINGLE task for role %q with the whole requirement", o.builderRoleName())
		return o.degradedPlan(opts.Goal), stats.CostUSD, stats.Attempts, nil
	}
	if err != nil {
		return nil, stats.CostUSD, stats.Attempts, err
	}
	o.Console.Printf("plan ready: %d task(s), planner cost $%.4f (%d attempt(s))",
		len(pl.Tasks), stats.CostUSD, stats.Attempts)
	return pl, stats.CostUSD, stats.Attempts, nil
}

// adapterWithLog wraps an adapter so every Run carries the planner log
// writer without threading RawLog through plan.Deps.
func (o *Orchestrator) adapterWithLog(ad adapter.Adapter, logW io.Writer) adapter.Adapter {
	return &logAdapter{Adapter: ad, log: logW}
}

type logAdapter struct {
	adapter.Adapter
	log io.Writer
}

func (l *logAdapter) Run(ctx context.Context, in adapter.TaskInput) (<-chan adapter.Event, error) {
	if in.RawLog == nil {
		in.RawLog = l.log
	}
	return l.Adapter.Run(ctx, in)
}

func (o *Orchestrator) degradedPlan(goal string) *plan.Plan {
	role := o.builderRoleName()
	return &plan.Plan{
		Goal: goal,
		Tasks: []plan.Task{{
			ID:          "t1",
			Role:        role,
			Title:       clip(goal, 80),
			Description: goal,
			FileScope:   o.Cfg.Roles[role].FileScope,
			Acceptance:  "the requirement is implemented",
		}},
	}
}

// runTask executes one task with retries; returns the persisted TaskState.
// resumeNote carries cross-session context (prior failure/interruption);
// prevAttempts keeps the attempt counter cumulative across resumes.
func (o *Orchestrator) runTask(ctx context.Context, t *plan.Task, bb *state.Blackboard, opts RunOptions,
	resumeNote string, prevAttempts int) *state.TaskState {

	role := o.Cfg.Roles[t.Role]
	ad := o.Registry[role.CLI]

	retries := o.Cfg.Defaults.Retries
	if role.Retries != nil {
		retries = *role.Retries
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = role.Timeout.D()
	}
	if timeout == 0 {
		timeout = o.Cfg.Defaults.TaskTimeout.D()
	}

	ts := &state.TaskState{
		ID: t.ID, Role: t.Role, Title: t.Title,
		Status: adapter.StatusRunning, Attempts: prevAttempts, StartedAt: time.Now(),
	}
	_ = o.Store.SaveTaskState(ts)
	started := adapter.Event{Kind: adapter.EvTaskStarted, TaskID: t.ID, Role: t.Role, Time: time.Now(), Text: t.Title}
	o.Console.Handle(started)
	o.Store.AppendEvent(started)

	var snap *gitx.Snapshot
	if gitx.IsRepo(o.WorkDir) {
		snap, _ = gitx.Take(o.WorkDir)
	}

	basePrompt := o.taskPrompt(t, role, bb)
	if resumeNote != "" {
		basePrompt += "\n\n## Resume context\n" + resumeNote
	}
	var (
		final    *adapter.TaskResult
		lastText string
		sid      string
		lastErr  string
	)
	for attempt := 1; attempt <= 1+retries; attempt++ {
		prompt, resume := basePrompt, ""
		if attempt > 1 {
			o.Console.Warnf("task %s failed (%s) - retrying, attempt %d/%d", t.ID, clip(lastErr, 120), attempt, 1+retries)
			if ad.Caps().Resume && sid != "" {
				resume = sid
				prompt = "The previous attempt failed:\n" + clip(lastErr, 1500) +
					"\nFix the problem and complete the original task. End with the same finish summary."
			} else {
				prompt = basePrompt + "\n\n## Previous attempt failed\n" + clip(lastErr, 1500) + "\nFix the problem and complete the task."
			}
		}

		logW, lerr := o.Store.TaskLog(t.ID)
		if lerr != nil {
			logW = nil
		}
		tctx, cancel := context.WithTimeout(ctx, timeout)
		ch, err := ad.Run(tctx, adapter.TaskInput{
			TaskID:          t.ID,
			Role:            t.Role,
			Prompt:          prompt,
			WorkDir:         o.WorkDir,
			Model:           role.Model,
			Permission:      role.Permission,
			ResumeSessionID: resume,
			ExtraArgs:       role.ExtraArgs,
			RawLog:          logW,
		})
		if err != nil {
			cancel()
			if logW != nil {
				logW.Close()
			}
			final = &adapter.TaskResult{Status: adapter.StatusFailed, ErrMsg: err.Error()}
			break // spawn failures are environmental; retrying won't help
		}
		res, text := o.drain(ch)
		cancel()
		if logW != nil {
			logW.Close()
		}

		final, lastText = res, text
		ts.Attempts = prevAttempts + attempt
		ts.CostUSD += res.CostUSD
		ts.NumTurns += res.NumTurns
		if res.SessionID != "" {
			sid = res.SessionID
			ts.SessionID = sid
		}
		if res.Status == adapter.StatusSucceeded || res.Status == adapter.StatusInterrupted {
			break
		}
		lastErr = res.ErrMsg
		if lastErr == "" {
			lastErr = res.Summary
		}
		if ctx.Err() != nil {
			break // parent cancelled; don't burn retries
		}
	}

	var changed []string
	if snap != nil {
		changed, _ = snap.ChangedSince()
	}
	if len(t.FileScope) > 0 && len(changed) > 0 {
		if violations := plan.ScopeViolations(changed, t.FileScope); len(violations) > 0 {
			ts.ScopeViolations = violations
			switch o.Cfg.Defaults.ScopeViolation {
			case "fail":
				if final.Status == adapter.StatusSucceeded {
					final.Status = adapter.StatusFailed
					final.ErrMsg = "modified files outside file_scope: " + strings.Join(violations, ", ")
				}
				o.Console.Errorf("task %s violated its file scope (policy=fail): %s", t.ID, strings.Join(violations, ", "))
			default:
				o.Console.Warnf("task %s touched files outside its file_scope: %s", t.ID, strings.Join(violations, ", "))
			}
		}
	}

	ts.Status = final.Status
	ts.Summary = condenseResult(final, lastText, changed)
	ts.LastError = final.ErrMsg
	ts.ChangedFiles = changed
	ts.EndedAt = time.Now()
	_ = o.Store.SaveTaskState(ts)

	if final.Status == adapter.StatusSucceeded {
		bb.TaskNotes[t.ID] = state.TaskNote{
			ID: t.ID, Title: t.Title, Role: t.Role,
			Summary: ts.Summary, ChangedFiles: changed,
		}
		_ = o.Store.SaveBlackboard(bb)
	}
	return ts
}

// taskPrompt composes the blackboard-backed prompt for one task.
func (o *Orchestrator) taskPrompt(t *plan.Task, role *config.Role, bb *state.Blackboard) string {
	scope := t.FileScope
	if len(scope) == 0 {
		scope = role.FileScope
	}
	var notes []DepNote
	for _, d := range t.DependsOn {
		if n, ok := bb.TaskNotes[d]; ok {
			notes = append(notes, DepNote{
				ID: n.ID, Title: n.Title, Role: n.Role,
				Summary:      n.Summary,
				ChangedFiles: n.ChangedFiles,
			})
		}
	}
	prompt, err := BuildTaskPrompt(PromptData{
		RoleName:         t.Role,
		RoleSystemPrompt: role.SystemPrompt,
		Goal:             bb.Goal,
		Architecture:     clip(bb.Architecture, maxArchitectureRunes),
		TaskID:           t.ID,
		Title:            t.Title,
		Description:      t.Description,
		Acceptance:       t.Acceptance,
		FileScope:        scope,
		DepNotes:         notes,
	})
	if err != nil { // template bugs should never kill a run
		return t.Description
	}
	return prompt
}

// drain consumes one adapter run: renders to the console, persists events,
// accumulates agent text, and returns the final result.
func (o *Orchestrator) drain(ch <-chan adapter.Event) (*adapter.TaskResult, string) {
	var final *adapter.TaskResult
	var sb strings.Builder
	for e := range ch {
		o.Console.Handle(e)
		o.Store.AppendEvent(e)
		if e.Kind == adapter.EvAgentText {
			sb.WriteString(e.Text)
			sb.WriteString("\n")
		}
		if e.Kind == adapter.EvResult {
			final = e.Result
		}
	}
	if final == nil {
		final = &adapter.TaskResult{Status: adapter.StatusFailed, ErrMsg: "adapter closed without a result event"}
	}
	return final, sb.String()
}

// checkTaskRoles verifies every task's role has a usable adapter up front.
func (o *Orchestrator) checkTaskRoles(ctx context.Context, pl *plan.Plan) error {
	probed := map[string]bool{}
	for _, t := range pl.Tasks {
		role, ok := o.Cfg.Roles[t.Role]
		if !ok {
			return fmt.Errorf("task %s uses role %q which is not in the config", t.ID, t.Role)
		}
		ad, ok := o.Registry[role.CLI]
		if !ok {
			return fmt.Errorf("task %s needs cli %q (role %s) but no adapter is available yet; point the role at an available CLI", t.ID, role.CLI, t.Role)
		}
		if probed[role.CLI] {
			continue
		}
		if pr := ad.Probe(ctx); !pr.Found {
			return fmt.Errorf("cli %q (role %s) not found in PATH - run 'ccd doctor'", role.CLI, t.Role)
		}
		probed[role.CLI] = true
	}
	return nil
}

func (o *Orchestrator) plannerRoleName() string {
	if n := o.Cfg.Defaults.PlannerRole; n != "" {
		return n
	}
	return "architect"
}

func (o *Orchestrator) builderRoleName() string {
	if n := o.Cfg.Defaults.BuilderRole; n != "" {
		return n
	}
	return o.plannerRoleName()
}

func (o *Orchestrator) roleSet() map[string]bool {
	m := map[string]bool{}
	for n := range o.Cfg.Roles {
		m[n] = true
	}
	return m
}

func (o *Orchestrator) finishMeta(meta *state.Meta, status string) {
	now := time.Now()
	meta.Status = status
	meta.EndedAt = &now
	_ = o.Store.SaveMeta(meta)
}

// askConfirm reads the y/N answer for the --confirm gate.
func (o *Orchestrator) askConfirm(r io.Reader) bool {
	if r == nil {
		return false
	}
	o.Console.Printf("")
	o.Console.Printf("proceed with this plan? [y/N] (plan file: %s)", o.Store.PlanPath())
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	default:
		return false
	}
}

func loadPlanFile(path string) (*plan.Plan, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read plan file: %w", err)
	}
	return plan.Parse(string(b))
}

func bulletize(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	return b.String()
}
