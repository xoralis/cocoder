package state

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xoralis/cocoder/internal/adapter"
)

func TestRunLifecycle(t *testing.T) {
	dir := t.TempDir()
	s, err := CreateRun(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if ok, _ := regexp.MatchString(`^\d{8}-\d{6}-[0-9a-f]{4}$`, s.RunID()); !ok {
		t.Errorf("run id format: %q", s.RunID())
	}

	meta := &Meta{RunID: s.RunID(), Mode: "assign", Goal: "test goal", Status: "running", CreatedAt: time.Now()}
	if err := s.SaveMeta(meta); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadMeta()
	if err != nil {
		t.Fatal(err)
	}
	if got.Goal != "test goal" || got.Status != "running" {
		t.Errorf("meta roundtrip: %+v", got)
	}

	ts := &TaskState{ID: "t1", Role: "backend", Status: adapter.StatusSucceeded, SessionID: "s1", CostUSD: 0.5}
	if err := s.SaveTaskState(ts); err != nil {
		t.Fatal(err)
	}
	states, err := s.LoadTaskStates()
	if err != nil {
		t.Fatal(err)
	}
	if len(states) != 1 || states["t1"].SessionID != "s1" {
		t.Errorf("task states: %+v", states)
	}

	// No .tmp litter from atomic writes.
	matches, _ := filepath.Glob(filepath.Join(s.Dir, "*.tmp"))
	if len(matches) != 0 {
		t.Errorf("tmp files left behind: %v", matches)
	}

	for i := 0; i < 3; i++ {
		s.AppendEvent(adapter.Event{Kind: adapter.EvAgentText, TaskID: "t1", Role: "backend", Time: time.Now(), Text: "x"})
	}
	s.Close()
	b, err := os.ReadFile(filepath.Join(s.Dir, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "\n"); n != 3 {
		t.Errorf("events.jsonl lines = %d, want 3", n)
	}

	// Reopen and locate.
	if _, err := OpenRun(dir, s.RunID()); err != nil {
		t.Errorf("OpenRun: %v", err)
	}
	latest, err := LatestRunID(dir)
	if err != nil || latest != s.RunID() {
		t.Errorf("LatestRunID = %q err=%v, want %q", latest, err, s.RunID())
	}
}

func TestTaskLog(t *testing.T) {
	s, err := CreateRun(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	w, err := s.TaskLog("t9")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("raw output")); err != nil {
		t.Fatal(err)
	}
	w.Close()
	b, err := os.ReadFile(s.TaskLogPath("t9"))
	if err != nil || string(b) != "raw output" {
		t.Errorf("log roundtrip: %q err=%v", b, err)
	}
}

func TestLatestUnfinishedRunID(t *testing.T) {
	dir := t.TempDir()
	mk := func(status string) *RunStore {
		s, err := CreateRun(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		if err := s.SaveMeta(&Meta{RunID: s.RunID(), Mode: "run", Status: status, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(1100 * time.Millisecond) // run ids have second granularity
		return s
	}
	mk("completed")
	unfinished := mk("interrupted")
	mk("completed")

	got, err := LatestUnfinishedRunID(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != unfinished.RunID() {
		t.Errorf("latest unfinished = %q, want %q", got, unfinished.RunID())
	}
}

func TestLatestUnfinishedNoneFound(t *testing.T) {
	dir := t.TempDir()
	s, _ := CreateRun(dir)
	defer s.Close()
	_ = s.SaveMeta(&Meta{RunID: s.RunID(), Mode: "run", Status: "completed", CreatedAt: time.Now()})
	if _, err := LatestUnfinishedRunID(dir); err == nil {
		t.Fatal("expected error when every run is completed")
	}
}
