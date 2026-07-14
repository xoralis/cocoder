package orchestrator

import (
	"strings"
	"testing"
)

func TestBuildTaskPromptFull(t *testing.T) {
	got, err := BuildTaskPrompt(PromptData{
		RoleName:         "backend",
		RoleSystemPrompt: "You are a backend engineer.",
		Goal:             "Build a TODO app",
		Architecture:     "## Contracts\nGET /todos returns JSON.",
		TaskID:           "t2",
		Title:            "Implement API",
		Description:      "Implement the /todos endpoint.",
		Acceptance:       "curl /todos returns 200.",
		FileScope:        []string{"server/", "internal/"},
		DepNotes: []DepNote{{
			ID: "t1", Title: "Define schema", Role: "architect",
			Summary: "Schema decided.", ChangedFiles: []string{"api/schema.json"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"You are a backend engineer.",
		"Build a TODO app",
		"GET /todos returns JSON.",
		"Implement API", "(id: t2, role: backend)",
		"curl /todos returns 200.",
		"- server/", "- internal/",
		"Do not run git commit or push.",
		"Define schema (t1, architect)",
		"Changed files: api/schema.json",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, got)
		}
	}
	if strings.Contains(got, "<no value>") {
		t.Errorf("template leaked <no value>:\n%s", got)
	}
}

func TestBuildTaskPromptMinimal(t *testing.T) {
	got, err := BuildTaskPrompt(PromptData{
		RoleName: "backend", Goal: "small fix", TaskID: "a1",
		Title: "small fix", Description: "fix the thing",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"Role briefing", "Architecture", "Acceptance", "prerequisite", "<no value>"} {
		if strings.Contains(got, banned) {
			t.Errorf("minimal prompt should not contain %q:\n%s", banned, got)
		}
	}
	if !strings.Contains(got, "Do not run git commit or push.") {
		t.Errorf("missing constraints:\n%s", got)
	}
}
