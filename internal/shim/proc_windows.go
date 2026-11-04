//go:build windows

package shim

import (
	"os"

	"golang.org/x/sys/windows"

	"github.com/robert-mcdermott/moorage/internal/platform"
)

// osProc holds the Job Object that contains the app and everything it
// starts. The job is closed when the shim exits (or dies), which kills the
// whole tree.
type osProc struct {
	job windows.Handle
}

func startProcess(p *proc) error {
	job, err := platform.StartInJob(p.cmd, p.limits)
	if err != nil {
		return err
	}
	p.osProc.job = job
	return nil
}

// signal asks the app to stop with CTRL_BREAK, whatever signal is named:
// console programs, Go programs among them, see it as an interrupt.
func (p *proc) signal(name string) error {
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(p.cmd.Process.Pid))
}

// kill ends the whole tree.
func (p *proc) kill() {
	if p.osProc.job != 0 {
		windows.TerminateJobObject(p.osProc.job, 1)
		return
	}
	p.cmd.Process.Kill()
}

// cleanup ends what is left of the tree and releases the job.
func (p *proc) cleanup() {
	if p.osProc.job != 0 {
		windows.TerminateJobObject(p.osProc.job, 1)
		windows.CloseHandle(p.osProc.job)
		p.osProc.job = 0
	}
}

func signalOf(*os.ProcessState) string { return "" }
