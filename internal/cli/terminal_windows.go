//go:build windows

package cli

import (
	"context"
	"errors"
	"io"
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

// copyKeys sends what is typed in a console. Ctrl-Z there is read as the end of the file, with more to come after it, and
// here it is only a key: with the terminal raw, nothing else would be able to end this.
func copyKeys(to io.Writer, from *os.File) {
	buf := make([]byte, 4096)
	for {
		n, err := from.Read(buf)
		if n > 0 {
			if _, err := to.Write(buf[:n]); err != nil {
				return
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			if _, err := to.Write([]byte{0x1a}); err != nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		case err != nil:
			return
		}
	}
}
