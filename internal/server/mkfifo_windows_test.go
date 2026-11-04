//go:build windows

package server

import "errors"

func mkfifo(string) error { return errors.New("there are none here") }
