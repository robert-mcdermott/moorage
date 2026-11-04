//go:build windows

package platform

import (
	"errors"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Terminal is a command running in a pseudo-console: what it shows is read from this, and what is typed is written to it.
type Terminal struct {
	console         windows.Handle
	process, job    windows.Handle
	in, out         *os.File
	closeConsole    sync.Once
	closeEverything sync.Once
}

// StartInTerminal starts cmd in a new pseudo-console, and in a Job Object of its own that holds it to limits. Going through
// CreateProcess directly is the only way of giving it one, as os/exec has no way to.
func StartInTerminal(cmd *exec.Cmd, size TerminalSize, limits TreeLimits, _ func(tty string) error) (*Terminal, error) {
	var inRead, inWrite, outRead, outWrite windows.Handle
	if err := windows.CreatePipe(&inRead, &inWrite, nil, 0); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(inRead)
	if err := windows.CreatePipe(&outRead, &outWrite, nil, 0); err != nil {
		windows.CloseHandle(inWrite)
		return nil, err
	}
	defer windows.CloseHandle(outWrite)
	t := &Terminal{in: os.NewFile(uintptr(inWrite), "terminal input"), out: os.NewFile(uintptr(outRead), "terminal output")}
	fail := func(err error) (*Terminal, error) {
		t.Close()
		return nil, err
	}
	if err := windows.CreatePseudoConsole(windows.Coord{X: int16(size.Cols), Y: int16(size.Rows)}, inRead, outWrite, 0, &t.console); err != nil {
		return fail(err)
	}
	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		return fail(err)
	}
	defer attrs.Delete()
	// What this takes is the handle itself, and not a pointer to it, however it is declared.
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_PSEUDOCONSOLE, *(*unsafe.Pointer)(unsafe.Pointer(&t.console)), unsafe.Sizeof(t.console)); err != nil {
		return fail(err)
	}
	// No handles of ours, or a server with its output going somewhere would have the command's go there too.
	info := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	info.StartupInfo.Cb = uint32(unsafe.Sizeof(info))
	info.StartupInfo.Flags = windows.STARTF_USESTDHANDLES

	app, err := windows.UTF16PtrFromString(cmd.Path)
	if err != nil {
		return fail(err)
	}
	line, err := windows.UTF16PtrFromString(windows.ComposeCommandLine(cmd.Args))
	if err != nil {
		return fail(err)
	}
	var dir *uint16
	if cmd.Dir != "" {
		if dir, err = windows.UTF16PtrFromString(cmd.Dir); err != nil {
			return fail(err)
		}
	}
	block, err := environmentBlock(cmd.Environ())
	if err != nil {
		return fail(err)
	}
	if t.job, err = newJob(limits); err != nil {
		return fail(err)
	}
	var started windows.ProcessInformation
	flags := uint32(windows.EXTENDED_STARTUPINFO_PRESENT | windows.CREATE_UNICODE_ENVIRONMENT | windows.CREATE_SUSPENDED)
	if err := windows.CreateProcess(app, line, nil, nil, false, flags, &block[0], dir, &info.StartupInfo, &started); err != nil {
		return fail(err)
	}
	defer windows.CloseHandle(started.Thread)
	t.process = started.Process
	if err := windows.AssignProcessToJobObject(t.job, t.process); err != nil {
		windows.TerminateProcess(t.process, 1)
		return fail(err)
	}
	if _, err := windows.ResumeThread(started.Thread); err != nil {
		windows.TerminateProcess(t.process, 1)
		return fail(err)
	}
	return t, nil
}

// environmentBlock is what CreateProcess takes for an environment: sorted, and ended with an empty string.
func environmentBlock(env []string) ([]uint16, error) {
	sorted := append([]string(nil), env...)
	sort.Slice(sorted, func(i, j int) bool { return strings.ToUpper(sorted[i]) < strings.ToUpper(sorted[j]) })
	var block []uint16
	for _, entry := range sorted {
		if strings.IndexByte(entry, 0) >= 0 {
			return nil, errors.New("an environment variable has a NUL in it")
		}
		encoded, err := windows.UTF16FromString(entry)
		if err != nil {
			return nil, err
		}
		block = append(block, encoded...)
	}
	return append(block, 0), nil
}

// Read reads what the command shows.
func (t *Terminal) Read(p []byte) (int, error) { return t.out.Read(p) }

// Write types at the command.
func (t *Terminal) Write(p []byte) (int, error) { return t.in.Write(p) }

// Resize says that the terminal is another size now.
func (t *Terminal) Resize(size TerminalSize) error {
	return windows.ResizePseudoConsole(t.console, windows.Coord{X: int16(size.Cols), Y: int16(size.Rows)})
}

// Wait waits for the command, and says how it ended. The pseudo-console stays open until told otherwise, so that is
// when what is being read comes to an end.
func (t *Terminal) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(t.process, windows.INFINITE); err != nil {
		return -1, err
	}
	var code uint32
	err := windows.GetExitCodeProcess(t.process, &code)
	go t.closeTheConsole() // it waits for what has been written to be read
	if err != nil {
		return -1, err
	}
	return int(code), nil
}

func (t *Terminal) closeTheConsole() {
	t.closeConsole.Do(func() {
		if t.console != 0 {
			windows.ClosePseudoConsole(t.console)
		}
	})
}

// Kill kills all that is in the command's job.
func (t *Terminal) Kill() {
	if t.job != 0 {
		windows.TerminateJobObject(t.job, 1)
	}
}

// Close lets go of the terminal.
func (t *Terminal) Close() {
	t.closeEverything.Do(func() {
		t.in.Close()
		go func() {
			t.closeTheConsole()
			t.out.Close()
		}()
		if t.process != 0 {
			windows.CloseHandle(t.process)
		}
		if t.job != 0 {
			windows.CloseHandle(t.job)
		}
	})
}
