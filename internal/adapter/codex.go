package adapter

import (
	"context"
	"fmt"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// Codex drives the OpenAI Codex CLI non-interactively:
//
//	codex exec --json --skip-git-repo-check [-C dir] [-m M] [sandbox] [resume <id>] -
//
// Empirically verified against codex-cli 0.144 (2026-07):
//   - the trailing "-" reads the prompt from stdin (required on Windows:
//     prompts contain quotes/newlines that a .cmd shim rejects in argv);
//   - exec-level flags must come BEFORE the `resume <id>` subcommand;
//   - `--ask-for-approval` no longer exists; exec mode is non-interactive
//     and the sandbox level governs what is allowed.
type Codex struct {
	spec   *config.CLISpec
	runner execx.Runner
}

// NewCodex builds the Codex adapter for a resolved spec.
func NewCodex(spec *config.CLISpec, r execx.Runner) *Codex {
	return &Codex{spec: spec, runner: r}
}

func (a *Codex) Name() string { return a.spec.Name }

func (a *Codex) Caps() Capabilities {
	// Codex reports token usage but not USD cost.
	return Capabilities{Resume: true, CostReport: false, JSONEvents: true, StructuredOutput: true}
}

func (a *Codex) Probe(ctx context.Context) ProbeResult { return ProbeSpec(ctx, a.spec) }

func (a *Codex) buildArgs(in TaskInput, schemaFile string) []string {
	args := []string{"exec"}
	if a.spec.Output != "text" {
		args = append(args, "--json")
	}
	args = append(args, "--skip-git-repo-check")
	if in.WorkDir != "" {
		args = append(args, "-C", in.WorkDir)
	}
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	args = append(args, codexSandboxArgs(a.spec, in.Permission)...)
	if schemaFile != "" {
		args = append(args, "--output-schema", schemaFile)
	}
	args = append(args, a.spec.ExtraArgs...)
	args = append(args, in.ExtraArgs...)
	if in.ResumeSessionID != "" {
		args = append(args, "resume", in.ResumeSessionID)
	}
	args = append(args, "-") // prompt from stdin
	return args
}

// codexSandboxArgs maps the coarse permission level to codex sandbox flags.
func codexSandboxArgs(spec *config.CLISpec, p config.Permission) []string {
	if spec.PermissionArgs != nil {
		if a, ok := spec.PermissionArgs[string(p)]; ok {
			return a
		}
	}
	switch p {
	case config.PermReadOnly:
		return []string{"-s", "read-only"}
	case config.PermFull:
		return []string{"--dangerously-bypass-approvals-and-sandbox"}
	default: // PermEdits
		return []string{"-s", "workspace-write"}
	}
}

// Run spawns codex for one task. See Adapter.Run for the channel contract.
func (a *Codex) Run(ctx context.Context, in TaskInput) (<-chan Event, error) {
	// Structured output goes through a temp schema file (codex takes a
	// path, not an inline schema).
	schemaFile, cleanup, err := writeTempFile("ccd-schema-*.json", in.JSONSchema)
	if err != nil {
		return nil, err
	}
	spec := execx.Spec{
		Command: a.spec.Command,
		Args:    a.buildArgs(in, schemaFile),
		Dir:     in.WorkDir,
		Stdin:   in.Prompt,
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

func (a *Codex) parser() streamParser {
	if a.spec.Output == "text" {
		return parseTextStream
	}
	return parseCodexStream
}
