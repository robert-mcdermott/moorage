//go:build windows

package isolate

import "os/exec"

// There is no user of its own for an app here, and apps run as the server does. Limits are for the Job Object that
// holds each one, which the shim makes.
func resolve(cfg Config) (Status, error) {
	st, err := settle(cfg, false, 0, false)
	st.JobLimits = true
	return st, err
}

func probe(cfg Config) (Status, error) { return resolve(cfg) }

// Wrap has nothing to change in how a command is started.
func Wrap(*exec.Cmd, *Spec) (func(), error) { return func() {}, nil }

// OOMKills is none: what asks for too much is refused, rather than killed.
func OOMKills(*Spec) int { return 0 }

// Remove has nothing to let go of.
func Remove(*Spec) {}

// Main is the helper, which only Linux has.
func Main([]string) int { return 127 }
