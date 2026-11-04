//go:build windows

package platform

import (
	"os/exec"
	"runtime"
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
func StartTree(cmd *exec.Cmd) (*Tree, error) { return StartTreeLimited(cmd, TreeLimits{}) }

// StartTreeLimited starts cmd inside a new Job Object that holds it to limits.
func StartTreeLimited(cmd *exec.Cmd, limits TreeLimits) (*Tree, error) {
	job, err := StartInJob(cmd, limits)
	if err != nil {
		return nil, err
	}
	return &Tree{cmd: cmd, job: job}, nil
}

// How the system takes the CPU limit, which is not in the package of its calls.
const (
	jobObjectCPURateControlInformation = 15
	jobCPURateControlEnable            = 0x1
	jobCPURateControlHardCap           = 0x4
)

type jobCPURateControl struct {
	ControlFlags uint32
	CPURate      uint32
}

// ntResumeProcess lets a process go that was started suspended.
var ntResumeProcess = windows.NewLazySystemDLL("ntdll.dll").NewProc("NtResumeProcess")

// StartInJob starts a command in a new Job Object, and in a new process group, so that CTRL_BREAK reaches it alone. It
// starts suspended and goes on only once it is in the job, so that nothing it starts can get away. The job is closed
// with the process that holds this handle, or when it dies, and that kills all that is in it.
func StartInJob(cmd *exec.Cmd, limits TreeLimits) (windows.Handle, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_SUSPENDED
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE},
	}
	if limits.Memory > 0 {
		// What it asks for beyond this is refused, and it is not killed.
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_JOB_MEMORY
		info.JobMemoryLimit = uintptr(limits.Memory)
	}
	if limits.Processes > 0 {
		info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_ACTIVE_PROCESS
		info.BasicLimitInformation.ActiveProcessLimit = uint32(limits.Processes)
	}
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	if limits.CPU > 0 {
		// In hundredths of a percent of all that this computer has.
		rate := uint32(limits.CPU / float64(runtime.NumCPU()) * 10000)
		if rate < 1 {
			rate = 1
		}
		if rate > 10000 {
			rate = 10000
		}
		control := jobCPURateControl{ControlFlags: jobCPURateControlEnable | jobCPURateControlHardCap, CPURate: rate}
		if _, err := windows.SetInformationJobObject(job, jobObjectCPURateControlInformation,
			uintptr(unsafe.Pointer(&control)), uint32(unsafe.Sizeof(control))); err != nil {
			windows.CloseHandle(job)
			return 0, err
		}
	}
	if err := cmd.Start(); err != nil {
		windows.CloseHandle(job)
		return 0, err
	}
	fail := func(err error) (windows.Handle, error) {
		cmd.Process.Kill()
		cmd.Wait()
		windows.CloseHandle(job)
		return 0, err
	}
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.PROCESS_SUSPEND_RESUME, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(job, h); err != nil {
		return fail(err)
	}
	if status, _, _ := ntResumeProcess.Call(uintptr(h)); status != 0 {
		return fail(windows.NTStatus(status))
	}
	return job, nil
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
