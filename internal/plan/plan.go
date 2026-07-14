// Package plan defines the task-plan model produced by the architect role,
// its validation and topological ordering, JSON extraction from noisy LLM
// output, and the planner invocation loop.
package plan

import (
	"fmt"
	"path"
	"sort"
	"strings"
)

// Task is one unit of work assigned to a role.
type Task struct {
	ID          string   `json:"id"`
	Role        string   `json:"role"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	DependsOn   []string `json:"depends_on"`
	FileScope   []string `json:"file_scope"`
	Acceptance  string   `json:"acceptance"`
}

// Plan is the architect's output: a task DAG plus the architecture document
// every task must conform to.
type Plan struct {
	Goal         string `json:"goal"`
	Architecture string `json:"architecture,omitempty"`
	Tasks        []Task `json:"tasks"`
}

// Task returns the task with the given id, or nil.
func (p *Plan) Task(id string) *Task {
	for i := range p.Tasks {
		if p.Tasks[i].ID == id {
			return &p.Tasks[i]
		}
	}
	return nil
}

// Validate checks the plan semantically. Errors are complete English
// sentences because they are fed back to the architect LLM verbatim on
// retry.
func (p *Plan) Validate(roleNames map[string]bool) []error {
	var errs []error
	if len(p.Tasks) == 0 {
		return []error{fmt.Errorf("the plan contains no tasks; produce at least one task")}
	}
	seen := map[string]int{}
	for i, t := range p.Tasks {
		if strings.TrimSpace(t.ID) == "" {
			errs = append(errs, fmt.Errorf("task #%d (%q) has an empty id", i+1, t.Title))
			continue
		}
		if j, dup := seen[t.ID]; dup {
			errs = append(errs, fmt.Errorf("task id %q is used by both task #%d and task #%d; ids must be unique", t.ID, j+1, i+1))
		}
		seen[t.ID] = i
	}
	for _, t := range p.Tasks {
		if t.ID == "" {
			continue
		}
		if !roleNames[t.Role] {
			errs = append(errs, fmt.Errorf("task %q uses unknown role %q; available roles: %s",
				t.ID, t.Role, strings.Join(sortedKeys(roleNames), ", ")))
		}
		if strings.TrimSpace(t.Description) == "" {
			errs = append(errs, fmt.Errorf("task %q has an empty description", t.ID))
		}
		for _, d := range t.DependsOn {
			if d == t.ID {
				errs = append(errs, fmt.Errorf("task %q depends on itself; remove it from depends_on", t.ID))
				continue
			}
			if _, ok := seen[d]; !ok {
				errs = append(errs, fmt.Errorf("task %q depends on unknown task id %q", t.ID, d))
			}
		}
	}
	if len(errs) > 0 {
		return errs // graph checks below assume structurally sane tasks
	}
	if _, err := p.TopoSort(); err != nil {
		errs = append(errs, err)
	}
	errs = append(errs, p.scopeConflicts()...)
	return errs
}

// TopoSort returns task ids in dependency order (Kahn), stable with respect
// to the original task order.
func (p *Plan) TopoSort() ([]string, error) {
	indeg := map[string]int{}
	dependents := map[string][]string{}
	for _, t := range p.Tasks {
		indeg[t.ID] = len(t.DependsOn)
		for _, d := range t.DependsOn {
			dependents[d] = append(dependents[d], t.ID)
		}
	}
	emitted := map[string]bool{}
	var order []string
	for len(order) < len(p.Tasks) {
		progressed := false
		for _, t := range p.Tasks { // earliest ready task first -> stable
			if emitted[t.ID] || indeg[t.ID] != 0 {
				continue
			}
			emitted[t.ID] = true
			order = append(order, t.ID)
			for _, dn := range dependents[t.ID] {
				indeg[dn]--
			}
			progressed = true
			break
		}
		if !progressed {
			var cyc []string
			for _, t := range p.Tasks {
				if !emitted[t.ID] {
					cyc = append(cyc, t.ID)
				}
			}
			return nil, fmt.Errorf("the dependency graph has a cycle involving tasks: %s; remove circular depends_on references",
				strings.Join(cyc, ", "))
		}
	}
	return order, nil
}

// scopeConflicts flags file_scope overlaps between tasks that could run in
// parallel (neither transitively depends on the other). Tasks with an empty
// scope are skipped (serial MVP tolerates them; the prompt demands scopes).
func (p *Plan) scopeConflicts() []error {
	reach := p.reachability()
	var errs []error
	for i := 0; i < len(p.Tasks); i++ {
		for j := i + 1; j < len(p.Tasks); j++ {
			a, b := p.Tasks[i], p.Tasks[j]
			if reach[a.ID][b.ID] || reach[b.ID][a.ID] {
				continue // ordered by dependencies
			}
			if len(a.FileScope) == 0 || len(b.FileScope) == 0 {
				continue
			}
			for _, sa := range a.FileScope {
				for _, sb := range b.FileScope {
					if scopesOverlap(sa, sb) {
						errs = append(errs, fmt.Errorf(
							"tasks %q and %q could run in parallel but their file_scope entries %q and %q overlap; make one depend on the other or give them disjoint scopes",
							a.ID, b.ID, sa, sb))
					}
				}
			}
		}
	}
	return errs
}

// reachability computes, per task, the set of tasks reachable via
// depends_on edges (i.e. its transitive prerequisites).
func (p *Plan) reachability() map[string]map[string]bool {
	memo := map[string]map[string]bool{}
	var visit func(id string) map[string]bool
	visit = func(id string) map[string]bool {
		if r, ok := memo[id]; ok {
			return r
		}
		r := map[string]bool{}
		memo[id] = r // pre-set breaks cycles (validated elsewhere)
		t := p.Task(id)
		if t == nil {
			return r
		}
		for _, d := range t.DependsOn {
			r[d] = true
			for k := range visit(d) {
				r[k] = true
			}
		}
		return r
	}
	for _, t := range p.Tasks {
		visit(t.ID)
	}
	return memo
}

// NormalizeScope canonicalizes a scope entry or file path for comparison:
// forward slashes, cleaned, lower-case (Windows-friendly), no trailing
// slash.
func NormalizeScope(s string) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\\", "/")
	s = path.Clean(s)
	s = strings.TrimSuffix(s, "/")
	return strings.ToLower(s)
}

// scopesOverlap reports whether two scope prefixes can claim the same file.
func scopesOverlap(a, b string) bool {
	a, b = NormalizeScope(a), NormalizeScope(b)
	if a == "." || b == "." || a == "" || b == "" {
		return true // repo root claims everything
	}
	return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ScopeViolations returns the changed files that fall outside the given
// scope prefixes. An empty scope means unrestricted.
func ScopeViolations(changedFiles, scope []string) []string {
	if len(scope) == 0 {
		return nil
	}
	norm := make([]string, len(scope))
	for i, s := range scope {
		norm[i] = NormalizeScope(s)
	}
	var out []string
	for _, f := range changedFiles {
		nf := NormalizeScope(f)
		ok := false
		for _, s := range norm {
			if s == "." || nf == s || strings.HasPrefix(nf, s+"/") {
				ok = true
				break
			}
		}
		if !ok {
			out = append(out, f)
		}
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
