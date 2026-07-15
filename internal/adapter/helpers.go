package adapter

import (
	"bufio"
	"io"
	"os"
	"strings"
)

// writeTempFile writes content to a new temp file and returns its path plus
// a cleanup func. When content is empty it returns ("", noop, nil) so
// callers can pass the result straight through.
func writeTempFile(pattern, content string) (path string, cleanup func(), err error) {
	if content == "" {
		return "", func() {}, nil
	}
	f, err := os.CreateTemp("", pattern)
	if err != nil {
		return "", func() {}, err
	}
	name := f.Name()
	if _, err := io.WriteString(f, content); err != nil {
		f.Close()
		os.Remove(name)
		return "", func() {}, err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", func() {}, err
	}
	return name, func() { os.Remove(name) }, nil
}

// parseTextStream is the plain-text fallback parser for CLIs without a
// machine-readable event stream: every stdout line becomes agent text, and
// the tail (trimmed) becomes the result summary. Final status is decided by
// exit code in pumpJSONL (sawResult stays false).
func parseTextStream(r io.Reader, emit func(Event)) (res TaskResult, sawResult bool, sessionID string) {
	br := bufio.NewReaderSize(r, 64*1024)
	var tail []string
	const keep = 30
	for {
		line, err := br.ReadString('\n')
		if s := strings.TrimRight(line, "\r\n"); s != "" {
			emit(Event{Kind: EvAgentText, Text: s})
			tail = append(tail, s)
			if len(tail) > keep {
				tail = tail[len(tail)-keep:]
			}
		}
		if err != nil {
			break
		}
	}
	res.Summary = truncate(strings.TrimSpace(strings.Join(tail, "\n")), 2000)
	return
}
