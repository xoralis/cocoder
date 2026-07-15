package adapter

import (
	"context"
	"fmt"
	"strings"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

// Generic drives any CLI declared in ccd.yaml without writing Go code:
//
//	clis:
//	  mycli:
//	    adapter: generic
//	    command: mycli
//	    run_args: ["-p", "{prompt}"]          # placeholders: {prompt} {model} {workdir} {session}
//	    resume_args: ["-r", "{session}", "-p", "{prompt}"]   # optional; enables resume
//	    prompt_via: arg | stdin               # stdin strongly preferred (Windows .cmd shims
//	    output: text                          # reject metacharacters in argv)
//
// Output is parsed in plain-text mode: stdout lines become agent text and
// the exit code decides success. Tokens that expand to "" are dropped, so
// "{model}" style placeholders vanish when unset (don't pair them with a
// separate flag token).
type Generic struct {
	spec   *config.CLISpec
	runner execx.Runner
}

// NewGeneric builds the config-driven adapter for a resolved spec.
func NewGeneric(spec *config.CLISpec, r execx.Runner) *Generic {
	return &Generic{spec: spec, runner: r}
}

func (a *Generic) Name() string { return a.spec.Name }

func (a *Generic) Caps() Capabilities {
	return Capabilities{Resume: len(a.spec.ResumeArgs) > 0}
}

func (a *Generic) Probe(ctx context.Context) ProbeResult { return ProbeSpec(ctx, a.spec) }

func (a *Generic) buildArgs(in TaskInput) []string {
	tmpl := a.spec.RunArgs
	if in.ResumeSessionID != "" && len(a.spec.ResumeArgs) > 0 {
		tmpl = a.spec.ResumeArgs
	}
	promptVal := in.Prompt
	if a.spec.PromptVia != "arg" {
		promptVal = "" // prompt travels via stdin; a bare {prompt} token drops out
	}
	rep := strings.NewReplacer(
		"{prompt}", promptVal,
		"{model}", in.Model,
		"{workdir}", in.WorkDir,
		"{session}", in.ResumeSessionID,
	)
	var args []string
	for _, tok := range tmpl {
		expanded := rep.Replace(tok)
		if expanded == "" && isPlaceholder(tok) {
			continue // unset pure placeholder disappears
		}
		args = append(args, expanded)
	}
	args = append(args, in.ExtraArgs...)
	return args
}

func isPlaceholder(tok string) bool {
	switch tok {
	case "{prompt}", "{model}", "{workdir}", "{session}":
		return true
	}
	return false
}

// Run spawns the CLI for one task. See Adapter.Run for the channel contract.
func (a *Generic) Run(ctx context.Context, in TaskInput) (<-chan Event, error) {
	stdin := ""
	if a.spec.PromptVia != "arg" {
		stdin = in.Prompt
	}
	spec := execx.Spec{
		Command: a.spec.Command,
		Args:    a.buildArgs(in),
		Dir:     in.WorkDir,
		Stdin:   stdin,
	}
	proc, err := a.runner.Start(ctx, spec)
	if err != nil {
		return nil, fmt.Errorf("spawn %s: %w", a.spec.Command, err)
	}
	ch := make(chan Event, 64)
	go pumpJSONL(ctx, in, proc, ch, parseTextStream)
	return ch, nil
}
