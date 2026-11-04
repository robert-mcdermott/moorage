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

// Close lets go of it, by which time Kill has been told to do what it does, and the process it knew of has been waited for: to
// do that now could reach something else that has been given the same number since.
func (p *piped) Close() {
	p.tree.Release()
	p.shown.Close()
}

// watchedWriter knows whether output is on its way, when it last was, and how long has gone on someone reading it.
type watchedWriter struct {
	out     io.Writer
	writing atomic.Bool
	last    atomic.Int64
	blocked atomic.Int64
}

func (w *watchedWriter) Write(b []byte) (int, error) {
	started := time.Now()
	w.writing.Store(true)
	defer func() {
		w.blocked.Add(int64(time.Since(started)))
		w.last.Store(time.Now().UnixNano())
		w.writing.Store(false)
	}()
	return w.out.Write(b)
}

// finish waits for the last of what a command showed. Once it has gone quiet for a moment, that is all, as what it left
// behind may hold its output open for ever. Nor does it go on for long if what it left behind goes on saying things: all that
// the command said fits in what was between them, and it does not take long to read. Time spent on someone reading slowly does
// not count, as they are to have all of it, unless the session has ended, in which case there is no one there.
func (w *watchedWriter) finish(drained <-chan struct{}, ctx context.Context) {
	started := time.Now()
	w.last.Store(started.UnixNano())
	blockedBefore := w.blocked.Load()
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
		if w.writing.Load() {
			continue
		}
		reading := time.Duration(w.blocked.Load() - blockedBefore)
		if time.Since(time.Unix(0, w.last.Load())) > lastOutput || time.Since(started)-reading > 5*lastOutput {
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
	stopResizing := func() {}
	if s.TTY {
		term, err := platform.StartInTerminal(cmd, s.Size, limits, func(tty string) error { return isolate.GiveTerminal(tty, spec) })
		release()
		if err != nil {
			return err
		}
		go io.Copy(term, s.In)
		// It is done with before the terminal is closed, as a change of size needs it to be there.
		stop, stopped := make(chan struct{}), make(chan struct{})
		go func() {
			defer close(stopped)
			for {
				select {
				case <-stop:
					return
				case size, ok := <-s.Resize:
					if !ok {
						return
					}
					term.Resize(size)
				}
			}
		}()
		stopResizing = func() {
			close(stop)
			<-stopped
		}
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
	defer stopResizing()
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
