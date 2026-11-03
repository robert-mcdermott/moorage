package platform

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// ProcessStartTime returns a value that identifies when process pid started
// (clock ticks since boot), so that a recorded process can be told apart
// from a later one that reuses its PID.
func ProcessStartTime(pid int) (int64, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	s := string(data)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	fields := strings.Fields(s[i+1:])
	if len(fields) < 20 {
		return 0, fmt.Errorf("unexpected /proc/%d/stat", pid)
	}
	return strconv.ParseInt(fields[19], 10, 64) // field 22, starttime
}
