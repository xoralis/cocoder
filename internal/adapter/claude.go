package adapter

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// argvSafeCommand reports whether the command resolves to something other
// than a .cmd/.bat shim, i.e. whether argv may safely contain cmd
// metacharacters (quotes, braces, %).
func argvSafeCommand(command string) bool {
	path, err := exec.LookPath(command)
	if err != nil {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	return ext != ".cmd" && ext != ".bat"
}

// Claude drives the Claude Code CLI in headless print mode:
//
//	claude -p --output-format stream-json --verbose   (prompt via stdin)
type Claude struct {
	spec   *config.CLISpec
	runner execx.Runner
}

// NewClaude builds the Claude Code adapter for the given resolved spec.
func NewClaude(spec *config.CLISpec, r execx.Runner) *Claude {
	return &Claude{spec: spec, runner: r}
}

func (a *Claude) Name() string { return a.spec.Name }

func (a *Claude) Caps() Capabilities {
	return Capabilities{Resume: true, CostReport: true, JSONEvents: true, StructuredOutput: true}
}

func (a *Claude) Probe(ctx context.Context) ProbeResult { return ProbeSpec(ctx, a.spec) }

func (a *Claude) buildArgs(in TaskInput) []string {
	// The prompt travels via stdin, never argv (Windows .cmd shim rejects
	// cmd metacharacters in argv; see package execx docs). Same reason the
	// role system prompt is folded into the prompt body upstream instead of
	// using --append-system-prompt.
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if a.spec.Bare {
		args = append(args, "--bare")
	}
	if in.Model != "" {
		args = append(args, "--model", in.Model)
	}
	if in.ResumeSessionID != "" {
		args = append(args, "--resume", in.ResumeSessionID)
	}
	if in.JSONSchema != "" && argvSafeCommand(a.spec.Command) {
		// The schema contains quotes/braces, which os/exec rejects in the
		// argv of .cmd/.bat shims (CVE-2024-24576); only pass it when the
		// command resolves to a real executable. Callers fall back to
		// fence extraction otherwise.
		args = append(args, "--json-schema", in.JSONSchema)
	}
	args = append(args, claudePermissionArgs(a.spec, in.Permission)...)
	args = append(args, a.spec.ExtraArgs...)
	args = append(args, in.ExtraArgs...)
	return args
}

// claudePermissionArgs maps the coarse permission level to Claude Code
// flags. In -p mode unapproved tools are auto-denied (no hanging prompts).
// read-only pairs the allowlist with an explicit --disallowedTools so it
// stays read-only even when the user's global claude config is permissive
// (e.g. bypassPermissions as defaultMode).
func claudePermissionArgs(spec *config.CLISpec, p config.Permission) []string {
	if spec.PermissionArgs != nil {
		if a, ok := spec.PermissionArgs[string(p)]; ok {
			return a
		}
	}
	switch p {
	case config.PermReadOnly:
		// dontAsk denies anything not explicitly allowed, overriding a
		// permissive user-global defaultMode (e.g. bypassPermissions);
		// the disallow list is belt-and-suspenders for write tools.
		return []string{
			"--permission-mode", "dontAsk",
			"--allowedTools", "Read,Grep,Glob,WebFetch,WebSearch,TodoWrite",
			"--disallowedTools", "Bash,Edit,Write,MultiEdit,NotebookEdit",
		}
	case config.PermFull:
		return []string{"--dangerously-skip-permissions"}
	default: // PermEdits
		return []string{"--permission-mode", "acceptEdits"}
	}
}

// Run spawns claude for one task. See Adapter.Run for the channel contract.
func (a *Claude) Run(ctx context.Context, in TaskInput) (<-chan Event, error) {
	spec := execx.Spec{
		Command: a.spec.Command,
		Args:    a.buildArgs(in),
		Dir:     in.WorkDir,
		Stdin:   in.Prompt,
	}
	proc, err := a.runner.Start(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", a.spec.Command, err)
	}
	ch := make(chan Event, 64)
	go pumpJSONL(ctx, in, proc, ch, parseClaudeStream)
	return ch, nil
}
