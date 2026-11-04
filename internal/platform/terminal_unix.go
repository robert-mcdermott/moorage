//go:build !windows

package platform

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// Terminal is a command running in a pseudo-terminal: what it shows is read from this, and what is typed is written to it.
type Terminal struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

// StartInTerminal starts cmd in a new pseudo-terminal, as the leader of its session, which is what makes it the terminal's
// own: Ctrl-C reaches it, and its children, and leaving hangs up on them. Own, if not nil, is given the terminal's path
// first, to give it to the user who will run in it. Limits are applied in another way here.
func StartInTerminal(cmd *exec.Cmd, size TerminalSize, _ TreeLimits, own func(tty string) error) (*Terminal, error) {
	ptmx, tty, err := pty.Open()
	if err != nil {
		return nil, err
	}
	defer tty.Close()
	fail := func(err error) (*Terminal, error) {
		ptmx.Close()
		return nil, err
	}
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: size.Rows, Cols: size.Cols}); err != nil {
		return fail(err)
	}
	if own != nil {
		if err := own(tty.Name()); err != nil {
			return fail(err)
		}
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid, cmd.SysProcAttr.Setctty, cmd.SysProcAttr.Ctty = true, true, 0
	if err := cmd.Start(); err != nil {
		return fail(err)
	}
	return &Terminal{cmd: cmd, ptmx: ptmx}, nil
}

// Read reads what the command shows. Everything having left the terminal is the end of that, which Linux calls an error.
func (t *Terminal) Read(p []byte) (int, error) {
	n, err := t.ptmx.Read(p)
	if errors.Is(err, unix.EIO) {
		err = io.EOF
	}
	return n, err
}

// Write types at the command.
func (t *Terminal) Write(p []byte) (int, error) { return t.ptmx.Write(p) }

// Resize says that the terminal is another size now.
func (t *Terminal) Resize(size TerminalSize) error {
	return pty.Setsize(t.ptmx, &pty.Winsize{Rows: size.Rows, Cols: size.Cols})
}

// Wait waits for the command, and says how it ended.
func (t *Terminal) Wait() (int, error) {
	err := t.cmd.Wait()
	if t.cmd.ProcessState != nil {
		return t.cmd.ProcessState.ExitCode(), nil
	}
	return -1, err
}

// Kill kills all that is in the command's session. A shell puts each job in a process group of its own, so that is not only
// the command's.
func (t *Terminal) Kill() {
	killSession(t.cmd.Process.Pid)
	unix.Kill(-t.cmd.Process.Pid, unix.SIGKILL)
}

// Close lets go of the terminal.
func (t *Terminal) Close() { t.ptmx.Close() }
