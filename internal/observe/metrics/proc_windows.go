//go:build windows

package metrics

import (
	"context"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	psapi                = windows.NewLazySystemDLL("psapi.dll")
	getProcessMemoryInfo = psapi.NewProc("GetProcessMemoryInfo")
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	globalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
)

type processMemoryCounters struct {
	cb                         uint32
	pageFaultCount             uint32
	peakWorkingSetSize         uintptr
	workingSetSize             uintptr
	quotaPeakPagedPoolUsage    uintptr
	quotaPagedPoolUsage        uintptr
	quotaPeakNonPagedPoolUsage uintptr
	quotaNonPagedPoolUsage     uintptr
	pagefileUsage              uintptr
	peakPagefileUsage          uintptr
}

func readProcesses(context.Context) (map[int]proc, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	out := map[int]proc{}
	var entry windows.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	for err = windows.Process32First(snap, &entry); err == nil; err = windows.Process32Next(snap, &entry) {
		p := proc{ppid: int(entry.ParentProcessID)}
		h, openErr := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, entry.ProcessID)
		if openErr == nil {
			var creation, exit, kernel, user windows.Filetime
			if windows.GetProcessTimes(h, &creation, &exit, &kernel, &user) == nil {
				p.seconds = float64(fileTimeTicks(kernel)+fileTimeTicks(user)) / 1e7
			}
			var counters processMemoryCounters
			counters.cb = uint32(unsafe.Sizeof(counters))
			if ok, _, _ := getProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&counters)), uintptr(counters.cb)); ok != 0 {
				p.rss = uint64(counters.workingSetSize)
			}
			windows.CloseHandle(h)
		}
		out[int(entry.ProcessID)] = p
	}
	if err != syscall.ERROR_NO_MORE_FILES {
		return out, err
	}
	return out, nil
}

func fileTimeTicks(t windows.Filetime) uint64 {
	return uint64(t.HighDateTime)<<32 | uint64(t.LowDateTime)
}

// Windows has no load average.
func hostLoad() (float64, bool) { return 0, false }

func hostMemoryUsed() (uint64, bool) {
	var status struct {
		length                                           uint32
		memoryLoad                                       uint32
		totalPhys, availPhys                             uint64
		totalPageFile, availPageFile                     uint64
		totalVirtual, availVirtual, availExtendedVirtual uint64
	}
	status.length = uint32(unsafe.Sizeof(status))
	if ok, _, _ := globalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&status))); ok == 0 {
		return 0, false
	}
	return status.totalPhys - status.availPhys, true
}
