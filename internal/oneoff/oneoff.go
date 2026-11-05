// Package oneoff runs a manifest command once and waits for it: a build
// step or a release command (spec/04-build-and-deploy.md). The command and
// everything it starts are killed together when it is cancelled or runs out
// of time.
package oneoff

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"

	"github.com/robert-mcdermott/moorage/internal/isolate"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/shell"
)

// Command is a command to run.
type Command struct {
	// Command is the manifest command. A simple command is executed
	// directly; anything else runs in the embedded shell through Shell.
	Command string
	Env     []string
	Dir     string
	// Shell is the program and arguments that run a command that is not
	// simple; the command is appended. ShellEnv is added to its environment.
	Shell    []string
	ShellEnv []string
	Stdout   io.Writer
	Stderr   io.Writer
	// Timeout, if set, bounds the run.
	Timeout time.Duration
	// Isolate says how it runs: as which user, in which cgroup, in what sandbox.
	Isolate *isolate.Spec
	// Session, if any, has someone at the other end of it, in place of Stdout and Stderr.
	Session *Session
}

// ExitError reports a command that ran and did not succeed.
type ExitError struct {
	// OutOfMemory says that the kernel killed it for going over its limit.
	OutOfMemory bool
	Command     string
	Code        int
	TimedOut    time.Duration // the timeout, if the command ran out of time
}

// Error says what happened and not what the command was: this ends up where more people read it than can read
// what the command is, and its log says that.
func (e *ExitError) Error() string {
	if e.OutOfMemory {
		return "it ran out of memory"
	}
	if e.TimedOut > 0 {
		return fmt.Sprintf("it did not finish within %s", e.TimedOut)
	}
	return fmt.Sprintf("it exited with status %d", e.Code)
}

// Run runs c and waits for it. It returns an *ExitError if the command
// exits with a non-zero status or runs out of time, and ctx's error if ctx
// ends first.
func Run(ctx context.Context, c Command) error {
	path, argv, env, simple, err := shell.Simple(c.Command, c.Env, c.Dir)
	if err != nil {
		return err
	}
	var cmd *exec.Cmd
	if simple {
		cmd = &exec.Cmd{Path: path, Args: argv, Env: env}
	} else {
		if len(c.Shell) == 0 {
			return errors.New("no shell is configured for commands that are not simple")
		}
		args := append(append([]string{}, c.Shell[1:]...), c.Command)
		cmd = exec.Command(c.Shell[0], args...)
		cmd.Env = append(append([]string{}, c.Env...), c.ShellEnv...)
	}
	cmd.Dir = c.Dir
	if c.Session != nil {
		if err := Attach(ctx, cmd, c.Command, c.Session, c.Timeout, c.Isolate.TreeLimits(), c.Isolate); err != nil {
			var exit *ExitError
			if !errors.As(err, &exit) && !errors.Is(err, ctx.Err()) {
				return fmt.Errorf("start %q: %w", c.Command, err)
			}
			return err
		}
		return nil
	}
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	cmd.WaitDelay = 2 * time.Second // do not wait forever for descendants holding the output open
	release, err := isolate.Wrap(cmd, c.Isolate)
	if err != nil {
		return fmt.Errorf("start %q: %w", c.Command, err)
	}
	defer isolate.Remove(c.Isolate)
	tree, err := platform.StartTreeLimited(cmd, c.Isolate.TreeLimits())
	release()
	if err != nil {
		return fmt.Errorf("start %q: %w", c.Command, err)
	}
	defer tree.Close()

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var timeout <-chan time.Time
	if c.Timeout > 0 {
		t := time.NewTimer(c.Timeout)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case err := <-done:
		failed := exitError(c.Command, cmd, err)
		var exit *ExitError
		if errors.As(failed, &exit) && isolate.OOMKills(c.Isolate) > 0 {
			exit.OutOfMemory = true
		}
		return failed
	case <-ctx.Done():
		tree.Kill()
		<-done
		return ctx.Err()
	case <-timeout:
		tree.Kill()
		<-done
		return &ExitError{Command: c.Command, Code: -1, TimedOut: c.Timeout}
	}
}

func exitError(command string, cmd *exec.Cmd, err error) error {
	if err == nil || errors.Is(err, exec.ErrWaitDelay) {
		if cmd.ProcessState != nil && cmd.ProcessState.ExitCode() == 0 {
			return nil
		}
	}
	if cmd.ProcessState != nil {
		return &ExitError{Command: command, Code: cmd.ProcessState.ExitCode()}
	}
	return err
}
