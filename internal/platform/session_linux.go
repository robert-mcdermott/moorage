package platform

import (
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// killSession kills all that is in a session, including what is in process groups of its own, as a shell puts each job,
// and what has outlived whoever started it.
func killSession(leader int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		text := string(data)
		end := strings.LastIndexByte(text, ')')
		if end < 0 {
			continue
		}
		fields := strings.Fields(text[end+1:])
		// After the name: state, parent, process group, session.
		if len(fields) > 3 && fields[3] == strconv.Itoa(leader) {
			unix.Kill(pid, unix.SIGKILL)
		}
	}
}
