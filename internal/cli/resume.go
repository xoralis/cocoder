package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
	"github.com/xoralis/cocoder/internal/orchestrator"
	"github.com/xoralis/cocoder/internal/state"
	"github.com/xoralis/cocoder/internal/ui"
)

// newOrchestrator assembles the orchestrator stack around an existing run
// store (shared by run/resume/followup).
func newOrchestrator(store *state.RunStore) (*orchestrator.Orchestrator, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	registry, err := adapter.BuildRegistry(cfg, execx.NewOSRunner())
	if err != nil {
		return nil, err
	}
	wd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &orchestrator.Orchestrator{
		Cfg:      cfg,
		Registry: registry,
		Store:    store,
		Console:  ui.NewConsole(color.Output, flagVerbose),
		WorkDir:  wd,
		Version:  Version,
	}, nil
}

var (
	resumeBudget  float64
	resumeTimeout time.Duration
	resumeDegrade bool
)

var resumeCmd = &cobra.Command{
	Use:   "resume [run-id]",
	Short: "Continue an interrupted or failed run (default: the latest unfinished)",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		wd, err := os.Getwd()
		if err != nil {
			return err
		}
		id := ""
		if len(args) > 0 {
			id = args[0]
		} else if id, err = state.LatestUnfinishedRunID(wd); err != nil {
			return err
		}
		store, err := state.OpenRun(wd, id)
		if err != nil {
			return err
		}
		defer store.Close()
		orch, err := newOrchestrator(store)
		if err != nil {
			return err
		}
		ctx, cancel := withInterrupt(context.Background())
		defer cancel()
		return orch.Resume(ctx, orchestrator.RunOptions{
			BudgetUSD: resumeBudget,
			Timeout:   resumeTimeout,
			Degrade:   resumeDegrade,
			Stdin:     os.Stdin,
		})
	},
}

var (
	followupRun     string
	followupTimeout time.Duration
)

var followupCmd = &cobra.Command{
	Use:   "followup <task-id> <instruction...>",
	Short: "Send a follow-up instruction into a task's stored CLI session",
	Args:  cobra.MinimumNArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		store, err := openRunFlag(followupRun)
		if err != nil {
			return err
		}
		defer store.Close()
		orch, err := newOrchestrator(store)
		if err != nil {
			return err
		}
		ctx, cancel := withInterrupt(context.Background())
		defer cancel()
		instruction := strings.Join(args[1:], " ")
		return orch.Followup(ctx, args[0], instruction, followupTimeout)
	},
}

var rolesCmd = &cobra.Command{
	Use:   "roles",
	Short: "List the configured roles and their CLI/permission/scope",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		printRolesTable(cfg)
		return nil
	},
}

func init() {
	resumeCmd.Flags().Float64Var(&resumeBudget, "budget", 0, "lifetime budget cap in USD (0 = config default)")
	resumeCmd.Flags().DurationVar(&resumeTimeout, "timeout", 0, "per-task timeout override")
	resumeCmd.Flags().BoolVar(&resumeDegrade, "degrade", false, "if re-planning is needed and fails, fall back to a single builder-role task")
	followupCmd.Flags().StringVar(&followupRun, "run", "", "run id (default: the latest run)")
	followupCmd.Flags().DurationVar(&followupTimeout, "timeout", 0, "timeout for the follow-up (default: role/config)")
}

func printRolesTable(cfg *config.Config) {
	tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "ROLE\tCLI\tMODEL\tPERMISSION\tSCOPE\tTIMEOUT")
	for _, name := range cfg.RoleNames() {
		r := cfg.Roles[name]
		timeout := ""
		if r.Timeout.D() > 0 {
			timeout = r.Timeout.D().String()
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			name, r.CLI, r.Model, string(r.Permission), strings.Join(r.FileScope, ","), timeout)
	}
}
