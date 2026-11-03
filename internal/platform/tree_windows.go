//go:build windows

package platform

import (
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Tree is a started command and every process it starts, so they can be
// killed together (a build step, a release command). On Windows they share
// a Job Object.
type Tree struct {
	cmd *exec.Cmd
	job windows.Handle
}

// StartTree starts cmd inside a new Job Object.
func StartTree(cmd *exec.Cmd) (*Tree, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return nil, err
	}
	// As for shims, the process joins the job right after it starts.
	if h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid)); err == nil {
		windows.AssignProcessToJobObject(job, h)
		windows.CloseHandle(h)
	}
	return &Tree{cmd: cmd, job: job}, nil
}

// Kill kills every process of the tree that is still running.
func (t *Tree) Kill() {
	if t.job != 0 {
		windows.TerminateJobObject(t.job, 1)
		return
	}
	t.cmd.Process.Kill()
}

// Close kills what is left of the tree once the command has exited, and
// releases it.
func (t *Tree) Close() {
	if t.job != 0 {
		windows.TerminateJobObject(t.job, 1)
		windows.CloseHandle(t.job)
		t.job = 0
	}
}
