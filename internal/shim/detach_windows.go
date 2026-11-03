//go:build windows

package shim

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

// detach gives the shim a console of its own, without a window: the app it
// starts shares that console, which is what lets the shim send it
// CTRL_BREAK.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CreationFlags: windows.CREATE_NO_WINDOW | windows.CREATE_NEW_PROCESS_GROUP,
	}
}
