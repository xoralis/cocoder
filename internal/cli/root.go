// Package cli wires the cobra command surface. Commands only parse flags
// and assemble internal packages; business logic lives below.
package cli

import (
	"fmt"
	"os"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/xoralis/cocoder/internal/config"
)

// Version is the ccd version reported by --version.
const Version = "0.1.0"

var (
	flagConfig  string
	flagChdir   string
	flagVerbose bool
	flagNoColor bool
)

var rootCmd = &cobra.Command{
	Use:   "ccd",
	Short: "cocoder - orchestrate multiple AI coding CLIs by role",
	Long: `cocoder (ccd) coordinates AI coding CLIs (Claude Code, Codex, Gemini, ...)
as a role-based team: an architect role decomposes a requirement into tasks,
and each task is dispatched to the CLI you assigned to that role.

Start with:
  ccd init      generate ccd.yaml
  ccd doctor    verify the configured CLIs are installed
  ccd assign    run a single task with one role`,
	Version:      Version,
	SilenceUsage: true,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if flagChdir != "" {
			if err := os.Chdir(flagChdir); err != nil {
				return fmt.Errorf("chdir: %w", err)
			}
		}
		if flagNoColor || os.Getenv("NO_COLOR") != "" {
			color.NoColor = true
		}
		return nil
	},
}

func init() {
	pf := rootCmd.PersistentFlags()
	pf.StringVarP(&flagConfig, "config", "c", "ccd.yaml", "path to the config file")
	pf.StringVarP(&flagChdir, "chdir", "C", "", "change working directory before doing anything")
	pf.BoolVar(&flagVerbose, "verbose", false, "show agent stderr and extra detail")
	pf.BoolVar(&flagNoColor, "no-color", false, "disable colored output")
	rootCmd.CompletionOptions.DisableDefaultCmd = true
	rootCmd.AddCommand(initCmd, doctorCmd, assignCmd)
}

// Execute runs the root command; errors exit with code 1.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

// loadConfig loads the config file with a friendly hint when missing.
func loadConfig() (*config.Config, error) {
	cfg, err := config.Load(flagConfig)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("config %s not found - run 'ccd init' first", flagConfig)
		}
		return nil, err
	}
	return cfg, nil
}
