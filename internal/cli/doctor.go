package cli

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/gitx"
	"github.com/xoralis/cocoder/internal/ui"
)

var doctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check that the configured CLIs, git and config are ready",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg, err := loadConfig()
		if err != nil {
			return err
		}
		console := ui.NewConsole(color.Output, flagVerbose)

		// Which roles use which CLI.
		rolesOf := map[string][]string{}
		for _, rn := range cfg.RoleNames() {
			r := cfg.Roles[rn]
			rolesOf[r.CLI] = append(rolesOf[r.CLI], rn)
		}
		for _, v := range rolesOf {
			sort.Strings(v)
		}

		problems := 0
		tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
		fmt.Fprintln(tw, "CLI\tSTATUS\tVERSION\tROLES\tNOTE")
		for _, name := range cfg.CLINames() {
			spec, rerr := cfg.ResolveCLI(name)
			if rerr != nil {
				fmt.Fprintf(tw, "%s\tINVALID\t\t%s\t%v\n", name, strings.Join(rolesOf[name], ","), rerr)
				problems++
				continue
			}
			pr := adapter.ProbeSpec(cmd.Context(), spec)
			status := "ok"
			if !pr.Found {
				status = "MISSING"
				if len(rolesOf[name]) > 0 {
					problems++
				}
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
				name, status, pr.Version, strings.Join(rolesOf[name], ","), pr.Warn)
		}
		tw.Flush()
		fmt.Println()

		// git and repo state.
		if _, err := exec.LookPath("git"); err != nil {
			console.Errorf("git: NOT FOUND (required)")
			problems++
		} else {
			wd, _ := os.Getwd()
			switch {
			case !gitx.IsRepo(wd):
				console.Warnf("git: ok, but current directory is not a git repository (ccd works best inside one)")
			default:
				if snap, err := gitx.Take(wd); err == nil && len(snap.Dirty) > 0 {
					console.Warnf("git: repo has %d uncommitted change(s) - runs are easier to review from a clean tree", len(snap.Dirty))
				} else {
					console.Printf("git: ok, repo clean")
				}
			}
		}
		console.Printf("config: %s valid", flagConfig)

		if problems > 0 {
			return fmt.Errorf("doctor found %d problem(s)", problems)
		}
		console.Successf("all checks passed")
		return nil
	},
}
