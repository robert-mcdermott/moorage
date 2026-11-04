//go:build !linux && !darwin && !windows

package metrics

import "context"

func readProcesses(context.Context) (map[int]proc, error) { return nil, nil }
func hostLoad() (float64, bool)                           { return 0, false }
func hostMemoryUsed() (uint64, bool)                      { return 0, false }
