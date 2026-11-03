package platform

import "errors"

// ErrUnsupported is returned by process functions not available on this
// operating system.
var ErrUnsupported = errors.New("not supported on this operating system")
