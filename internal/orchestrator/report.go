package orchestrator

import (
	"bytes"
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/xoralis/cocoder/internal/gitx"
	"github.com/xoralis/cocoder/internal/plan"
	"github.com/xoralis/cocoder/internal/state"
)

// printPlanTable shows the plan before execution (and for --confirm).
func (o *Orchestrator) printPlanTable(pl *plan.Plan, order []string) {
	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tROLE\tDEPS\tSCOPE\tTITLE")
	for _, id := range order {
		t := pl.Task(id)
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			t.ID, t.Role,
			strings.Join(t.DependsOn, ","),
			strings.Join(t.FileScope, ","),
			clip(t.Title, 60))
	}
	tw.Flush()
	o.Console.Printf("")
	o.Console.Printf("%s", strings.TrimRight(buf.String(), "\n"))
}

// report prints the end-of-run summary: task table, git evidence, costs.
// This is the human review gate - ccd never commits.
func (o *Orchestrator) report(meta *state.Meta, pl *plan.Plan, order []string,
	states map[string]*state.TaskState, snapStart *gitx.Snapshot,
	totalCost, budget float64, stopReason string) {

	c := o.Console
	c.Printf("")
	c.Printf("=== run %s: %s ===", meta.RunID, meta.Status)
	if stopReason != "" {
		c.Warnf("%s", stopReason)
	}

	var buf bytes.Buffer
	tw := tabwriter.NewWriter(&buf, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TASK\tROLE\tSTATUS\tDUR\tCOST\tTITLE")
	for _, id := range order {
		t := pl.Task(id)
		ts := states[id]
		if ts == nil {
			fmt.Fprintf(tw, "%s\t%s\tpending\t-\t-\t%s\n", t.ID, t.Role, clip(t.Title, 60))
			continue
		}
		dur := "-"
		if !ts.StartedAt.IsZero() && !ts.EndedAt.IsZero() {
			dur = ts.EndedAt.Sub(ts.StartedAt).Round(time.Second).String()
		}
		cost := "-"
		if ts.CostUSD > 0 {
			cost = fmt.Sprintf("$%.4f", ts.CostUSD)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Role, ts.Status, dur, cost, clip(t.Title, 60))
	}
	tw.Flush()
	c.Printf("%s", strings.TrimRight(buf.String(), "\n"))

	// Scope violations recap.
	for _, id := range order {
		if ts := states[id]; ts != nil && len(ts.ScopeViolations) > 0 {
			c.Warnf("task %s wrote outside its file_scope: %s", id, strings.Join(ts.ScopeViolations, ", "))
		}
	}

	// Git evidence for the human review gate.
	if snapStart != nil {
		if diff := gitx.DiffStat(o.WorkDir, snapStart.Head, 30); diff != "" {
			c.Printf("")
			c.Printf("git diff --stat (since run start):")
			c.Printf("%s", diff)
		}
		if untracked := gitx.Untracked(o.WorkDir); len(untracked) > 0 {
			c.Printf("")
			c.Printf("new files (untracked):")
			for i, f := range untracked {
				if i == 20 {
					c.Printf("  ... and %d more", len(untracked)-20)
					break
				}
				c.Printf("  + %s", f)
			}
		}
	}

	c.Printf("")
	costLine := fmt.Sprintf("total cost: $%.4f (only CLIs that report cost are counted)", totalCost)
	if budget > 0 {
		costLine += fmt.Sprintf(", budget $%.2f", budget)
	}
	c.Printf("%s", costLine)
	c.Printf("next: review the diff, then commit yourself. details: ccd status | ccd logs <task-id>")
}
