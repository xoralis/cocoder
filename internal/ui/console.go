// Package ui renders normalized adapter events to the console with
// role-tagged, colored prefixes.
package ui

import (
	"fmt"
	"io"
	"sync"

	"github.com/fatih/color"

	"github.com/xoralis/cocoder/internal/adapter"
)

// palette cycles deterministically over readable colors, assigned per role.
var palette = []*color.Color{
	color.New(color.FgCyan),
	color.New(color.FgGreen),
	color.New(color.FgMagenta),
	color.New(color.FgYellow),
	color.New(color.FgBlue),
	color.New(color.FgHiRed),
}

// Console renders events and general messages. Markers are ASCII so legacy
// conhost stays readable; colors go through fatih/color (go-colorable
// handles Windows). Pass color.Output as w to get ANSI on Windows.
type Console struct {
	w       io.Writer
	verbose bool
	mu      sync.Mutex
	roleClr map[string]*color.Color
	nextClr int
	dimClr  *color.Color
	toolClr *color.Color
	errClr  *color.Color
	okClr   *color.Color
	warnClr *color.Color
}

// NewConsole builds a console writer; verbose surfaces child stderr lines.
func NewConsole(w io.Writer, verbose bool) *Console {
	return &Console{
		w:       w,
		verbose: verbose,
		roleClr: map[string]*color.Color{},
		dimClr:  color.New(color.Faint),
		toolClr: color.New(color.FgYellow),
		errClr:  color.New(color.FgHiRed, color.Bold),
		okClr:   color.New(color.FgGreen, color.Bold),
		warnClr: color.New(color.FgYellow, color.Bold),
	}
}

func (c *Console) colorFor(role string) *color.Color {
	if cl, ok := c.roleClr[role]; ok {
		return cl
	}
	cl := palette[c.nextClr%len(palette)]
	c.nextClr++
	c.roleClr[role] = cl
	return cl
}

// prefix renders "[taskID/role] " in the role's color.
func (c *Console) prefix(e adapter.Event) string {
	tag := fmt.Sprintf("[%s/%s]", e.TaskID, e.Role)
	return c.colorFor(e.Role).Sprint(tag) + " "
}

// Handle renders one event. Route every adapter event through here.
func (c *Console) Handle(e adapter.Event) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch e.Kind {
	case adapter.EvTaskStarted:
		fmt.Fprintln(c.w, c.prefix(e)+c.dimClr.Sprint(">>> "+e.Text))
	case adapter.EvAgentText:
		c.writeBlock(e, e.Text)
	case adapter.EvToolUse:
		s := e.Tool
		if e.Text != "" {
			s += " " + e.Text
		}
		fmt.Fprintln(c.w, c.prefix(e)+c.toolClr.Sprint("> "+s))
	case adapter.EvFileChanged:
		fmt.Fprintln(c.w, c.prefix(e)+c.toolClr.Sprint("~ "+e.Path))
	case adapter.EvStderrLine:
		if c.verbose {
			fmt.Fprintln(c.w, c.prefix(e)+c.dimClr.Sprint("! "+e.Text))
		}
	case adapter.EvResult:
		c.writeResult(e)
	}
}

// writeBlock prints possibly-multiline agent text, prefixing each line.
func (c *Console) writeBlock(e adapter.Event, text string) {
	p := c.prefix(e)
	start := 0
	for i := 0; i < len(text); i++ {
		if text[i] == '\n' {
			fmt.Fprintln(c.w, p+text[start:i])
			start = i + 1
		}
	}
	if start < len(text) {
		fmt.Fprintln(c.w, p+text[start:])
	}
}

func (c *Console) writeResult(e adapter.Event) {
	r := e.Result
	if r == nil {
		return
	}
	p := c.prefix(e)
	switch r.Status {
	case adapter.StatusSucceeded:
		line := c.okClr.Sprint("OK")
		if r.CostUSD > 0 {
			line += c.dimClr.Sprintf("  $%.4f", r.CostUSD)
		}
		if r.NumTurns > 0 {
			line += c.dimClr.Sprintf("  %d turns", r.NumTurns)
		}
		fmt.Fprintln(c.w, p+line)
	case adapter.StatusInterrupted:
		fmt.Fprintln(c.w, p+c.warnClr.Sprint("INTERRUPTED"))
	default:
		msg := "FAILED"
		if r.ErrMsg != "" {
			msg += ": " + firstLine(r.ErrMsg)
		}
		fmt.Fprintln(c.w, p+c.errClr.Sprint(msg))
	}
}

// Printf writes a plain console message (not tied to a task).
func (c *Console) Printf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(c.w, format+"\n", a...)
}

// Successf writes a green message.
func (c *Console) Successf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(c.w, c.okClr.Sprintf(format, a...))
}

// Warnf writes a yellow message.
func (c *Console) Warnf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(c.w, c.warnClr.Sprintf(format, a...))
}

// Errorf writes a red message.
func (c *Console) Errorf(format string, a ...any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintln(c.w, c.errClr.Sprintf(format, a...))
}

func firstLine(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return s[:i]
		}
	}
	return s
}
