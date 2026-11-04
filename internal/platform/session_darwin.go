package platform

import "golang.org/x/sys/unix"

// killSession kills all that is in a session, including what is in process groups of its own, as a shell puts each job,
// and what has outlived whoever started it. It goes by the session and not the terminal, as this system stops saying which
// terminal something has as soon as whoever led its session has gone.
func killSession(leader int) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return
	}
	for i := range all {
		pid := int(all[i].Proc.P_pid)
		if pid <= 0 {
			continue
		}
		if sid, err := unix.Getsid(pid); err == nil && sid == leader {
			unix.Kill(pid, unix.SIGKILL)
		}
	}
}
