package adapter

import (
	"bufio"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/xoralis/cocoder/internal/execx"
)

// streamParser consumes a CLI's stdout, emits normalized events, and returns
// the final result info parsed from the stream (if any) plus the captured
// session id. Parsers are pure functions so golden-file tests cover them
// without spawning processes.
type streamParser func(r io.Reader, emit func(Event)) (res TaskResult, sawResult bool, sessionID string)

// pumpJSONL is the shared adapter glue: tee raw output to the log, parse
// stdout, collect stderr, wait for exit, resolve the final status, emit
// exactly one EvResult, close the channel.
func pumpJSONL(ctx context.Context, in TaskInput, p execx.Proc, ch chan<- Event, parse streamParser) {
	defer close(ch)
	emit := func(e Event) {
		e.TaskID, e.Role, e.Time = in.TaskID, in.Role, time.Now()
		ch <- e
	}
	raw := newSyncWriter(in.RawLog)
	stdout := io.Reader(p.Stdout())
	if raw != nil {
		stdout = io.TeeReader(stdout, raw)
	}
	tail := &tailBuffer{limit: 4096}

	var (
		res TaskResult
		saw bool
		sid string
		wg  sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		res, saw, sid = parse(stdout, emit)
	}()
	go func() {
		defer wg.Done()
		scanStderr(p.Stderr(), raw, tail, emit)
	}()
	wg.Wait()

	exit, werr := p.Wait()
	res.ExitCode = exit
	if sid != "" {
		res.SessionID = sid
	}
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		res.Status = StatusInterrupted
		if res.ErrMsg == "" {
			res.ErrMsg = "interrupted by user"
		}
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		res.Status = StatusFailed
		if res.ErrMsg == "" {
			res.ErrMsg = "task timed out"
		}
	case saw:
		if res.Status == "" {
			res.Status = StatusSucceeded
		}
		if res.Status == StatusFailed && res.ErrMsg == "" {
			res.ErrMsg = truncate(res.Summary, 500)
		}
	case exit == 0:
		res.Status = StatusSucceeded
	default:
		res.Status = StatusFailed
		msg := strings.TrimSpace(tail.String())
		if msg == "" && werr != nil {
			msg = werr.Error()
		}
		res.ErrMsg = truncate(msg, 1000)
	}
	emit(Event{Kind: EvResult, Result: &res})
}

// scanStderr streams child stderr into the raw log, the tail buffer (for
// error messages) and EvStderrLine events.
func scanStderr(r io.Reader, raw io.Writer, tail *tailBuffer, emit func(Event)) {
	br := bufio.NewReaderSize(r, 32*1024)
	for {
		line, err := br.ReadString('\n')
		if line != "" {
			if raw != nil {
				_, _ = io.WriteString(raw, line)
			}
			_, _ = tail.Write([]byte(line))
			if s := strings.TrimRight(line, "\r\n"); s != "" {
				emit(Event{Kind: EvStderrLine, Text: s})
			}
		}
		if err != nil {
			return
		}
	}
}

// syncWriter serializes writes from the stdout and stderr goroutines into
// the shared raw log.
type syncWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func newSyncWriter(w io.Writer) io.Writer {
	if w == nil {
		return nil
	}
	return &syncWriter{w: w}
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

// tailBuffer keeps the last `limit` bytes written to it.
type tailBuffer struct {
	limit int
	buf   []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.limit {
		t.buf = t.buf[len(t.buf)-t.limit:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string { return string(t.buf) }

// truncate limits s to n runes (CJK-safe), appending an ellipsis marker.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "..."
}
