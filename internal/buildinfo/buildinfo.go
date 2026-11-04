// Package buildinfo reports which version of moor is running.
package buildinfo

import (
	"runtime"
	"runtime/debug"
)

// Version is the version being developed or released. The release workflow
// refuses a tag vX.Y.Z that does not match it, so it is set to the release
// number by the last slice of each wave.
var Version = "0.5.0"

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit,omitempty"`
	Modified  bool   `json:"modified,omitempty"`
	Time      string `json:"time,omitempty"`
	GoVersion string `json:"go"`
	Platform  string `json:"platform"`
}

// Get returns the version and, when the build recorded them, the commit it
// was built from and that commit's time.
func Get() Info {
	info := Info{
		Version:   Version,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			info.Commit = s.Value
		case "vcs.time":
			info.Time = s.Value
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}

// String formats the information on one line, as `moor version` prints it:
// moor 0.1.0 (3f2a9c1d0b7e, 2026-11-02T18:44:39Z) go1.26.6 linux/amd64
func (i Info) String() string {
	s := "moor " + i.Version
	if i.Commit != "" {
		commit := i.Commit
		if len(commit) > 12 {
			commit = commit[:12]
		}
		if i.Modified {
			commit += "+modified"
		}
		s += " (" + commit
		if i.Time != "" {
			s += ", " + i.Time
		}
		s += ")"
	}
	return s + " " + i.GoVersion + " " + i.Platform
}
