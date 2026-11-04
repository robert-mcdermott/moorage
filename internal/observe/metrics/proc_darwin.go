//go:build darwin

package metrics

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func readProcesses(ctx context.Context) (map[int]proc, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/bin/ps", "-A", "-o", "pid=,ppid=,rss=,time=").Output()
	if err != nil {
		return nil, err
	}
	procs := map[int]proc{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 {
			continue
		}
		pid, err := strconv.Atoi(f[0])
		if err != nil {
			continue
		}
		ppid, _ := strconv.Atoi(f[1])
		rss, _ := strconv.ParseUint(f[2], 10, 64)
		procs[pid] = proc{ppid: ppid, seconds: parseCPUTime(f[3]), rss: rss * 1024}
	}
	return procs, nil
}

// parseCPUTime reads what ps says of time: [[dd-]hh:]mm:ss.cc.
func parseCPUTime(s string) float64 {
	days := 0.0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		days, _ = strconv.ParseFloat(d, 64)
		s = rest
	}
	total := days * 24
	for _, part := range strings.Split(s, ":") {
		v, _ := strconv.ParseFloat(part, 64)
		total = total*60 + v
	}
	return total
}

func sysctl(name string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/sbin/sysctl", "-n", name).Output()
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

func hostLoad() (float64, bool) {
	text, ok := sysctl("vm.loadavg")
	if !ok {
		return 0, false
	}
	f := strings.Fields(strings.Trim(text, "{} "))
	if len(f) == 0 {
		return 0, false
	}
	v, err := strconv.ParseFloat(f[0], 64)
	return v, err == nil
}

func hostMemoryUsed() (uint64, bool) {
	text, ok := sysctl("hw.memsize")
	if !ok {
		return 0, false
	}
	total, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "/usr/bin/vm_stat").Output()
	if err != nil {
		return 0, false
	}
	var pageSize, free uint64 = 4096, 0
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, "page size of "); i >= 0 {
			f := strings.Fields(line[i+len("page size of "):])
			if len(f) > 0 {
				if v, err := strconv.ParseUint(f[0], 10, 64); err == nil {
					pageSize = v
				}
			}
		}
		for _, name := range []string{"Pages free:", "Pages inactive:", "Pages speculative:"} {
			if rest, found := strings.CutPrefix(line, name); found {
				v, _ := strconv.ParseUint(strings.TrimSuffix(strings.TrimSpace(rest), "."), 10, 64)
				free += v
			}
		}
	}
	if free*pageSize > total {
		return 0, false
	}
	return total - free*pageSize, true
}
