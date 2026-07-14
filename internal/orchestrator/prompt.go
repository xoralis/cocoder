// Package orchestrator composes task prompts and (from M2) runs the
// plan/schedule/execute loop.
package orchestrator

import (
	"strings"
	"text/template"
)

// DepNote is the condensed result of a completed prerequisite task, fed
// into dependent tasks' prompts (the "blackboard" hand-off).
type DepNote struct {
	ID           string
	Title        string
	Role         string
	Summary      string
	ChangedFiles []string
}

// PromptData carries everything the task prompt template needs. Optional
// fields (Architecture, Acceptance, DepNotes, FileScope) render
// conditionally, so `ccd assign` (no plan) reuses the same template as
// `ccd run`.
type PromptData struct {
	RoleName         string
	RoleSystemPrompt string
	Goal             string
	Architecture     string

	TaskID      string
	Title       string
	Description string
	Acceptance  string
	FileScope   []string

	DepNotes []DepNote
}

// taskPromptTmpl is the single authoritative task prompt shape. The role
// system prompt is folded into the body (not --append-system-prompt) so it
// survives the stdin-only transport required on Windows.
const taskPromptTmpl = `{{if .RoleSystemPrompt}}## Role briefing
{{.RoleSystemPrompt}}

{{end}}# Mission (overall goal)
{{.Goal}}
{{if .Architecture}}
# Architecture & contracts (authoritative - conform to these)
{{.Architecture}}
{{end}}
# Your task: {{.Title}} (id: {{.TaskID}}, role: {{.RoleName}})
{{.Description}}
{{if .Acceptance}}
## Acceptance criteria
{{.Acceptance}}
{{end}}
{{if .FileScope}}## File scope - you may ONLY create or modify files under:
{{range .FileScope}}- {{.}}
{{end}}Do not touch other paths. Do not run git commit or push.
{{else}}## Constraints
Do not run git commit or push.
{{end}}{{if .DepNotes}}
# Results from completed prerequisite tasks
{{range .DepNotes}}## {{.Title}} ({{.ID}}, {{.Role}})
{{.Summary}}
{{if .ChangedFiles}}Changed files: {{join .ChangedFiles ", "}}
{{end}}
{{end}}{{end}}
# When finished
End your reply with a short summary: files you changed, decisions made, and anything later tasks must know.
`

var taskTmpl = template.Must(template.New("task").
	Funcs(template.FuncMap{"join": strings.Join}).
	Parse(taskPromptTmpl))

// BuildTaskPrompt renders the task prompt.
func BuildTaskPrompt(d PromptData) (string, error) {
	var b strings.Builder
	if err := taskTmpl.Execute(&b, d); err != nil {
		return "", err
	}
	return b.String(), nil
}
