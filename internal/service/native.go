package service

import "runtime"

// For returns the manager of this operating system's service manager, for a
// system-wide service or one of the current user.
func For(system bool) (Manager, error) {
	switch runtime.GOOS {
	case "linux":
		return &Systemd{System: system, Run: Run}, nil
	case "darwin":
		return &Launchd{System: system, Run: Run}, nil
	}
	return nil, ErrUnsupported
}
