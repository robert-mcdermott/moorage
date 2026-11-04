package platform

import "golang.org/x/sys/unix"

// killSession kills all that has this terminal, including what is in process groups of its own, as a shell puts each job,
// and what has outlived whoever started it. This system does not say which session something is in.
func killSession(_ int, tty uint64) {
	all, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return
	}
	for i := range all {
		if uint64(uint32(all[i].Eproc.Tdev)) == tty && all[i].Proc.P_pid > 0 {
			unix.Kill(int(all[i].Proc.P_pid), unix.SIGKILL)
		}
	}
}
