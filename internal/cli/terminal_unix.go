//go:build !windows

package cli

import (
	"context"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// prepareTerminalOutput has this terminal ready for what a command shows, which on this system it already is.
func prepareTerminalOutput() (restore func()) { return func() {} }

// watchTerminalSize says when this terminal changes size.
func watchTerminalSize(ctx context.Context, fd int, changed func(rows, cols int)) {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGWINCH)
	defer signal.Stop(signals)
	for {
		select {
		case <-ctx.Done():
			return
		case <-signals:
			if cols, rows, err := term.GetSize(fd); err == nil {
				changed(rows, cols)
			}
		}
	}
}

// copyKeys sends what is typed in a terminal.
func copyKeys(to io.Writer, from *os.File) { io.Copy(to, from) }
