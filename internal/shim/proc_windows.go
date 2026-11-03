//go:build windows

package shim

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// osProc holds the Job Object that contains the app and everything it
// starts. The job is closed when the shim exits (or dies), which kills the
// whole tree.
type osProc struct {
	job windows.Handle
}

func startProcess(p *proc) error {
	if p.cmd.SysProcAttr == nil {
		p.cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	// A new process group on the shim's console, so that CTRL_BREAK reaches
	// the app (as an interrupt) and not the shim.
	p.cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return err
	}
	if err := p.cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return err
	}
	// The app is assigned to the job right after it starts. A child it
	// starts in that instant escapes the job; creating the process inside
	// the job would close the gap.
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(p.cmd.Process.Pid))
	if err == nil {
		windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
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
