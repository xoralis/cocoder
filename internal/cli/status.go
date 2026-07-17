package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/xoralis/cocoder/internal/state"
)

var statusCmd = &cobra.Command{
	Use:   "status [run-id]",
	Short: "Show the task table of a run (default: the latest)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := openRunArg(args)
		if err != nil {
			return err
		}
		meta, err := store.LoadMeta()
		if err != nil {
			return fmt.Errorf("read run meta: %w", err)
		}
		states, err := store.LoadTaskStates()
		if err != nil {
			return err
		}

		fmt.Printf("run %s | mode=%s | status=%s\ngoal: %s\n\n",
			meta.RunID, meta.Mode, meta.Status, truncateRunes(meta.Goal, 120))

		// Order by plan when available, else sorted task ids.
		var order []string
		if pl, err := store.LoadPlanFile(); err == nil {
			if o, err := pl.TopoSort(); err == nil {
				order = o
			}
		}
		if order == nil {
			for id := range states {
				order = append(order, id)
			}
			sort.Strings(order)
		}

		var buf bytes.Buffer
		tw := tabwriter.NewWriter(&buf, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "TASK\tROLE\tSTATUS\tDUR\tCOST\tSESSION\tTITLE")
		total := 0.0
		for _, id := range order {
			ts := states[id]
			if ts == nil {
				continue
			}
			dur := "-"
			if !ts.StartedAt.IsZero() && !ts.EndedAt.IsZero() {
				dur = ts.EndedAt.Sub(ts.StartedAt).Round(time.Second).String()
			}
			cost := "-"
			if ts.CostUSD > 0 {
				cost = fmt.Sprintf("$%.4f", ts.CostUSD)
				total += ts.CostUSD
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
				ts.ID, ts.Role, ts.Status, dur, cost, ts.SessionID, truncateRunes(ts.Title, 50))
		}
		tw.Flush()
		fmt.Print(buf.String())
		if meta.PlannerCostUSD > 0 {
			fmt.Printf("\nplanner: $%.4f (%d attempt(s))", meta.PlannerCostUSD, meta.PlannerAttempts)
			total += meta.PlannerCostUSD
		}
		if meta.Resumes > 0 {
			fmt.Printf("\nresumed %d time(s)", meta.Resumes)
		}
		if total > 0 {
			fmt.Printf("\ntotal cost: $%.4f (only CLIs that report cost are counted)", total)
		}
		fmt.Println()
		return nil
	},
}

var (
	logsRunID  string
	logsFollow bool
)

var logsCmd = &cobra.Command{
	Use:   "logs <task-id>",
	Short: "Print the raw agent log of a task (use task id 'plan' for the planner)",
	Args:  cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := openRunFlag(logsRunID)
		if err != nil {
			return err
		}
		path := store.TaskLogPath(args[0])
		if _, err := os.Stat(path); err != nil {
			avail := availableLogs(store)
			return fmt.Errorf("no log for task %q in run %s (available: %s)",
				args[0], store.RunID(), strings.Join(avail, ", "))
		}
		if logsFollow {
			return followFile(path)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		_, _ = os.Stdout.Write(b)
		return nil
	},
}

// followFile tails a log file (print existing content, then poll for
// growth) until Ctrl-C.
func followFile(path string) error {
	ctx, cancel := withInterrupt(context.Background())
	defer cancel()
	var offset int64
	for {
		f, err := os.Open(path)
		if err == nil {
			if fi, serr := f.Stat(); serr == nil {
				if fi.Size() < offset {
					offset = 0 // truncated: start over
				}
				if fi.Size() > offset {
					_, _ = f.Seek(offset, io.SeekStart)
					n, _ := io.Copy(os.Stdout, f)
					offset += n
				}
			}
			f.Close()
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func init() {
	logsCmd.Flags().StringVar(&logsRunID, "run", "", "run id (default: the latest run)")
	logsCmd.Flags().BoolVarP(&logsFollow, "follow", "f", false, "keep printing as the log grows (Ctrl-C to stop)")
}

// openRunArg opens the run named by args[0], or the latest.
func openRunArg(args []string) (*state.RunStore, error) {
	id := ""
	if len(args) > 0 {
		id = args[0]
	}
	return openRunFlag(id)
}

func openRunFlag(id string) (*state.RunStore, error) {
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	if id == "" {
		if id, err = state.LatestRunID(wd); err != nil {
			return nil, fmt.Errorf("no runs yet - start one with 'ccd run' or 'ccd assign'")
		}
	}
	return state.OpenRun(wd, id)
}

func availableLogs(store *state.RunStore) []string {
	entries, err := os.ReadDir(store.Dir + string(os.PathSeparator) + "logs")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".log"))
	}
	sort.Strings(names)
	return names
}
