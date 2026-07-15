package adapter

import (
	"context"
	"slices"
	"testing"

	"github.com/xoralis/cocoder/internal/config"
	"github.com/xoralis/cocoder/internal/execx"
)

func genericSpec() *config.CLISpec {
	return &config.CLISpec{
		Name: "mycli", Adapter: "generic", Command: "mycli",
		RunArgs:    []string{"-p", "{prompt}", "--dir", "{workdir}", "{model}"},
		ResumeArgs: []string{"-r", "{session}", "-p", "{prompt}"},
		PromptVia:  "arg", Output: "text",
	}
}

func TestGenericArgExpansion(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Stdout: "ok\n", Exit: 0}}}
	a := NewGeneric(genericSpec(), fr)
	ch, err := a.Run(context.Background(), TaskInput{
		TaskID: "t", Role: "r", Prompt: "do the thing", WorkDir: "/w", Model: "m9",
		Permission: config.PermEdits,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, final := drain(ch)
	if final.Status != StatusSucceeded {
		t.Errorf("status=%q", final.Status)
	}
	got := fr.Calls[0].Args
	want := []string{"-p", "do the thing", "--dir", "/w", "m9"}
	if !slices.Equal(got, want) {
		t.Errorf("args=%v, want %v", got, want)
	}
	if fr.Calls[0].Stdin != "" {
		t.Errorf("arg mode must not use stdin, got %q", fr.Calls[0].Stdin)
	}
}

func TestGenericUnsetPlaceholderDropped(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Exit: 0}}}
	a := NewGeneric(genericSpec(), fr)
	ch, _ := a.Run(context.Background(), TaskInput{TaskID: "t", Role: "r", Prompt: "p", Permission: config.PermEdits})
	drain(ch)
	got := fr.Calls[0].Args
	// {workdir} and {model} unset -> "--dir" keeps an empty companion?
	// No: pure placeholders drop; "--dir" itself stays (config author's
	// responsibility, documented). {model} disappears entirely.
	want := []string{"-p", "p", "--dir"}
	if !slices.Equal(got, want) {
		t.Errorf("args=%v, want %v", got, want)
	}
}

func TestGenericStdinMode(t *testing.T) {
	spec := genericSpec()
	spec.PromptVia = "stdin"
	spec.RunArgs = []string{"run", "{prompt}"}
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Exit: 0}}}
	a := NewGeneric(spec, fr)
	ch, _ := a.Run(context.Background(), TaskInput{TaskID: "t", Role: "r", Prompt: "the prompt", Permission: config.PermEdits})
	drain(ch)
	call := fr.Calls[0]
	if call.Stdin != "the prompt" {
		t.Errorf("stdin=%q", call.Stdin)
	}
	// In stdin mode the {prompt} token expands empty and drops out.
	if !slices.Equal(call.Args, []string{"run"}) {
		t.Errorf("args=%v", call.Args)
	}
}

func TestGenericResume(t *testing.T) {
	fr := &execx.FakeRunner{Outcomes: []execx.FakeOutcome{{Exit: 0}}}
	a := NewGeneric(genericSpec(), fr)
	if !a.Caps().Resume {
		t.Fatal("resume_args present but Caps().Resume is false")
	}
	ch, _ := a.Run(context.Background(), TaskInput{
		TaskID: "t", Role: "r", Prompt: "again", ResumeSessionID: "s77", Permission: config.PermEdits,
	})
	drain(ch)
	want := []string{"-r", "s77", "-p", "again"}
	if !slices.Equal(fr.Calls[0].Args, want) {
		t.Errorf("args=%v, want %v", fr.Calls[0].Args, want)
	}
}

func TestGenericNoResumeArgsMeansNoResume(t *testing.T) {
	spec := genericSpec()
	spec.ResumeArgs = nil
	a := NewGeneric(spec, &execx.FakeRunner{})
	if a.Caps().Resume {
		t.Error("Caps().Resume must be false without resume_args")
	}
}
