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

// RunOptions parameterizes one `ccd run`.
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
	var snapStart *gitx.Snapshot
	if gitx.IsRepo(o.WorkDir) {
		if s, err := gitx.Take(o.WorkDir); err == nil {
			snapStart = s
			meta.StartHead = s.Head
		}
	} else {
		o.Console.Warnf("not a git repository - diffs and scope checks are disabled")
	}
	if err := o.Store.SaveMeta(meta); err != nil {
		return err
	}

	totalCost := 0.0
	budget := opts.BudgetUSD
	if budget == 0 {
		budget = o.Cfg.Defaults.BudgetUSD
	}

	// ---- 1. Acquire the plan --------------------------------------------
	pl, plannerCost, err := o.acquirePlan(ctx, opts)
	totalCost += plannerCost
	if err != nil {
		o.finishMeta(meta, "failed")
		return err
	}
	if pl.Goal == "" {
		pl.Goal = opts.Goal
	}
	if meta.Goal == "" {
		meta.Goal = pl.Goal
		_ = o.Store.SaveMeta(meta)
	}
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
			o.Console.Printf("aborted. edit the plan at %s and rerun with:\n  ccd run --plan %s",
				o.Store.PlanPath(), o.Store.PlanPath())
			return nil
		}
	}

	// ---- 3. Execute (serial main loop, single writer) --------------------
	sched := newScheduler(pl, order, nil)
	states := map[string]*state.TaskState{}
	stopReason := ""

	for {
		if ctx.Err() != nil {
			meta.Status = "interrupted"
			stopReason = "interrupted - resume support lands in M4; rerun with --plan to redo remaining tasks"
			break
		}
		t := sched.nextReady()
		if t == nil {
			break
		}
		if budget > 0 && totalCost >= budget {
			meta.Status = "interrupted"
			stopReason = fmt.Sprintf("budget $%.2f reached (spent $%.4f) - stopped before task %s", budget, totalCost, t.ID)
			break
		}

		ts := o.runTask(ctx, t, bb, opts)
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
			if st == adapter.StatusPending {
				ts.Status = adapter.StatusPending
			}
			_ = o.Store.SaveTaskState(ts)
			states[id] = ts
		}
	}

	if meta.Status == "running" {
		meta.Status = "completed"
	}
	o.finishMeta(meta, meta.Status)

	// ---- 4. Report --------------------------------------------------------
	o.report(meta, pl, order, states, snapStart, totalCost, budget, stopReason)

	switch meta.Status {
	case "completed":
		return nil
	case "interrupted":
		return errors.New("run interrupted")
	default:
		return errors.New("run failed - see the report above; logs: ccd logs <task-id>")
	}
}

// acquirePlan loads --plan or invokes the architect (with --degrade
// fallback).
func (o *Orchestrator) acquirePlan(ctx context.Context, opts RunOptions) (*plan.Plan, float64, error) {
	if opts.PlanFile != "" {
		pl, err := loadPlanFile(opts.PlanFile)
		if err != nil {
			return nil, 0, err
		}
		if errs := pl.Validate(o.roleSet()); len(errs) > 0 {
			return nil, 0, fmt.Errorf("plan file %s is invalid:\n%s", opts.PlanFile, bulletize(errs))
		}
		o.Console.Printf("using plan from %s (planner skipped)", opts.PlanFile)
		return pl, 0, nil
	}

	roleName := o.plannerRoleName()
	role, ok := o.Cfg.Roles[roleName]
	if !ok {
		return nil, 0, fmt.Errorf("planner role %q is not defined in the config (set defaults.planner_role or add the role)", roleName)
	}
	ad, ok := o.Registry[role.CLI]
	if !ok {
		return nil, 0, fmt.Errorf("no adapter available for the planner role's cli %q", role.CLI)
	}
	if pr := ad.Probe(ctx); !pr.Found {
		return nil, 0, fmt.Errorf("planner cli %q not found in PATH - run 'ccd doctor'", role.CLI)
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
		return o.degradedPlan(opts.Goal), stats.CostUSD, nil
	}
	if err != nil {
		return nil, stats.CostUSD, err
	}
	o.Console.Printf("plan ready: %d task(s), planner cost $%.4f (%d attempt(s))",
		len(pl.Tasks), stats.CostUSD, stats.Attempts)
	return pl, stats.CostUSD, nil
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
func (o *Orchestrator) runTask(ctx context.Context, t *plan.Task, bb *state.Blackboard, opts RunOptions) *state.TaskState {
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

	ts := &state.TaskState{ID: t.ID, Role: t.Role, Title: t.Title, Status: adapter.StatusRunning, StartedAt: time.Now()}
	_ = o.Store.SaveTaskState(ts)
	started := adapter.Event{Kind: adapter.EvTaskStarted, TaskID: t.ID, Role: t.Role, Time: time.Now(), Text: t.Title}
	o.Console.Handle(started)
	o.Store.AppendEvent(started)

	var snap *gitx.Snapshot
	if gitx.IsRepo(o.WorkDir) {
		snap, _ = gitx.Take(o.WorkDir)
	}

	basePrompt := o.taskPrompt(t, role, bb)
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
		ts.Attempts = attempt
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
