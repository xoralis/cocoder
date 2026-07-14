package execx

import (
	"context"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperProcess is re-entered as the child process by the tests below
// (stdlib os/exec's own testing pattern). It is not a real test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_EXECX_HELPER") != "1" {
		return
	}
	code := 0
	if os.Getenv("HELPER_EXIT") == "3" {
		code = 3
	}
	switch os.Getenv("HELPER_MODE") {
	case "echo":
		_, _ = io.Copy(os.Stdout, os.Stdin)
		_, _ = io.WriteString(os.Stderr, "helper stderr\n")
	case "sleep":
		time.Sleep(30 * time.Second)
	}
	os.Exit(code)
}

func helperSpec(mode string, env ...string) Spec {
	return Spec{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestHelperProcess"},
		Env:     append([]string{"GO_EXECX_HELPER=1", "HELPER_MODE=" + mode}, env...),
	}
}

// readAll drains stdout and stderr concurrently (required before Wait).
func readAll(t *testing.T, p Proc) (stdout, stderr string) {
	t.Helper()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); b, _ := io.ReadAll(p.Stdout()); stdout = string(b) }()
	go func() { defer wg.Done(); b, _ := io.ReadAll(p.Stderr()); stderr = string(b) }()
	wg.Wait()
	return
}

func TestStdinRoundTripAndExitCode(t *testing.T) {
	r := NewOSRunner()
	p, err := r.Start(context.Background(), func() Spec {
		s := helperSpec("echo", "HELPER_EXIT=3")
		s.Stdin = "hello from stdin"
		return s
	}())
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := readAll(t, p)
	exit, werr := p.Wait()
	if werr != nil {
		t.Fatalf("wait err: %v", werr)
	}
	if exit != 3 {
		t.Errorf("exit = %d, want 3", exit)
	}
	if !strings.Contains(stdout, "hello from stdin") {
		t.Errorf("stdout = %q, want the stdin roundtrip", stdout)
	}
	if !strings.Contains(stderr, "helper stderr") {
		t.Errorf("stderr = %q", stderr)
	}
}

func TestZeroExit(t *testing.T) {
	r := NewOSRunner()
	p, err := r.Start(context.Background(), helperSpec("echo"))
	if err != nil {
		t.Fatal(err)
	}
	readAll(t, p)
	exit, werr := p.Wait()
	if exit != 0 || werr != nil {
		t.Errorf("exit = %d err = %v, want 0 nil", exit, werr)
	}
}

func TestContextCancelKillsProcessTree(t *testing.T) {
	r := NewOSRunner()
	ctx, cancel := context.WithCancel(context.Background())
	p, err := r.Start(ctx, helperSpec("sleep"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	readAll(t, p) // returns on EOF when the process dies
	exit, _ := p.Wait()
	elapsed := time.Since(start)
	if elapsed > 15*time.Second {
		t.Fatalf("kill took %v; process tree not terminated", elapsed)
	}
	if exit == 0 {
		t.Errorf("exit = 0, want non-zero after kill")
	}
}

func TestMissingCommand(t *testing.T) {
	r := NewOSRunner()
	_, err := r.Start(context.Background(), Spec{Command: "definitely-not-a-real-binary-xyz"})
	if err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestFakeRunnerScripts(t *testing.T) {
	fr := &FakeRunner{Outcomes: []FakeOutcome{
		{Stdout: "one", Exit: 0},
		{Stdout: "two", Exit: 5},
	}}
	p1, _ := fr.Start(context.Background(), Spec{Command: "x"})
	out1, _ := readAll(t, p1)
	e1, _ := p1.Wait()
	p2, _ := fr.Start(context.Background(), Spec{Command: "y"})
	out2, _ := readAll(t, p2)
	e2, _ := p2.Wait()
	if out1 != "one" || e1 != 0 || out2 != "two" || e2 != 5 {
		t.Errorf("fake outcomes wrong: %q/%d %q/%d", out1, e1, out2, e2)
	}
	if len(fr.Calls) != 2 || fr.Calls[0].Command != "x" {
		t.Errorf("calls not recorded: %+v", fr.Calls)
	}
}
