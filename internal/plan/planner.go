package plan

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"text/template"
	"time"

	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/config"
)

// planSchemaJSON is enforced via structured output when the architect CLI
// supports it (claude --json-schema / codex --output-schema). Single line:
// it travels as one argv token.
const planSchemaJSON = `{"type":"object","required":["goal","architecture","tasks"],"properties":{"goal":{"type":"string","description":"one-line restatement of the requirement"},"architecture":{"type":"string","description":"the full ARCHITECTURE & CONTRACTS markdown document"},"tasks":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"object","required":["id","role","title","description","depends_on","file_scope","acceptance"],"properties":{"id":{"type":"string"},"role":{"type":"string"},"title":{"type":"string"},"description":{"type":"string"},"depends_on":{"type":"array","items":{"type":"string"}},"file_scope":{"type":"array","items":{"type":"string"}},"acceptance":{"type":"string"}},"additionalProperties":false}}},"additionalProperties":false}`

// Deps wires the planner to the orchestrator without import cycles.
type Deps struct {
	Adapter  adapter.Adapter
	Cfg      *config.Config
	RoleName string // planner role name, e.g. "architect"
	WorkDir  string
	Timeout  time.Duration
	Retries  int // validation retries after the first attempt

	// Drain consumes one run's event stream (console rendering +
	// persistence happen inside) and returns the final result plus the
	// concatenated agent text.
	Drain func(ch <-chan adapter.Event) (*adapter.TaskResult, string)
	// SaveRaw persists the raw reply of a failed attempt (best effort).
	SaveRaw func(attempt int, text string)
}

// GenStats reports planner cost/attempts for budget accounting.
type GenStats struct {
	CostUSD   float64
	Attempts  int
	SessionID string
}

// Generate runs the architect role until it produces a validated Plan.
//
// Parse preference: structured output (schema-enforced) > last ```json
// fence > last balanced JSON object. Validation errors are fed back
// verbatim; the retry resumes the CLI session when possible.
func Generate(ctx context.Context, goal string, d Deps) (*Plan, *GenStats, error) {
	role := d.Cfg.Roles[d.RoleName]
	if role == nil {
		return nil, &GenStats{}, fmt.Errorf("planner role %q is not defined in the config", d.RoleName)
	}
	stats := &GenStats{}

	schema := ""
	if d.Adapter.Caps().StructuredOutput {
		schema = planSchemaJSON
	}
	basePrompt := plannerPrompt(goal, d.Cfg, schema != "")
	prompt := basePrompt
	resumeSID := ""
	lastArch := ""
	schemaFallbackUsed := false

	maxAttempts := 1 + d.Retries
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		tctx := ctx
		var cancel context.CancelFunc = func() {}
		if d.Timeout > 0 {
			tctx, cancel = context.WithTimeout(ctx, d.Timeout)
		}
		ch, err := d.Adapter.Run(tctx, adapter.TaskInput{
			TaskID:          "plan",
			Role:            d.RoleName,
			Prompt:          prompt,
			WorkDir:         d.WorkDir,
			Model:           role.Model,
			Permission:      config.PermReadOnly, // the architect never writes
			ResumeSessionID: resumeSID,
			JSONSchema:      schema,
			ExtraArgs:       role.ExtraArgs,
		})
		if err != nil {
			cancel()
			return nil, stats, fmt.Errorf("spawn architect: %w", err)
		}
		res, text := d.Drain(ch)
		cancel()
		stats.Attempts++
		stats.CostUSD += res.CostUSD
		if res.SessionID != "" {
			stats.SessionID = res.SessionID
		}

		switch res.Status {
		case adapter.StatusInterrupted:
			return nil, stats, fmt.Errorf("planning interrupted")
		case adapter.StatusFailed:
			// One-shot fallback: strip the schema flag if this CLI build
			// doesn't know it, then redo the attempt.
			if schema != "" && !schemaFallbackUsed && looksLikeUnknownFlag(res.ErrMsg) {
				schemaFallbackUsed = true
				schema = ""
				prompt = plannerPrompt(goal, d.Cfg, false)
				resumeSID = ""
				attempt--
				continue
			}
			return nil, stats, fmt.Errorf("architect run failed: %s", strings.TrimSpace(res.ErrMsg))
		}

		p, perrs := interpretReply(res, text, goal, lastArch)
		if p != nil {
			lastArch = p.Architecture
			if verrs := p.Validate(roleSet(d.Cfg)); len(verrs) > 0 {
				perrs = verrs
			} else {
				return p, stats, nil
			}
		}

		if d.SaveRaw != nil {
			d.SaveRaw(attempt, text+"\n"+res.Summary)
		}
		if attempt == maxAttempts {
			break
		}
		// Prepare the retry.
		if d.Adapter.Caps().Resume && res.SessionID != "" {
			resumeSID = res.SessionID
			prompt = feedbackPrompt(perrs, schema != "")
		} else {
			resumeSID = ""
			prompt = basePrompt + "\n\n# Problems with your previous reply\n" + bulletize(perrs) +
				"\nFix these and reply again following the format exactly."
		}
	}
	return nil, stats, fmt.Errorf(
		"architect failed to produce a valid plan after %d attempt(s); raw replies are saved in the run directory - fix the JSON by hand and rerun with: ccd run --plan <file>",
		stats.Attempts)
}

// interpretReply extracts a Plan from a successful architect reply.
func interpretReply(res *adapter.TaskResult, text, goal, lastArch string) (*Plan, []error) {
	var p Plan
	if len(res.StructuredOutput) > 0 {
		if err := json.Unmarshal(res.StructuredOutput, &p); err != nil {
			return nil, []error{fmt.Errorf("the structured output failed to parse as a plan: %v", err)}
		}
	} else {
		jsonStr, before, err := ExtractJSON(text + "\n" + res.Summary)
		if err != nil {
			return nil, []error{err}
		}
		pp, perr := Parse(jsonStr)
		if perr != nil {
			return nil, []error{perr}
		}
		p = *pp
		if p.Architecture == "" {
			p.Architecture = before
		}
	}
	if p.Goal == "" {
		p.Goal = goal
	}
	if p.Architecture == "" {
		p.Architecture = lastArch // retries may reply with JSON only
	}
	return &p, nil
}

func looksLikeUnknownFlag(errMsg string) bool {
	s := strings.ToLower(errMsg)
	return strings.Contains(s, "json-schema") &&
		(strings.Contains(s, "unknown option") || strings.Contains(s, "unknown flag") || strings.Contains(s, "unrecognized"))
}

func roleSet(cfg *config.Config) map[string]bool {
	m := map[string]bool{}
	for name := range cfg.Roles {
		m[name] = true
	}
	return m
}

func bulletize(errs []error) string {
	var b strings.Builder
	for _, e := range errs {
		fmt.Fprintf(&b, "- %s\n", e)
	}
	return b.String()
}

func feedbackPrompt(errs []error, structured bool) string {
	if structured {
		return "Your previous plan failed validation:\n" + bulletize(errs) +
			"\nReply with the corrected plan object (same schema: goal, architecture, tasks)."
	}
	return "Your previous plan failed validation:\n" + bulletize(errs) +
		"\nReply with ONLY the corrected JSON object in a ```json fenced block. No other text."
}

// plannerPromptData feeds the planner prompt template.
type plannerPromptData struct {
	Goal       string
	Roles      []plannerRoleLine
	Fence      string
	Structured bool
}

type plannerRoleLine struct {
	Name  string
	CLI   string
	Scope string
}

const plannerTmplText = `You are the ARCHITECT of an automated multi-agent coding pipeline named ccd.
Your output will be parsed by a machine. Follow the format EXACTLY.

# Requirement
{{.Goal}}

# Available roles (you may ONLY assign tasks to these)
{{range .Roles}}- {{.Name}}: cli={{.CLI}}{{if .Scope}}, default file scope: {{.Scope}}{{end}}
{{end}}
# Your job
1. Inspect the repository as needed (you are read-only).
2. Write a concise ARCHITECTURE & CONTRACTS document in markdown: module
   layout, public interfaces / API signatures / data schemas that tasks must
   conform to, and key decisions. Interfaces are decided HERE, once, centrally.
3. Produce the task plan.
{{if .Structured}}
Your reply is constrained to a JSON object with fields "goal",
"architecture" and "tasks" - put the full markdown document from step 2
into the "architecture" field.
{{else}}
Output the plan as a single JSON object inside a {{.Fence}}json fenced block.
It must be the LAST fenced block in your reply; the markdown document from
step 2 comes before it.
{{end}}
# Plan JSON shape
{
  "goal": "one-line restatement",
  "tasks": [
    {
      "id": "t1",
      "role": "backend",
      "title": "short imperative title",
      "description": "what to build, referencing the contracts above",
      "depends_on": [],
      "file_scope": ["server/"],
      "acceptance": "how a reviewer verifies this task is done"
    }
  ]
}

# Hard rules
- 2 to 10 tasks. Each task must be completable by one agent in one session.
- The dependency graph must be acyclic; depends_on may only reference other task ids.
- Every task gets a file_scope. Scopes of tasks that could run concurrently
  MUST NOT overlap; shared files (e.g. an API schema) belong to exactly ONE
  early task that the others depend on.
- Do NOT include tasks for git commits, deployment, or running ccd itself.
`

var plannerTmpl = template.Must(template.New("planner").Parse(plannerTmplText))

func plannerPrompt(goal string, cfg *config.Config, structured bool) string {
	var roles []plannerRoleLine
	names := make([]string, 0, len(cfg.Roles))
	for n := range cfg.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		r := cfg.Roles[n]
		roles = append(roles, plannerRoleLine{Name: n, CLI: r.CLI, Scope: strings.Join(r.FileScope, ", ")})
	}
	var b strings.Builder
	_ = plannerTmpl.Execute(&b, plannerPromptData{Goal: goal, Roles: roles, Fence: "```", Structured: structured})
	return b.String()
}
