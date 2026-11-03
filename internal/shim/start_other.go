//go:build !linux && !windows

package shim

import "os/exec"

// start starts the app. Where there is no parent-death signal (macOS), the
// server kills an orphaned app's process group if its shim dies.
func start(cmd *exec.Cmd) error {
	return cmd.Start()
}
