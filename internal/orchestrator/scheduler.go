package orchestrator

import (
	"github.com/xoralis/cocoder/internal/adapter"
	"github.com/xoralis/cocoder/internal/plan"
)

// scheduler tracks task readiness over the plan DAG. It holds no locks:
// the orchestrator main loop is the single writer, so with a worker pool of
// one this is trivially correct, and raising the pool size later only needs
// worktree isolation, not a redesign.
type scheduler struct {
	plan   *plan.Plan
	order  []string
	status map[string]adapter.TaskStatus
}

func newScheduler(p *plan.Plan, order []string, initial map[string]adapter.TaskStatus) *scheduler {
	status := map[string]adapter.TaskStatus{}
	for _, t := range p.Tasks {
		if s, ok := initial[t.ID]; ok && s != "" {
			status[t.ID] = s
		} else {
			status[t.ID] = adapter.StatusPending
		}
	}
	return &scheduler{plan: p, order: order, status: status}
}

// nextReady returns the earliest pending task whose dependencies have all
// succeeded, or nil when none is currently runnable.
func (s *scheduler) nextReady() *plan.Task {
	for _, id := range s.order {
		if s.status[id] != adapter.StatusPending {
			continue
		}
		t := s.plan.Task(id)
		if s.depsSucceeded(t) {
			return t
		}
	}
	return nil
}

func (s *scheduler) depsSucceeded(t *plan.Task) bool {
	for _, d := range t.DependsOn {
		if s.status[d] != adapter.StatusSucceeded {
			return false
		}
	}
	return true
}

func (s *scheduler) mark(id string, st adapter.TaskStatus) { s.status[id] = st }

// blockDependents transitively marks everything depending on a failed task
// as blocked, so the final report explains why they never ran.
func (s *scheduler) blockDependents(failedID string) {
	changed := true
	for changed {
		changed = false
		for _, t := range s.plan.Tasks {
			if s.status[t.ID] != adapter.StatusPending {
				continue
			}
			for _, d := range t.DependsOn {
				st := s.status[d]
				if st == adapter.StatusFailed || st == adapter.StatusBlocked {
					s.status[t.ID] = adapter.StatusBlocked
					changed = true
					break
				}
			}
		}
	}
}

// allDone reports whether no task is pending (run is finished).
func (s *scheduler) pendingCount() int {
	n := 0
	for _, st := range s.status {
		if st == adapter.StatusPending {
			n++
		}
	}
	return n
}

func (s *scheduler) counts() map[adapter.TaskStatus]int {
	m := map[adapter.TaskStatus]int{}
	for _, st := range s.status {
		m[st]++
	}
	return m
}
