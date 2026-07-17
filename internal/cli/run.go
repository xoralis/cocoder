package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/xoralis/cocoder/internal/orchestrator"
	"github.com/xoralis/cocoder/internal/state"
)

var (
	runConfirm bool
	runPlan    string
	runBudget  float64
	runTimeout time.Duration
	runDegrade bool
)

var runCmd = &cobra.Command{
	Use:   "run <requirement...>",
	Short: "Decompose a requirement into tasks and run them by role",
	Args:  cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		goal := strings.Join(args, " ")
		if goal == "" && runPlan == "" {
			return fmt.Errorf("provide a requirement (or --plan <file>)")
		}
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		store, err := state.CreateRun(wd)
		if err != nil {
			return err
		}
		defer store.Close()

		orch, err := newOrchestrator(store)
		if err != nil {
			return err
		}
		orch.Console.Printf("run %s | goal: %s", store.RunID(), truncateRunes(goal, 100))

		ctx, cancel := withInterrupt(context.Background())
		defer cancel()

		return orch.Run(ctx, orchestrator.RunOptions{
			Goal:      goal,
			Confirm:   runConfirm,
			PlanFile:  runPlan,
			BudgetUSD: runBudget,
			Timeout:   runTimeout,
			Degrade:   runDegrade,
			Stdin:     os.Stdin,
		})
	},
}

func init() {
	f := runCmd.Flags()
	f.BoolVar(&runConfirm, "confirm", false, "pause to review the plan before executing")
	f.StringVar(&runPlan, "plan", "", "run a pre-written plan.json instead of invoking the architect")
	f.Float64Var(&runBudget, "budget", 0, "stop the run once total reported cost exceeds this many USD")
	f.DurationVar(&runTimeout, "timeout", 0, "per-task timeout override")
	f.BoolVar(&runDegrade, "degrade", false, "if planning fails, fall back to a single builder-role task")
}
