package service

import (
	"bytes"
	"encoding/xml"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
)

// Launchd manages a launchd job: a daemon (system-wide) or an agent of the
// current user.
type Launchd struct {
	System bool
	// Dir overrides where the job's plist is written (tests).
	Dir string
	// UID is the user whose agent is managed; 0 means os.Getuid.
	UID int
	Run Runner
}

// Label is the launchd label of the server.
const Label = "io.moorage.server"

// File is where the plist is written.
func (l *Launchd) File() string {
	dir := l.Dir
	switch {
	case dir != "":
	case l.System:
		dir = "/Library/LaunchDaemons"
	default:
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, "Library", "LaunchAgents")
	}
	return filepath.Join(dir, Label+".plist")
}

func (l *Launchd) domain() string {
	if l.System {
		return "system"
	}
	uid := l.UID
	if uid == 0 {
		uid = os.Getuid()
	}
	return "gui/" + strconv.Itoa(uid)
}

func (l *Launchd) target() string { return l.domain() + "/" + Label }

var plistTemplate = template.Must(template.New("plist").Parse(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Moorage's server, installed by moor server install. -->
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>{{.Label}}</string>
	<key>ProgramArguments</key>
	<array>{{range .Args}}
		<string>{{.}}</string>{{end}}
	</array>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>{{.Path}}</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<!-- Restarted when it fails; a server stopped on purpose stays stopped. -->
	<key>KeepAlive</key>
	<dict>
		<key>SuccessfulExit</key>
		<false/>
	</dict>
	<!-- launchd starts a job at most once in this many seconds (10 by
	     default), restarts included: apps are unreachable meanwhile. -->
	<key>ThrottleInterval</key>
	<integer>2</integer>
	<!-- Apps run under small supervisors (shims) that outlive the server:
	     when the server stops, launchd must leave them running, and the
	     server adopts them when it starts again. -->
	<key>AbandonProcessGroup</key>
	<true/>
	<key>SoftResourceLimits</key>
	<dict>
		<key>NumberOfFiles</key>
		<integer>65536</integer>
	</dict>
	<key>StandardOutPath</key>
	<string>{{.Log}}</string>
	<key>StandardErrorPath</key>
	<string>{{.Log}}</string>
</dict>
</plist>
`))

// logPath is where launchd writes the server's standard output and error.
func logPath(spec Spec) string {
	return filepath.Join(spec.DataDir, "logs", "server", "launchd.log")
}

// Render returns the plist.
func (l *Launchd) Render(spec Spec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	esc := func(s string) string {
		var b bytes.Buffer
		xml.EscapeText(&b, []byte(s))
		return b.String()
	}
	args := []string{spec.Exe, "server", "run", "--config", spec.Config}
	for i := range args {
		args[i] = esc(args[i])
	}
	var b bytes.Buffer
	err := plistTemplate.Execute(&b, map[string]any{
		"Label": Label, "Args": args, "Path": esc(spec.Path), "Log": esc(logPath(spec)),
	})
	return b.Bytes(), err
}

// Install writes the plist and, with start, loads the job (which starts
// it now and at every boot or login).
func (l *Launchd) Install(spec Spec, start bool) error {
	plist, err := l.Render(spec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath(spec)), 0o755); err != nil {
		return err
	}
	if l.loaded() {
		l.Run("launchctl", "bootout", l.target())
	}
	if err := writeFile(l.File(), plist); err != nil {
		return err
	}
	if start {
		_, err = l.Run("launchctl", "bootstrap", l.domain(), l.File())
	}
	return err
}

func (l *Launchd) loaded() bool {
	_, err := l.Run("launchctl", "print", l.target())
	return err == nil
}

// Uninstall unloads the job (stopping the server) and removes the plist.
func (l *Launchd) Uninstall() error {
	if l.loaded() {
		if _, err := l.Run("launchctl", "bootout", l.target()); err != nil {
			return err
		}
	}
	if exists(l.File()) {
		return os.Remove(l.File())
	}
	return nil
}

// Start loads the job if needed and starts the server.
func (l *Launchd) Start() error {
	if !l.loaded() {
		_, err := l.Run("launchctl", "bootstrap", l.domain(), l.File())
		return err
	}
	_, err := l.Run("launchctl", "kickstart", l.target())
	return err
}

// Stop stops the server; the job stays loaded, and a stopped server is not
// restarted until Start.
func (l *Launchd) Stop() error {
	_, err := l.Run("launchctl", "kill", "SIGTERM", l.target())
	// launchd has it waiting to start again, as it does with one that keeps failing: it is stopped already.
	if err != nil && strings.Contains(err.Error(), "No process to signal") {
		return nil
	}
	return err
}

// Restart stops the server and starts it again.
func (l *Launchd) Restart() error {
	_, err := l.Run("launchctl", "kickstart", "-k", l.target())
	return err
}

var (
	launchdState = regexp.MustCompile(`(?m)^\s*state = (.+)$`)
	launchdPID   = regexp.MustCompile(`(?m)^\s*pid = (\d+)$`)
)

// Status reads the job's state.
func (l *Launchd) Status() (Status, error) {
	st := Status{Installed: exists(l.File())}
	out, err := l.Run("launchctl", "print", l.target())
	if err != nil {
		st.Detail = "not loaded"
		return st, nil
	}
	if m := launchdState.FindSubmatch(out); m != nil {
		st.Detail = string(m[1])
		st.Running = st.Detail == "running"
		st.Starting = strings.HasPrefix(st.Detail, "spawn")
	}
	if m := launchdPID.FindSubmatch(out); m != nil {
		st.PID, _ = strconv.Atoi(string(m[1]))
	}
	return st, nil
}

// Logs says how to read the server's log.
func (l *Launchd) Logs() string {
	return "the server's log in its data directory: logs/server/moorage.log (and launchd's capture of its output, logs/server/launchd.log)"
}
