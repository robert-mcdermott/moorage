package service

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
)

// Systemd manages a systemd unit, system-wide or for the current user.
type Systemd struct {
	System bool
	// UnitDir overrides where the unit is written (tests).
	UnitDir string
	Run     Runner
}

// UnitName is the systemd unit of the server.
const UnitName = "moorage.service"

// File is where the unit is written.
func (s *Systemd) File() string {
	dir := s.UnitDir
	switch {
	case dir != "":
	case s.System:
		dir = "/etc/systemd/system"
	default:
		config, _ := os.UserConfigDir()
		dir = filepath.Join(config, "systemd", "user")
	}
	return filepath.Join(dir, UnitName)
}

var unitTemplate = template.Must(template.New("unit").Parse(`# Moorage's server, installed by moor server install.
[Unit]
Description=Moorage app server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{.Exec}}
Environment={{.Path}}
Restart=on-failure
RestartSec=2s
# Apps run under small supervisors (shims) that outlive the server: stopping
# or restarting the service stops only the server's own process, and the
# server adopts the running apps when it starts again. The service owns its
# cgroup subtree, as container engines do, and the default limit on tasks
# would otherwise cap all the apps together.
KillMode=process
Delegate=yes
TasksMax=infinity
LimitNOFILE=1048576

[Install]
WantedBy={{.WantedBy}}
`))

// Render returns the unit.
func (s *Systemd) Render(spec Spec) ([]byte, error) {
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	wanted := "default.target"
	if s.System {
		wanted = "multi-user.target"
	}
	var b bytes.Buffer
	err := unitTemplate.Execute(&b, map[string]string{
		"Exec":     systemdArgs(spec.Exe, "server", "run", "--config", spec.Config),
		"Path":     systemdArgs("PATH=" + spec.Path),
		"WantedBy": wanted,
	})
	return b.Bytes(), err
}

// systemdArgs quotes words for a unit file: double quotes when needed, and
// "%" doubled (systemd expands specifiers).
func systemdArgs(words ...string) string {
	out := make([]string, len(words))
	for i, w := range words {
		w = strings.ReplaceAll(w, "%", "%%")
		if w == "" || strings.ContainsAny(w, " \t\"'\\;$") {
			w = `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(w) + `"`
		}
		out[i] = w
	}
	return strings.Join(out, " ")
}

func (s *Systemd) ctl(args ...string) ([]byte, error) {
	if !s.System {
		args = append([]string{"--user"}, args...)
	}
	return s.Run("systemctl", args...)
}

// Install writes the unit, reloads systemd, and with start enables and
// starts the service.
func (s *Systemd) Install(spec Spec, start bool) error {
	unit, err := s.Render(spec)
	if err != nil {
		return err
	}
	if err := writeFile(s.File(), unit); err != nil {
		return err
	}
	if _, err := s.ctl("daemon-reload"); err != nil {
		return err
	}
	if start {
		_, err = s.ctl("enable", "--now", UnitName)
	}
	return err
}

// Uninstall stops and disables the service and removes the unit.
func (s *Systemd) Uninstall() error {
	if exists(s.File()) {
		if _, err := s.ctl("disable", "--now", UnitName); err != nil {
			return err
		}
		if err := os.Remove(s.File()); err != nil {
			return err
		}
	}
	_, err := s.ctl("daemon-reload")
	return err
}

func (s *Systemd) Start() error   { _, err := s.ctl("start", UnitName); return err }
func (s *Systemd) Stop() error    { _, err := s.ctl("stop", UnitName); return err }
func (s *Systemd) Restart() error { _, err := s.ctl("restart", UnitName); return err }

// Status reads the unit's state.
func (s *Systemd) Status() (Status, error) {
	st := Status{Installed: exists(s.File())}
	if !st.Installed {
		return st, nil
	}
	out, err := s.ctl("show", UnitName, "--property=ActiveState,SubState,MainPID")
	if err != nil {
		return st, err
	}
	props := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			props[k] = v
		}
	}
	st.Running = props["ActiveState"] == "active"
	st.PID, _ = strconv.Atoi(props["MainPID"])
	st.Detail = fmt.Sprintf("%s (%s)", props["ActiveState"], props["SubState"])
	return st, nil
}

// Logs says how to read the server's log.
func (s *Systemd) Logs() string {
	if s.System {
		return "journalctl -u " + UnitName + " -f"
	}
	return "journalctl --user -u " + UnitName + " -f"
}
