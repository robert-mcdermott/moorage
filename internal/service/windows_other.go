//go:build !windows

package service

func forWindows(bool) (Manager, error) { return nil, ErrUnsupported }
