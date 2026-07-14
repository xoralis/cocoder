package adapter

import (
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/xoralis/cocoder/internal/config"
)

// ProbeResult reports whether a CLI is installed and responsive.
type ProbeResult struct {
	Found   bool
	Path    string
	Version string
	Warn    string
}

// ProbeSpec checks that spec.Command resolves on PATH and answers a
// version probe within 5 seconds. Used by `ccd doctor` and by adapters'
// Probe methods.
func ProbeSpec(ctx context.Context, spec *config.CLISpec) ProbeResult {
	path, err := exec.LookPath(spec.Command)
	if err != nil {
		return ProbeResult{Warn: "not found in PATH"}
	}
	pr := ProbeResult{Found: true, Path: path}
	vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(vctx, path, spec.VersionArgs...).CombinedOutput()
	if err != nil {
		pr.Warn = appendWarn(pr.Warn, "version probe failed")
	} else {
		line := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
		if len(line) > 60 {
			line = line[:60]
		}
		pr.Version = line
	}
	if spec.Unverified {
		pr.Warn = appendWarn(pr.Warn, "headless interface unverified")
	}
	return pr
}

func appendWarn(existing, add string) string {
	if existing == "" {
		return add
	}
	return existing + "; " + add
}
