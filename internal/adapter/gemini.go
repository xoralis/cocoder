package adapter

import (
	"context"
	"fmt"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// Gemini drives the Google Gemini CLI headlessly:
//
//	gemini --output-format stream-json [-m M] [-r sid] [--approval-mode X]
//
// with the prompt piped via stdin (non-TTY stdin triggers non-interactive
// mode). NOTE: the stream-json event shapes below follow the 2026-07 docs
// but have not been verified against a live install; if they drift, set
// `clis: gemini: {output: text}` in ccd.yaml to fall back to plain-text
// parsing until the parser is updated.
type Gemini struct {
	spec   *config.CLISpec
	runner execx.Runner
}

// NewGemini builds the Gemini adapter for a resolved spec.
func NewGemini(spec *config.CLISpec, r execx.Runner) *Gemini {
	return &Gemini{spec: spec, runner: r}
}

func (a *Gemini) Name() string { return a.spec.Name }

func (a *Gemini) Caps() Capabilities {
	return Capabilities{Resume: true, CostReport: false, JSONEvents: true, StructuredOutput: false}
}

func (a *Gemini) Probe(ctx context.Context) ProbeResult { return ProbeSpec(ctx, a.spec) }

func (a *Gemini) buildArgs(in TaskInput) []string {
	var args []string
	if a.spec.Output != "text" {
		args = append(args, "--output-format", "stream-json")
	}
	if in.Model != "" {
		args = append(args, "-m", in.Model)
	}
	if in.ResumeSessionID != "" {
		args = append(args, "-r", in.ResumeSessionID)
	}
	args = append(args, geminiPermissionArgs(a.spec, in.Permission)...)
	args = append(args, a.spec.ExtraArgs...)
	args = append(args, in.ExtraArgs...)
	return args
}

// geminiPermissionArgs maps the coarse permission level to gemini approval
// modes (--approval-mode default|auto_edit|yolo|plan).
func geminiPermissionArgs(spec *config.CLISpec, p config.Permission) []string {
	if spec.PermissionArgs != nil {
		if a, ok := spec.PermissionArgs[string(p)]; ok {
			return a
		}
	}
	switch p {
	case config.PermReadOnly:
		return []string{"--approval-mode", "plan"}
	case config.PermFull:
		return []string{"--approval-mode", "yolo"}
	default: // PermEdits
		return []string{"--approval-mode", "auto_edit"}
	}
}

// Run spawns gemini for one task. See Adapter.Run for the channel contract.
func (a *Gemini) Run(ctx context.Context, in TaskInput) (<-chan Event, error) {
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
	go pumpJSONL(ctx, in, proc, ch, a.parser())
	return ch, nil
}

func (a *Gemini) parser() streamParser {
	if a.spec.Output == "text" {
		return parseTextStream
	}
	return parseGeminiStream
}
