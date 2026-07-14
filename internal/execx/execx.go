// Package execx abstracts child-process spawning so adapters stay testable
// (inject FakeRunner) and Windows specifics live in one place.
//
// Key decisions:
//   - Prompts travel via stdin, never argv: npm-installed CLIs are .cmd shims
//     on Windows and Go's os/exec rejects cmd metacharacters (quotes,
//     newlines, %) in .bat/.cmd argv since the CVE-2024-24576 fix. Stdin also
//     sidesteps the ~32K command-line length limit.
//   - Kill() terminates the whole process tree (cmd -> node chains), see
//     kill_windows.go / kill_unix.go.
package execx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"sync/atomic"
)

// Spec describes one child process invocation.
type Spec struct {
	Command string   // binary name or path; resolved via exec.LookPath (finds .cmd via PATHEXT)
	Args    []string // passed token-by-token, never through a shell
	Dir     string   // working directory ("" = inherit)
	Stdin   string   // if non-empty, written to child stdin then closed; child always sees EOF
	Env     []string // appended to os.Environ()
}

// Proc is a started child process.
type Proc interface {
	Stdout() io.Reader
	Stderr() io.Reader
	// Wait blocks until the process exits and returns its exit code.
	// Callers must finish reading Stdout/Stderr before calling Wait.
	// err is non-nil only for failures other than a non-zero exit.
	Wait() (exitCode int, err error)
	// Kill terminates the whole process tree. Idempotent.
	Kill() error
}

// Runner spawns processes. The only implementation touching os/exec.
type Runner interface {
	Start(ctx context.Context, spec Spec) (Proc, error)
}

// OSRunner is the real implementation. Cancelling ctx kills the process tree.
type OSRunner struct{}

// NewOSRunner returns the real process runner.
func NewOSRunner() OSRunner { return OSRunner{} }

// Start launches the process described by spec.
func (OSRunner) Start(ctx context.Context, spec Spec) (Proc, error) {
	path, err := exec.LookPath(spec.Command)
	if err != nil {
		return nil, fmt.Errorf("resolve %s: %w", spec.Command, err)
	}
	cmd := exec.Command(path, spec.Args...)
	cmd.Dir = spec.Dir
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}
	setSysProcAttr(cmd)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	var stdin io.WriteCloser
	if spec.Stdin != "" {
		if stdin, err = cmd.StdinPipe(); err != nil {
			return nil, err
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.Command, err)
	}
	if stdin != nil {
		go func() {
			_, _ = io.WriteString(stdin, spec.Stdin)
			_ = stdin.Close()
		}()
	}
	p := &osProc{cmd: cmd, stdout: stdout, stderr: stderr, done: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done:
		}
	}()
	return p, nil
}

type osProc struct {
	cmd      *exec.Cmd
	stdout   io.Reader
	stderr   io.Reader
	done     chan struct{}
	waitOnce sync.Once
	exit     int
	werr     error
	killed   atomic.Bool
}

func (p *osProc) Stdout() io.Reader { return p.stdout }
func (p *osProc) Stderr() io.Reader { return p.stderr }

func (p *osProc) Wait() (int, error) {
	p.waitOnce.Do(func() {
		defer close(p.done)
		err := p.cmd.Wait()
		if err == nil {
			p.exit = 0
			return
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			p.exit = ee.ExitCode()
		} else {
			p.exit = -1
			p.werr = err
		}
	})
	return p.exit, p.werr
}

func (p *osProc) Kill() error {
	if !p.killed.CompareAndSwap(false, true) {
		return nil
	}
	return killTree(p.cmd)
}
