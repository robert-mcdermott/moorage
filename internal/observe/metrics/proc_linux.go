//go:build linux

package metrics

import (
	"bytes"
	"context"
	"os"
	"strconv"
	"strings"
)

// The kernel counts CPU in these, on every system a Go binary runs on these days.
const clockTicks = 100

func readProcesses(context.Context) (map[int]proc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	page := uint64(os.Getpagesize())
	out := make(map[int]proc, len(entries))
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		data, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // gone already
		}
		// Its name is in brackets, and may have anything in it.
		close := bytes.LastIndexByte(data, ')')
		if close < 0 {
			continue
		}
		f := strings.Fields(string(data[close+1:]))
		if len(f) < 22 {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		utime, _ := strconv.ParseUint(f[11], 10, 64)
		stime, _ := strconv.ParseUint(f[12], 10, 64)
		rss, _ := strconv.ParseUint(f[21], 10, 64)
		out[pid] = proc{ppid: ppid, seconds: float64(utime+stime) / clockTicks, rss: rss * page}
	}
	return out, nil
}

func hostLoad() (float64, bool) {
	data, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(data))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func hostMemoryUsed() (uint64, bool) {
	data, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, false
	}
	var total, available uint64
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		switch f[0] {
		case "MemTotal:":
			total, _ = strconv.ParseUint(f[1], 10, 64)
		case "MemAvailable:":
			available, _ = strconv.ParseUint(f[1], 10, 64)
		}
	}
	if total == 0 {
		return 0, false
	}
	return (total - available) * 1024, true
}
