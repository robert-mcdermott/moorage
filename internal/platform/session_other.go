//go:build !windows && !linux && !darwin

package platform

// killSession is left to the process group, here.
func killSession(int) {}
