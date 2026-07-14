package adapter

import (
	"context"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// Capabilities declares what a CLI adapter can do.
type Capabilities struct {
	Resume           bool // can continue a stored session
	CostReport       bool // reports cost in USD
	JSONEvents       bool // machine-readable event stream (vs plain text)
	StructuredOutput bool // can enforce a JSON schema on the final output
}

// Adapter drives one coding CLI as a headless subprocess.
type Adapter interface {
	Name() string
	Caps() Capabilities
	Probe(ctx context.Context) ProbeResult
	// Run spawns the CLI for one task. It returns immediately after a
	// successful spawn; the returned error covers spawn failures only.
	//
	// Channel contract: exactly one EvResult event is delivered last, then
	// the channel closes. Cancelling ctx kills the process tree and yields
	// a Result with StatusInterrupted (context.Canceled) or StatusFailed
	// (context.DeadlineExceeded).
	Run(ctx context.Context, in TaskInput) (<-chan Event, error)
}

// BuildRegistry constructs adapters for every CLI referenced by the config.
// CLIs whose adapter kind is not implemented yet are silently absent from
// the registry; callers report a clear error at use time.
func BuildRegistry(cfg *config.Config, r execx.Runner) (map[string]Adapter, error) {
	reg := map[string]Adapter{}
	for _, name := range cfg.CLINames() {
		spec, err := cfg.ResolveCLI(name)
		if err != nil {
			return nil, err
		}
		switch spec.Adapter {
		case "claude":
			reg[name] = NewClaude(spec, r)
		default:
			// codex / gemini / generic adapters land in M3.
		}
	}
	return reg, nil
}
