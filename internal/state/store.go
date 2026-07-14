// Package state persists run/task state under .ccd/runs/<run-id>/ with
// atomic writes so `ccd resume` and `ccd status` can always trust disk.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/xoralis/cocoder/internal/adapter"
)

// Meta describes one run (meta.json).
type Meta struct {
	RunID      string     `json:"run_id"`
	Mode       string     `json:"mode"` // assign | run
	Goal       string     `json:"goal"`
	Status     string     `json:"status"` // running | completed | failed | interrupted
	StartHead  string     `json:"start_head,omitempty"`
	CcdVersion string     `json:"ccd_version,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	EndedAt    *time.Time `json:"ended_at,omitempty"`
}

// TaskState is the persisted per-task state (task-<id>.state.json).
type TaskState struct {
	ID              string             `json:"id"`
	Role            string             `json:"role"`
	Title           string             `json:"title,omitempty"`
	Status          adapter.TaskStatus `json:"status"`
	Attempts        int                `json:"attempts"`
	SessionID       string             `json:"session_id,omitempty"`
	Summary         string             `json:"summary,omitempty"`
	CostUSD         float64            `json:"cost_usd,omitempty"`
	NumTurns        int                `json:"num_turns,omitempty"`
	StartedAt       time.Time          `json:"started_at,omitempty"`
	EndedAt         time.Time          `json:"ended_at,omitempty"`
	LastError       string             `json:"last_error,omitempty"`
	ChangedFiles    []string           `json:"changed_files,omitempty"`
	ScopeViolations []string           `json:"scope_violations,omitempty"`
}

// RunStore reads and writes one run directory.
type RunStore struct {
	Dir string

	mu     sync.Mutex
	events *os.File
}

// runsRoot returns <projectDir>/.ccd/runs.
func runsRoot(projectDir string) string {
	return filepath.Join(projectDir, ".ccd", "runs")
}

// CreateRun makes a fresh run directory (with logs/ and tmp/) and returns
// its store.
func CreateRun(projectDir string) (*RunStore, error) {
	id := newRunID()
	dir := filepath.Join(runsRoot(projectDir), id)
	for _, sub := range []string{"logs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			return nil, fmt.Errorf("create run dir: %w", err)
		}
	}
	return &RunStore{Dir: dir}, nil
}

// OpenRun opens an existing run directory by id.
func OpenRun(projectDir, runID string) (*RunStore, error) {
	dir := filepath.Join(runsRoot(projectDir), runID)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("run %s not found under %s", runID, runsRoot(projectDir))
	}
	return &RunStore{Dir: dir}, nil
}

// LatestRunID returns the newest run id (ids sort chronologically).
func LatestRunID(projectDir string) (string, error) {
	entries, err := os.ReadDir(runsRoot(projectDir))
	if err != nil {
		return "", fmt.Errorf("no runs found: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() {
			ids = append(ids, e.Name())
		}
	}
	if len(ids) == 0 {
		return "", fmt.Errorf("no runs found under %s", runsRoot(projectDir))
	}
	sort.Strings(ids)
	return ids[len(ids)-1], nil
}

// RunID is the directory name.
func (s *RunStore) RunID() string { return filepath.Base(s.Dir) }

// SaveMeta atomically writes meta.json.
func (s *RunStore) SaveMeta(m *Meta) error {
	return writeAtomicJSON(filepath.Join(s.Dir, "meta.json"), m)
}

// LoadMeta reads meta.json.
func (s *RunStore) LoadMeta() (*Meta, error) {
	var m Meta
	if err := readJSON(filepath.Join(s.Dir, "meta.json"), &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// SaveTaskState atomically writes task-<id>.state.json.
func (s *RunStore) SaveTaskState(ts *TaskState) error {
	return writeAtomicJSON(filepath.Join(s.Dir, "task-"+ts.ID+".state.json"), ts)
}

// LoadTaskStates reads every task-*.state.json in the run.
func (s *RunStore) LoadTaskStates() (map[string]*TaskState, error) {
	matches, err := filepath.Glob(filepath.Join(s.Dir, "task-*.state.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]*TaskState{}
	for _, m := range matches {
		var ts TaskState
		if err := readJSON(m, &ts); err != nil {
			return nil, fmt.Errorf("read %s: %w", m, err)
		}
		out[ts.ID] = &ts
	}
	return out, nil
}

// AppendEvent appends one normalized event to events.jsonl (best effort).
func (s *RunStore) AppendEvent(e adapter.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events == nil {
		f, err := os.OpenFile(filepath.Join(s.Dir, "events.jsonl"),
			os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		s.events = f
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = s.events.Write(append(b, '\n'))
}

// TaskLog opens the raw log file for one task in append mode, so retry
// attempts accumulate in the same file.
func (s *RunStore) TaskLog(taskID string) (io.WriteCloser, error) {
	return os.OpenFile(filepath.Join(s.Dir, "logs", taskID+".log"),
		os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
}

// TaskLogPath returns the raw log path for one task.
func (s *RunStore) TaskLogPath(taskID string) string {
	return filepath.Join(s.Dir, "logs", taskID+".log")
}

// Close releases open file handles.
func (s *RunStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.events != nil {
		_ = s.events.Close()
		s.events = nil
	}
}

// newRunID is sortable-by-time plus 2 random bytes: 20260714-231502-a1b2.
func newRunID() string {
	var b [2]byte
	_, _ = rand.Read(b[:])
	return time.Now().Format("20060102-150405") + "-" + hex.EncodeToString(b[:])
}

// writeAtomicJSON writes tmp-then-rename in the same directory. Go's rename
// on Windows uses MOVEFILE_REPLACE_EXISTING, so replacing is atomic enough
// for our single-writer model.
func writeAtomicJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	return dec.Decode(v)
}
