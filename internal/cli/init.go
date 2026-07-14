package cli

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/xoralis/cocoder/internal/config"
)

var initForce bool

var initCmd = &cobra.Command{
	Use:   "init",
	Short: "Generate a ccd.yaml config template in the current directory",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := flagConfig
		if _, err := os.Stat(path); err == nil && !initForce {
			return fmt.Errorf("%s already exists (use --force to overwrite)", path)
		}
		if err := os.WriteFile(path, []byte(config.TemplateYAML()), 0o644); err != nil {
			return err
		}
		ensureGitignoreEntry(".ccd/")
		fmt.Printf("wrote %s\n\nnext steps:\n", path)
		fmt.Println("  1. edit roles in " + path + " (which CLI plays which role)")
		fmt.Println("  2. ccd doctor                      # verify the CLIs are installed")
		fmt.Println("  3. ccd assign backend \"task...\"    # run a single task")
		return nil
	},
}

func init() {
	initCmd.Flags().BoolVar(&initForce, "force", false, "overwrite an existing config file")
}

// ensureGitignoreEntry appends entry to .gitignore (creating it if needed),
// idempotently. Best effort: failures are silent.
func ensureGitignoreEntry(entry string) {
	const gi = ".gitignore"
	data, err := os.ReadFile(gi)
	if err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.TrimSpace(line) == entry {
				return
			}
		}
	}
	f, err := os.OpenFile(gi, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	prefix := ""
	if len(data) > 0 && !strings.HasSuffix(string(data), "\n") {
		prefix = "\n"
	}
	fmt.Fprintf(f, "%s%s\n", prefix, entry)
}
