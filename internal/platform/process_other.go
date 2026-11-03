//go:build !linux && !darwin

package platform

// ProcessStartTime is not needed where processes are tied to Job Objects
// (Windows) and is not implemented elsewhere.
func ProcessStartTime(pid int) (int64, error) {
	return 0, ErrUnsupported
}
