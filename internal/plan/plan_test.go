package plan

import (
	"strings"
	"testing"
)

var testRoles = map[string]bool{"architect": true, "backend": true, "frontend": true, "docs": true}

func validPlan() *Plan {
	return &Plan{
		Goal: "build it",
		Tasks: []Task{
			{ID: "t1", Role: "backend", Title: "api", Description: "build api", FileScope: []string{"server/"}},
			{ID: "t2", Role: "frontend", Title: "ui", Description: "build ui", FileScope: []string{"web/"}},
			{ID: "t3", Role: "docs", Title: "docs", Description: "write docs", DependsOn: []string{"t1", "t2"}, FileScope: []string{"docs/"}},
		},
	}
}

func errsContain(t *testing.T, errs []error, want string) {
	t.Helper()
	for _, e := range errs {
		if strings.Contains(e.Error(), want) {
			return
		}
	}
	t.Errorf("errors %v missing %q", errs, want)
}

func TestValidateOK(t *testing.T) {
	if errs := validPlan().Validate(testRoles); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestValidateDuplicateID(t *testing.T) {
	p := validPlan()
	p.Tasks[1].ID = "t1"
	errsContain(t, p.Validate(testRoles), "unique")
}

func TestValidateUnknownRole(t *testing.T) {
	p := validPlan()
	p.Tasks[0].Role = "devops"
	errsContain(t, p.Validate(testRoles), `unknown role "devops"`)
}

func TestValidateUnknownDep(t *testing.T) {
	p := validPlan()
	p.Tasks[2].DependsOn = []string{"t9"}
	errsContain(t, p.Validate(testRoles), `unknown task id "t9"`)
}

func TestValidateSelfDep(t *testing.T) {
	p := validPlan()
	p.Tasks[0].DependsOn = []string{"t1"}
	errsContain(t, p.Validate(testRoles), "depends on itself")
}

func TestValidateCycle(t *testing.T) {
	p := validPlan()
	p.Tasks[0].DependsOn = []string{"t3"} // t1 -> t3 -> t1
	errsContain(t, p.Validate(testRoles), "cycle")
}

func TestValidateParallelScopeOverlap(t *testing.T) {
	p := validPlan()
	p.Tasks[1].FileScope = []string{"server/api/"} // overlaps t1's server/, no ordering
	errsContain(t, p.Validate(testRoles), "overlap")
}

func TestValidateOrderedScopeOverlapAllowed(t *testing.T) {
	p := validPlan()
	p.Tasks[1].FileScope = []string{"server/api/"}
	p.Tasks[1].DependsOn = []string{"t1"} // ordered now
	if errs := p.Validate(testRoles); len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
}

func TestTopoSortStable(t *testing.T) {
	p := &Plan{Tasks: []Task{
		{ID: "a", Role: "backend", Description: "x"},
		{ID: "b", Role: "backend", Description: "x", DependsOn: []string{"a"}},
		{ID: "c", Role: "backend", Description: "x", DependsOn: []string{"a"}},
		{ID: "d", Role: "backend", Description: "x", DependsOn: []string{"b", "c"}},
	}}
	order, err := p.TopoSort()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(order, ""); got != "abcd" {
		t.Errorf("order = %q, want abcd", got)
	}
}

func TestScopeViolations(t *testing.T) {
	changed := []string{"server/api.go", "web/App.tsx", "README.md", `Server\nested\x.go`}
	got := ScopeViolations(changed, []string{"server/", "README.md"})
	want := []string{"web/App.tsx"}
	if len(got) != 1 || got[0] != want[0] {
		t.Errorf("violations = %v, want %v", got, want)
	}
	if v := ScopeViolations(changed, nil); v != nil {
		t.Errorf("empty scope should allow everything, got %v", v)
	}
	if v := ScopeViolations(changed, []string{"."}); v != nil {
		t.Errorf("root scope should allow everything, got %v", v)
	}
}
