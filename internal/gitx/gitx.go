// Package gitx wraps the git CLI for snapshots, change detection and
// file-scope boundary checks.
package gitx

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strings"
)

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return out.String(), err
}

// IsRepo reports whether dir is inside a git work tree.
func IsRepo(dir string) bool {
	out, err := git(dir, "rev-parse", "--is-inside-work-tree")
	return err == nil && strings.TrimSpace(out) == "true"
}

// Head returns the current HEAD hash, or "" (e.g. unborn branch).
func Head(dir string) string {
	out, err := git(dir, "rev-parse", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Snapshot captures the dirty-path set at a point in time so changes made
// by an agent run can be attributed afterwards.
type Snapshot struct {
	Dir   string
	Head  string
	Dirty map[string]bool
}

// Take records HEAD plus the current dirty set.
func Take(dir string) (*Snapshot, error) {
	dirty, err := statusPaths(dir)
	if err != nil {
		return nil, err
	}
	return &Snapshot{Dir: dir, Head: Head(dir), Dirty: dirty}, nil
}

// ChangedSince returns paths dirty now that were clean at snapshot time.
// (Files already dirty before the run stay unattributed — acceptable for
// the MVP; agents are told not to commit, so commits don't hide changes.)
func (s *Snapshot) ChangedSince() ([]string, error) {
	cur, err := statusPaths(s.Dir)
	if err != nil {
		return nil, err
	}
	var changed []string
	for p := range cur {
		if !s.Dirty[p] {
			changed = append(changed, p)
		}
	}
	sort.Strings(changed)
	return changed, nil
}

// statusPaths parses `git status --porcelain=v1 -z -uall` into a path set.
// -z gives NUL-separated entries with no quoting; rename/copy entries carry
// the origin path as an extra NUL field which we skip.
func statusPaths(dir string) (map[string]bool, error) {
	out, err := git(dir, "status", "--porcelain=v1", "-z", "-uall")
	if err != nil {
		return nil, fmt.Errorf("git status in %s: %w", dir, err)
	}
	m := map[string]bool{}
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		status, path := e[:2], e[3:]
		m[path] = true
		if status[0] == 'R' || status[0] == 'C' {
			i++ // skip the origin-path field
		}
	}
	return m, nil
}

// DiffStat renders `git diff --stat [since]` (empty since = vs HEAD),
// trimmed and capped to maxLines.
func DiffStat(dir, since string, maxLines int) string {
	args := []string{"diff", "--stat"}
	if since != "" {
		args = append(args, since)
	}
	out, err := git(dir, args...)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if maxLines > 0 && len(lines) > maxLines {
		lines = append(lines[:maxLines], fmt.Sprintf("... (%d more lines)", len(lines)-maxLines))
	}
	return strings.Join(lines, "\n")
}

// Untracked lists untracked files (porcelain "??" entries).
func Untracked(dir string) []string {
	out, err := git(dir, "status", "--porcelain=v1", "-z", "-uall")
	if err != nil {
		return nil
	}
	var files []string
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		status, path := e[:2], e[3:]
		if status == "??" {
			files = append(files, path)
		}
		if status[0] == 'R' || status[0] == 'C' {
			i++
		}
	}
	sort.Strings(files)
	return files
}
