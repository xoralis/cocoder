package execx

import (
	"context"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// FakeOutcome scripts one FakeRunner.Start call.
type FakeOutcome struct {
	Stdout           string
	Stderr           string
	Exit             int
	StartErr         error         // returned by Start itself
	Delay            time.Duration // Wait sleeps this long before returning
	BlockUntilKilled bool          // Wait blocks until Kill (tests cancellation)
}

// FakeRunner is a scriptable Runner for adapter contract tests.
// Outcomes are consumed in order; the last one is reused when exhausted.
type FakeRunner struct {
	mu       sync.Mutex
	Outcomes []FakeOutcome
	Calls    []Spec // records every Start invocation
}

// Start records the spec and returns a scripted FakeProc.
func (r *FakeRunner) Start(ctx context.Context, spec Spec) (Proc, error) {
	r.mu.Lock()
	r.Calls = append(r.Calls, spec)
	var oc FakeOutcome
	if len(r.Outcomes) > 0 {
		oc = r.Outcomes[0]
		if len(r.Outcomes) > 1 {
			r.Outcomes = r.Outcomes[1:]
		}
	}
	r.mu.Unlock()
	if oc.StartErr != nil {
		return nil, oc.StartErr
	}
	p := &FakeProc{
		oc:     oc,
		stdout: strings.NewReader(oc.Stdout),
		stderr: strings.NewReader(oc.Stderr),
		killCh: make(chan struct{}),
		done:   make(chan struct{}),
	}
	go func() {
		select {
		case <-ctx.Done():
			_ = p.Kill()
		case <-p.done:
		}
	}()
	return p, nil
}

// FakeProc implements Proc with canned output.
type FakeProc struct {
	oc       FakeOutcome
	stdout   io.Reader
	stderr   io.Reader
	killOnce sync.Once
	killCh   chan struct{}
	doneOnce sync.Once
	done     chan struct{}
	killed   atomic.Bool
}

func (p *FakeProc) Stdout() io.Reader { return p.stdout }
func (p *FakeProc) Stderr() io.Reader { return p.stderr }

func (p *FakeProc) Wait() (int, error) {
	defer p.doneOnce.Do(func() { close(p.done) })
	if p.oc.Delay > 0 {
		time.Sleep(p.oc.Delay)
	}
	if p.oc.BlockUntilKilled {
		<-p.killCh
	}
	if p.killed.Load() {
		return 1, nil
	}
	return p.oc.Exit, nil
}

func (p *FakeProc) Kill() error {
	p.killed.Store(true)
	p.killOnce.Do(func() { close(p.killCh) })
	return nil
}
