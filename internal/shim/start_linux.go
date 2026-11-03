package shim

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"
)

// On Linux the app gets SIGKILL when its shim dies (the parent-death
// signal). Linux ties that signal to the thread that started the child, not
// the process, so every app is started from one dedicated OS thread that
// lives as long as the shim.
var (
	spawnOnce sync.Once
	spawns    = make(chan spawnRequest)
)

type spawnRequest struct {
	cmd  *exec.Cmd
	done chan error
}

func start(cmd *exec.Cmd) error {
	cmd.SysProcAttr.Pdeathsig = syscall.SIGKILL
	spawnOnce.Do(func() {
		go func() {
			runtime.LockOSThread() // never unlocked: the thread must not exit
			for req := range spawns {
				req.done <- req.cmd.Start()
			}
		}()
	})
	done := make(chan error, 1)
	spawns <- spawnRequest{cmd: cmd, done: done}
	return <-done
}
