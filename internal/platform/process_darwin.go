package platform

import "golang.org/x/sys/unix"

// ProcessStartTime returns a value that identifies when process pid started
// (microseconds since the epoch), so that a recorded process can be told
// apart from a later one that reuses its PID.
func ProcessStartTime(pid int) (int64, error) {
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return 0, err
	}
	if kp.Proc.P_pid != int32(pid) {
		return 0, unix.ESRCH
	}
	t := kp.Proc.P_starttime
	return int64(t.Sec)*1_000_000 + int64(t.Usec), nil
}
