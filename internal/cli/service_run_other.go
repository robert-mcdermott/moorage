//go:build !windows

package cli

import "context"

// runUnderServiceManager has nothing to do here, where a service is an ordinary process.
func runUnderServiceManager(context.Context, func(context.Context) error) (bool, error) {
	return false, nil
}
