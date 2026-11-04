//go:build windows

package cli

import (
	"context"
	"os"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// prepareTerminalOutput has this console read what a command shows as a terminal would, rather than print its escapes.
func prepareTerminalOutput() (restore func()) {
	out := windows.Handle(os.Stdout.Fd())
	var mode uint32
	if windows.GetConsoleMode(out, &mode) != nil {
		return func() {}
	}
	if windows.SetConsoleMode(out, mode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING) != nil {
		return func() {}
	}
	return func() { windows.SetConsoleMode(out, mode) }
}

// watchTerminalSize says when this console changes size. Nothing says so here, other than by looking.
func watchTerminalSize(ctx context.Context, fd int, changed func(rows, cols int)) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	lastCols, lastRows, _ := term.GetSize(fd)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if cols, rows, err := term.GetSize(fd); err == nil && (cols != lastCols || rows != lastRows) {
				lastCols, lastRows = cols, rows
				changed(rows, cols)
			}
		}
	}
}
