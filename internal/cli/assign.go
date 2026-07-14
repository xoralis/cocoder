package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/execx"
	"github.com/xoralis/cocoder/internal/gitx"
	"github.com/xoralis/cocoder/internal/orchestrator"
	"github.com/xoralis/cocoder/internal/state"
	"github.com/xoralis/cocoder/internal/ui"
)

var (
	assignScope   []string
	assignTimeout time.Duration
)

var assignCmd = &cobra.Command{
	Use:   "assign <role> <task...>",
	Short: "Run a single task with one role's CLI (bypasses the planner)",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		roleName := args[0]
		task := strings.Join(args[1:], " ")
		return runAssign(roleName, task)
	},
}

func init() {
	assignCmd.Flags().StringSliceVar(&assignScope, "scope", nil,
		"file scope the task may modify (comma-separated path prefixes, overrides the role default)")
	assignCmd.Flags().DurationVar(&assignTimeout, "timeout", 0,
		"task timeout (overrides role/defaults)")
}

func runAssign(roleName, task string) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	role, ok := cfg.Roles[roleName]
	if !ok {
		return fmt.Errorf("unknown role %q; defined roles: %s", roleName, strings.Join(cfg.RoleNames(), ", "))
	}

	registry, err := adapter.BuildRegistry(cfg, execx.NewOSRunner())
	if err != nil {
		return err
	}
	ad, ok := registry[role.CLI]
	if !ok {
		var avail []string
		for n := range registry {
			avail = append(avail, n)
		}
		sort.Strings(avail)
		return fmt.Errorf("no adapter implemented yet for cli %q (available: %s); point roles.%s.cli at an available CLI for now",
			role.CLI, strings.Join(avail, ", "), roleName)
	}

	console := ui.NewConsole(color.Output, flagVerbose)

	// Fail fast if the CLI is not actually installed.
	pr := ad.Probe(context.Background())
	if !pr.Found {
		return fmt.Errorf("cli %q (%s) not found in PATH - run 'ccd doctor'", role.CLI, roleName)
	}
	if pr.Warn != "" {
		console.Warnf("%s: %s", role.CLI, pr.Warn)
	}

	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	var snap *gitx.Snapshot
	if gitx.IsRepo(wd) {
		if snap, err = gitx.Take(wd); err != nil {
			console.Warnf("git snapshot failed: %v (changed-file reporting disabled)", err)
			snap = nil
		}
	} else {
		console.Warnf("not a git repository - changed-file reporting disabled")
	}

	store, err := state.CreateRun(wd)
	if err != nil {
		return err
	}
	defer store.Close()

	meta := &state.Meta{
		RunID:      store.RunID(),
		Mode:       "assign",
		Goal:       task,
		Status:     "running",
		CcdVersion: Version,
		CreatedAt:  time.Now(),
	}
	if snap != nil {
		meta.StartHead = snap.Head
	}
	if err := store.SaveMeta(meta); err != nil {
		return err
	}

	scope := assignScope
	if len(scope) == 0 {
		scope = role.FileScope
	}
	const taskID = "a1"
	prompt, err := orchestrator.BuildTaskPrompt(orchestrator.PromptData{
		RoleName:         roleName,
		RoleSystemPrompt: role.SystemPrompt,
		Goal:             task,
		TaskID:           taskID,
		Title:            truncateRunes(task, 80),
		Description:      task,
		FileScope:        scope,
	})
	if err != nil {
		return err
	}

	timeout := assignTimeout
	if timeout == 0 {
		timeout = role.Timeout.D()
	}
	if timeout == 0 {
		timeout = cfg.Defaults.TaskTimeout.D()
	}
	ctx, cancel := withInterrupt(context.Background())
	defer cancel()
	taskCtx, tcancel := context.WithTimeout(ctx, timeout)
	defer tcancel()

	logW, err := store.TaskLog(taskID)
	if err != nil {
		return err
	}
	defer logW.Close()

	console.Printf("run %s | %s -> %s | timeout %s | log %s",
		store.RunID(), roleName, role.CLI, timeout, store.TaskLogPath(taskID))

	ts := &state.TaskState{
		ID:        taskID,
		Role:      roleName,
		Title:     truncateRunes(task, 80),
		Status:    adapter.StatusRunning,
		Attempts:  1,
		StartedAt: time.Now(),
	}
	_ = store.SaveTaskState(ts)

	started := adapter.Event{
		Kind: adapter.EvTaskStarted, TaskID: taskID, Role: roleName,
		Time: time.Now(), Text: truncateRunes(task, 120),
	}
	console.Handle(started)
	store.AppendEvent(started)

	ch, err := ad.Run(taskCtx, adapter.TaskInput{
		TaskID:     taskID,
		Role:       roleName,
		Prompt:     prompt,
		WorkDir:    wd,
		Model:      role.Model,
		Permission: role.Permission,
		ExtraArgs:  role.ExtraArgs,
		RawLog:     logW,
	})
	if err != nil {
		ts.Status = adapter.StatusFailed
		ts.LastError = err.Error()
		ts.EndedAt = time.Now()
		_ = store.SaveTaskState(ts)
		finishMeta(store, meta, "failed")
		return err
	}

	var final *adapter.TaskResult
	for e := range ch {
		console.Handle(e)
		store.AppendEvent(e)
		if e.Kind == adapter.EvResult {
			final = e.Result
		}
	}
	if final == nil { // channel-contract violation; be defensive
		final = &adapter.TaskResult{Status: adapter.StatusFailed, ErrMsg: "adapter closed without a result event"}
	}

	ts.Status = final.Status
	ts.SessionID = final.SessionID
	ts.Summary = final.Summary
	ts.CostUSD = final.CostUSD
	ts.NumTurns = final.NumTurns
	ts.LastError = final.ErrMsg
	ts.EndedAt = time.Now()
	if snap != nil {
		if changed, cerr := snap.ChangedSince(); cerr == nil {
			ts.ChangedFiles = changed
		}
	}
	_ = store.SaveTaskState(ts)

	dur := ts.EndedAt.Sub(ts.StartedAt).Round(time.Second)
	console.Printf("")
	switch final.Status {
	case adapter.StatusSucceeded:
		finishMeta(store, meta, "completed")
		console.Successf("task %s succeeded in %s", taskID, dur)
	case adapter.StatusInterrupted:
		finishMeta(store, meta, "interrupted")
		console.Warnf("task %s interrupted after %s", taskID, dur)
	default:
		finishMeta(store, meta, "failed")
		console.Errorf("task %s failed after %s", taskID, dur)
	}
	if final.CostUSD > 0 {
		console.Printf("cost: $%.4f (%d turns)", final.CostUSD, final.NumTurns)
	}
	if final.SessionID != "" {
		console.Printf("session: %s", final.SessionID)
	}
	if len(ts.ChangedFiles) > 0 {
		console.Printf("changed files:")
		for i, f := range ts.ChangedFiles {
			if i == 20 {
				console.Printf("  ... and %d more", len(ts.ChangedFiles)-20)
				break
			}
			console.Printf("  ~ %s", f)
		}
	}

	if final.Status != adapter.StatusSucceeded {
		if final.ErrMsg != "" {
			return fmt.Errorf("task %s: %s", final.Status, truncateRunes(final.ErrMsg, 200))
		}
		return errors.New("task " + string(final.Status))
	}
	return nil
}

// finishMeta stamps the run outcome (best effort).
func finishMeta(store *state.RunStore, meta *state.Meta, status string) {
	now := time.Now()
	meta.Status = status
	meta.EndedAt = &now
	_ = store.SaveMeta(meta)
}

// truncateRunes limits s to n runes (CJK-safe).
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
