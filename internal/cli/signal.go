package cli

import (
	"context"
	"os"
	"os/signal"
)

// withInterrupt returns a context cancelled on the first Ctrl-C; a second
// Ctrl-C force-exits the process (after the first one we are draining the
// agent process tree and flushing state, which should be quick but might
// not be).
func withInterrupt(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt)
	go func() {
		<-ch
		cancel()
		<-ch
		os.Exit(130)
	}()
	return ctx, func() {
		signal.Stop(ch)
		cancel()
	}
}
