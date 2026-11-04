package oneoff

import (
	"context"
	"io"
	"os/exec"
	"time"

	"github.com/robert-mcdermott/moorage/internal/isolate"
	"github.com/robert-mcdermott/moorage/internal/platform"
)

// Session is someone at the other end of a command, typing at it and seeing what it shows, in place of its output going to
// a log. What is said in one is not kept, as a password can be typed into it.
type Session struct {
	// TTY gives it a terminal of Size, in which it can be asked for passwords and draw on the screen. Without one,
	// it has pipes, with its output and errors together.
	TTY  bool
	Size platform.TerminalSize
	// Term is the kind of terminal it is, for what it runs.
	Term string
	In   io.Reader
	Out  io.Writer
	// Resize says when the terminal becomes another size.
	Resize <-chan platform.TerminalSize
}

// Attach runs a command in a session, and waits for it. It returns an *ExitError as Run does.
func Attach(ctx context.Context, cmd *exec.Cmd, name string, s *Session, timeout time.Duration, limits platform.TreeLimits, spec *isolate.Spec) error {
	if s.Term != "" {
		cmd.Env = append(cmd.Environ(), "TERM="+s.Term)
	}
	release, err := isolate.Wrap(cmd, spec)
	if err != nil {
		return err
	}
	defer isolate.Remove(spec)
	if !s.TTY {
		defer release()
		return attachPipes(ctx, cmd, name, s, timeout, limits)
	}
	term, err := platform.StartInTerminal(cmd, s.Size, limits, func(tty string) error { return isolate.GiveTerminal(tty, spec) })
	release()
	if err != nil {
		return err
	}
	defer term.Close()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		io.Copy(s.Out, term)
	}()
	go io.Copy(term, s.In)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-drained:
				return
			case size, ok := <-s.Resize:
				if !ok {
					return
				}
				term.Resize(size)
			}
		}
	}()
	type ending struct {
		code int
		err  error
	}
	done := make(chan ending, 1)
	go func() {
		code, err := term.Wait()
		done <- ending{code, err}
	}()
	var limit <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		limit = t.C
	}
	var end ending
	var failure error
	select {
	case end = <-done:
	case <-ctx.Done():
		term.Kill()
		<-done
		failure = ctx.Err()
	case <-limit:
		term.Kill()
		<-done
		failure = &ExitError{Command: name, Code: -1, TimedOut: timeout}
	}
	// What it started goes with it, and is not left holding the terminal open.
	term.Kill()
	select {
	case <-drained:
	case <-time.After(2 * time.Second):
	}
	if failure != nil {
		return failure
	}
	if end.err != nil {
		return end.err
	}
	if end.code != 0 {
		exit := &ExitError{Command: name, Code: end.code}
		if isolate.OOMKills(spec) > 0 {
			exit.OutOfMemory = true
		}
		return exit
	}
	return nil
}

func attachPipes(ctx context.Context, cmd *exec.Cmd, name string, s *Session, timeout time.Duration, limits platform.TreeLimits) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = s.In, s.Out, s.Out
	cmd.WaitDelay = 2 * time.Second
	tree, err := platform.StartTreeLimited(cmd, limits)
	if err != nil {
		return err
	}
	defer tree.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var limit <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		limit = t.C
	}
	select {
	case err := <-done:
		return exitError(name, cmd, err)
	case <-ctx.Done():
		tree.Kill()
		<-done
		return ctx.Err()
	case <-limit:
		tree.Kill()
		<-done
		return &ExitError{Command: name, Code: -1, TimedOut: timeout}
	}
}
