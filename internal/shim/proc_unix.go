//go:build !windows

package shim

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// osProc is empty on Unix: the app's process group is its PID.
type osProc struct{}

func startProcess(p *proc) error {
	if p.cmd.SysProcAttr == nil {
		p.cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// The app leads its own process group, so stopping it reaches every
	// process it started.
	p.cmd.SysProcAttr.Setpgid = true
	return start(p.cmd)
}

func signalNumber(name string) (syscall.Signal, error) {
	switch strings.ToUpper(strings.TrimPrefix(strings.ToUpper(name), "SIG")) {
	case "TERM", "":
		return unix.SIGTERM, nil
	case "INT":
		return unix.SIGINT, nil
	case "QUIT":
		return unix.SIGQUIT, nil
	case "HUP":
		return unix.SIGHUP, nil
	case "USR1":
		return unix.SIGUSR1, nil
	case "USR2":
		return unix.SIGUSR2, nil
	case "KILL":
		return unix.SIGKILL, nil
	}
	return 0, fmt.Errorf("unknown signal %q", name)
}

// signal sends a signal to the app's process group.
func (p *proc) signal(name string) error {
	sig, err := signalNumber(name)
	if err != nil {
		return err
	}
	return unix.Kill(-p.cmd.Process.Pid, sig)
}

// kill kills the app's process group.
func (p *proc) kill() {
	unix.Kill(-p.cmd.Process.Pid, unix.SIGKILL)
}

// cleanup kills whatever is left of the app's process group after it
// exited, so that no stray child holds its port or output.
func (p *proc) cleanup() {
	unix.Kill(-p.cmd.Process.Pid, unix.SIGKILL)
}

func signalOf(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return unix.SignalName(ws.Signal())
	}
	return ""
}
