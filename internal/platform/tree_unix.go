//go:build !windows

package platform

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// Tree is a started command and every process it starts, so they can be
// killed together (a build step, a release command).
type Tree struct {
	cmd *exec.Cmd
}

// StartTree starts cmd as the leader of its own process group.
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &Tree{cmd: cmd}, nil
}

// StartTreeLimited is StartTree here, where limits are applied in another way.
func StartTreeLimited(cmd *exec.Cmd, _ TreeLimits) (*Tree, error) { return StartTree(cmd) }

// Kill kills every process of the tree that is still running.
func (t *Tree) Kill() {
	unix.Kill(-t.cmd.Process.Pid, unix.SIGKILL)
}

// Close kills what is left of the tree once the command has exited, and
// releases it.
func (t *Tree) Close() { t.Kill() }

// Release lets go of the tree, killing nothing: there is nothing to let go of here.
func (t *Tree) Release() {}
