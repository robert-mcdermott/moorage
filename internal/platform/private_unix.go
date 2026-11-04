//go:build !windows

package platform

import "os"

// MakePrivate is nothing here, where what is secret is made with a mode that says so.
func MakePrivate(string) error { return nil }

// OpenForReading opens a file to read.
func OpenForReading(path string) (*os.File, error) { return os.Open(path) }
