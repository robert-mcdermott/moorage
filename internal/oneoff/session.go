package oneoff

import (
	"context"
	"io"
	"os"
	"os/exec"
	"sync/atomic"
	"time"

	"github.com/robert-mcdermott/moorage/internal/isolate"
	"github.com/robert-mcdermott/moorage/internal/platform"
)

// lastOutput is how long after a command has ended to go on waiting for more from what it left behind. Someone who is
// reading slowly is another matter, and gets all of it.
const lastOutput = 2 * time.Second

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

// attached is a command that has started, with what it shows to be read.
type attached interface {
	io.Reader
	Wait() (int, error)
	Kill()
	Close()
}

// piped is a command with pipes in place of a terminal. Standard input does not belong to exec: Wait would go on waiting
// for it after the command has ended, for as long as no one typed anything. Nor does its output, for the sake of whoever is
// reading it slowly, whose output Wait would otherwise give up on.
type piped struct {
	cmd   *exec.Cmd
	tree  *platform.Tree
	shown *os.File
}

func startPiped(cmd *exec.Cmd, s *Session, limits platform.TreeLimits) (*piped, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	shown, written, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.Stdout, cmd.Stderr = written, written
	tree, err := platform.StartTreeLimited(cmd, limits)
	written.Close()
	if err != nil {
		shown.Close()
		return nil, err
	}
	go func() {
		io.Copy(stdin, s.In)
		stdin.Close()
	}()
	return &piped{cmd: cmd, tree: tree, shown: shown}, nil
}

func (p *piped) Read(b []byte) (int, error) { return p.shown.Read(b) }

func (p *piped) Wait() (int, error) {
	err := p.cmd.Wait()
	if p.cmd.ProcessState != nil {
		return p.cmd.ProcessState.ExitCode(), nil
	}
	return -1, err
}

func (p *piped) Kill() { p.tree.Kill() }

func (p *piped) Close() {
	p.tree.Close()
	p.shown.Close()
}

// watchedWriter knows whether output is on its way, and when it last was.
type watchedWriter struct {
	out     io.Writer
	writing atomic.Bool
	last    atomic.Int64
}

func (w *watchedWriter) Write(b []byte) (int, error) {
	w.writing.Store(true)
	defer w.writing.Store(false)
	defer w.last.Store(time.Now().UnixNano())
	return w.out.Write(b)
}

// finish waits for the last of what a command showed. Once it has gone quiet for a moment, that is all, as what it left
// behind may hold its output open for ever. While someone is still being sent it, it is not: they are to have all of it,
// unless the session has ended, in which case there is no one there.
func (w *watchedWriter) finish(drained <-chan struct{}, ctx context.Context) {
	w.last.Store(time.Now().UnixNano())
	t := time.NewTicker(20 * time.Millisecond)
	defer t.Stop()
	gaveUp := time.Time{}
	for {
		select {
		case <-drained:
			return
		case <-t.C:
		}
		if ctx.Err() != nil {
			if gaveUp.IsZero() {
				gaveUp = time.Now()
			}
			if time.Since(gaveUp) > lastOutput {
				return
			}
			continue
		}
		if !w.writing.Load() && time.Since(time.Unix(0, w.last.Load())) > lastOutput {
			return
		}
	}
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
	var run attached
	if s.TTY {
		term, err := platform.StartInTerminal(cmd, s.Size, limits, func(tty string) error { return isolate.GiveTerminal(tty, spec) })
		release()
		if err != nil {
			return err
		}
		go io.Copy(term, s.In)
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case size, ok := <-s.Resize:
					if !ok {
						return
					}
					term.Resize(size)
				}
			}
		}()
		run = term
	} else {
		pipes, err := startPiped(cmd, s, limits)
		release()
		if err != nil {
			return err
		}
		run = pipes
	}
	defer run.Close()
	out := &watchedWriter{out: s.Out}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		io.Copy(out, run)
	}()
	type ending struct {
		code int
		err  error
	}
	done := make(chan ending, 1)
	go func() {
		code, err := run.Wait()
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
		run.Kill()
		<-done
		failure = ctx.Err()
	case <-limit:
		run.Kill()
		<-done
		failure = &ExitError{Command: name, Code: -1, TimedOut: timeout}
	}
	// What it started goes with it, and is not left holding its output open.
	run.Kill()
	out.finish(drained, ctx)
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
