package state

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/xoralis/cocoder/internal/plan"
)

// TaskNote is the condensed, prompt-ready record of a completed task.
type TaskNote struct {
	ID           string   `json:"id"`
	Title        string   `json:"title"`
	Role         string   `json:"role"`
	Summary      string   `json:"summary"`
	ChangedFiles []string `json:"changed_files,omitempty"`
}

// Blackboard is the shared context all task prompts are composed from:
// the goal, the architect's contracts document, and condensed results of
// completed tasks. Agents never talk to each other directly - everything
// flows through here.
type Blackboard struct {
	Goal         string              `json:"goal"`
	Architecture string              `json:"architecture"`
	TaskNotes    map[string]TaskNote `json:"task_notes"`
}

// SaveBlackboard atomically writes blackboard.json.
func (s *RunStore) SaveBlackboard(b *Blackboard) error {
	return writeAtomicJSON(filepath.Join(s.Dir, "blackboard.json"), b)
}

// LoadBlackboard reads blackboard.json.
func (s *RunStore) LoadBlackboard() (*Blackboard, error) {
	var b Blackboard
	if err := readJSON(filepath.Join(s.Dir, "blackboard.json"), &b); err != nil {
		return nil, err
	}
	if b.TaskNotes == nil {
		b.TaskNotes = map[string]TaskNote{}
	}
	return &b, nil
}

// SavePlanFile atomically writes plan.json.
func (s *RunStore) SavePlanFile(p *plan.Plan) error {
	return writeAtomicJSON(filepath.Join(s.Dir, "plan.json"), p)
}

// LoadPlanFile reads plan.json.
func (s *RunStore) LoadPlanFile() (*plan.Plan, error) {
	var p plan.Plan
	if err := readJSON(filepath.Join(s.Dir, "plan.json"), &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// PlanPath is where plan.json lives (shown to users for hand edits).
func (s *RunStore) PlanPath() string { return filepath.Join(s.Dir, "plan.json") }

// SaveArchitecture writes architecture.md.
func (s *RunStore) SaveArchitecture(md string) error {
	if strings.TrimSpace(md) == "" {
		return nil
	}
	return os.WriteFile(filepath.Join(s.Dir, "architecture.md"), []byte(md), 0o644)
}

// SavePlannerRaw persists a failed planner reply for hand repair.
func (s *RunStore) SavePlannerRaw(attempt int, text string) {
	name := "planner-raw-" + strconv.Itoa(attempt) + ".txt"
	_ = os.WriteFile(filepath.Join(s.Dir, name), []byte(text), 0o644)
}
