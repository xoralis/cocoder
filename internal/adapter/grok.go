package adapter

import (
	"context"
	"fmt"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// Grok drives xAI's official Grok CLI (grok-build) headlessly:
//
//	grok --output-format streaming-json --prompt-file <tmp> [-m M] [-r sid] [permission]
//
// Empirically verified against grok 0.2.93 (2026-07):
//   - --prompt-file runs one single-turn headless prompt (avoids both the
//     ~32K argv limit and cmd-shim metacharacter issues; grok.exe is a
//     native binary but the file channel is robust everywhere);
//   - streaming-json emits {"type":"thought"|"text","data":...} token
//     deltas and a terminal {"type":"end","stopReason":...,"sessionId":...};
//     tool calls are NOT surfaced in this format (file changes are picked
//     up by ccd's git snapshot instead);
//   - --permission-mode takes the same values as Claude Code.
type Grok struct {
	spec   *config.CLISpec
	runner execx.Runner
}

// NewGrok builds the Grok adapter for a resolved spec.
func NewGrok(spec *config.CLISpec, r execx.Runner) *Grok {
	return &Grok{spec: spec, runner: r}
}

func (a *Grok) Name() string { return a.spec.Name }

func (a *Grok) Caps() Capabilities {
	// --json-schema exists but switches the output format away from
	// streaming-json; structured output support is deferred until that
	// format is mapped.
	return Capabilities{Resume: true, CostReport: false, JSONEvents: true, StructuredOutput: false}
}

func (a *Grok) Probe(ctx context.Context) ProbeResult { return ProbeSpec(ctx, a.spec) }

func (a *Grok) buildArgs(in TaskInput, promptFile string) []string {
	var args []string
	if a.spec.Output != "text" {
		args = append(args, "--output-format", "streaming-json")
	}
	args = append(args, "--prompt-file", promptFile)
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	if in.ResumeSessionID != "" {
		args = append(args, "-r", in.ResumeSessionID)
	}
	args = append(args, grokPermissionArgs(a.spec, in.Permission)...)
	args = append(args, a.spec.ExtraArgs...)
	args = append(args, in.ExtraArgs...)
	return args
}

// grokPermissionArgs maps the coarse permission level to grok's
// Claude-Code-compatible permission modes.
func grokPermissionArgs(spec *config.CLISpec, p config.Permission) []string {
	if spec.PermissionArgs != nil {
		if a, ok := spec.PermissionArgs[string(p)]; ok {
			return a
		}
	}
	switch p {
	case config.PermReadOnly:
		return []string{"--permission-mode", "plan"}
	case config.PermFull:
		return []string{"--permission-mode", "bypassPermissions"}
	default: // PermEdits
		return []string{"--permission-mode", "acceptEdits"}
	}
}

// Run spawns grok for one task. See Adapter.Run for the channel contract.
func (a *Grok) Run(ctx context.Context, in TaskInput) (<-chan Event, error) {
	promptFile, cleanup, err := writeTempFile("ccd-prompt-*.md", in.Prompt)
	if err != nil {
		return nil, err
	}
	if promptFile == "" {
		cleanup()
		return nil, fmt.Errorf("grok: empty prompt")
	}
	spec := execx.Spec{
		Command: a.spec.Command,
		Args:    a.buildArgs(in, promptFile),
		Dir:     in.WorkDir,
	}
	proc, err := a.runner.Start(ctx, spec)
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("spawn %s: %w", a.spec.Command, err)
	}
	ch := make(chan Event, 64)
	go func() {
		defer cleanup()
		pumpJSONL(ctx, in, proc, ch, a.parser())
	}()
	return ch, nil
}

func (a *Grok) parser() streamParser {
	if a.spec.Output == "text" {
		return parseTextStream
	}
	return parseGrokStream
}
