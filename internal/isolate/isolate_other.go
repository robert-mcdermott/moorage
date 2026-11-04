//go:build !linux

package isolate

import (
	"errors"
	"os/exec"
)

func resolve(cfg Config) (Status, error) { return settle(cfg, false, 0, false) }

func probe(cfg Config) (Status, error) { return resolve(cfg) }

func ownerOf(string) (uint32, uint32, bool) { return 0, 0, false }

// Wrap would apply s to cmd, but there is nothing on this system to apply.
func Wrap(_ *exec.Cmd, s *Spec) (func(), error) {
	if s != nil {
		return nil, errors.New("this system has no isolation to apply")
	}
	return func() {}, nil
}

// OOMKills says how many times the kernel has killed what is in a cgroup for using too much memory.
func OOMKills(*Spec) int { return 0 }

// Remove lets go of a cgroup.
func Remove(*Spec) {}

// Main is the helper, which only Linux has.
func Main([]string) int { return 127 }
