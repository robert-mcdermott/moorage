// Package oneoff runs a manifest command once and waits for it: a build
// step or a release command (docs/04-build-and-deploy.md). The command and
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
}

// ExitError reports a command that ran and did not succeed.
type ExitError struct {
	Command  string
	Code     int
	TimedOut time.Duration // the timeout, if the command ran out of time
}

func (e *ExitError) Error() string {
	if e.TimedOut > 0 {
		return fmt.Sprintf("%q did not finish within %s", e.Command, e.TimedOut)
	}
	return fmt.Sprintf("%q exited with status %d", e.Command, e.Code)
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
	cmd.Stdout, cmd.Stderr = c.Stdout, c.Stderr
	cmd.WaitDelay = 2 * time.Second // do not wait forever for descendants holding the output open
	tree, err := platform.StartTree(cmd)
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
		return exitError(c.Command, cmd, err)
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
