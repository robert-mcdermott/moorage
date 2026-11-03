package service

import (
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// recorder is a Runner that records the commands and answers from a table.
type recorder struct {
	calls   []string
	answers map[string]string // command prefix → output; "!" in front fails
}

func (r *recorder) run(name string, args ...string) ([]byte, error) {
	cmd := name + " " + strings.Join(args, " ")
	r.calls = append(r.calls, cmd)
	for prefix, out := range r.answers {
		if strings.HasPrefix(cmd, prefix) {
			if strings.HasPrefix(out, "!") {
				return nil, errors.New(out[1:])
			}
			return []byte(out), nil
		}
	}
	return nil, nil
}

func spec(t *testing.T) Spec {
	if runtime.GOOS == "windows" {
		t.Skip("systemd and launchd services use Unix paths, which are not absolute on Windows")
	}
	dir := t.TempDir()
	return Spec{
		Exe: "/opt/moor tools/moor", Config: "/etc/moorage/server.toml", DataDir: filepath.Join(dir, "data"),
		System: true, Path: "/usr/local/bin:/usr/bin:/bin",
	}
}

func TestSystemd(t *testing.T) {
	rec := &recorder{answers: map[string]string{"systemctl show": "ActiveState=active\nSubState=running\nMainPID=4242\n"}}
	s := &Systemd{System: true, UnitDir: t.TempDir(), Run: rec.run}
	sp := spec(t)
	if err := s.Install(sp, true); err != nil {
		t.Fatal(err)
	}
	unit, _ := os.ReadFile(s.File())
	for _, want := range []string{
		`ExecStart="/opt/moor tools/moor" server run --config /etc/moorage/server.toml`,
		`Environment=PATH=/usr/local/bin:/usr/bin:/bin`,
		"KillMode=process", "Delegate=yes", "TasksMax=infinity", "WantedBy=multi-user.target",
	} {
		if !strings.Contains(string(unit), want) {
			t.Errorf("the unit lacks %q:\n%s", want, unit)
		}
	}
	if got := strings.Join(rec.calls, "; "); got != "systemctl daemon-reload; systemctl enable --now moorage.service" {
		t.Errorf("install ran: %s", got)
	}
	st, err := s.Status()
	if err != nil || !st.Running || st.PID != 4242 || !st.Installed {
		t.Errorf("status: %+v %v", st, err)
	}
	rec.calls = nil
	if err := s.Uninstall(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.File()); err == nil {
		t.Error("the unit is still there")
	}
	if got := strings.Join(rec.calls, "; "); got != "systemctl disable --now moorage.service; systemctl daemon-reload" {
		t.Errorf("uninstall ran: %s", got)
	}

	user := &Systemd{UnitDir: t.TempDir(), Run: rec.run}
	rec.calls = nil
	user.Restart()
	unit, _ = user.Render(sp)
	if rec.calls[0] != "systemctl --user restart moorage.service" || !strings.Contains(string(unit), "WantedBy=default.target") {
		t.Errorf("a user service: %v\n%s", rec.calls, unit)
	}
	if _, err := s.Render(Spec{Exe: "moor", Config: "/x", DataDir: "/y"}); err == nil {
		t.Error("rendered a relative binary path")
	}
	if got := systemdArgs("a%b", `say "hi"`); got != `a%%b "say \"hi\""` {
		t.Errorf("systemdArgs = %s", got)
	}
}

func TestLaunchd(t *testing.T) {
	rec := &recorder{answers: map[string]string{
		"launchctl print": "system/io.moorage.server = {\n\tstate = running\n\tpid = 777\n}\n",
	}}
	l := &Launchd{System: true, Dir: t.TempDir(), Run: rec.run}
	sp := spec(t)
	sp.Config = "/Library/Application Support/Moorage/server.toml"
	if err := l.Install(sp, true); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(l.File())
	if err := xml.Unmarshal(data, new(any)); err != nil {
		t.Fatalf("the plist is not XML: %v\n%s", err, data)
	}
	for _, want := range []string{
		"<string>io.moorage.server</string>", "<string>/opt/moor tools/moor</string>",
		"<string>/Library/Application Support/Moorage/server.toml</string>",
		"<key>AbandonProcessGroup</key>\n\t<true/>", "<key>SuccessfulExit</key>\n\t\t<false/>",
		"<key>ThrottleInterval</key>\n\t<integer>2</integer>",
		"<string>/usr/local/bin:/usr/bin:/bin</string>", "launchd.log</string>",
	} {
		if !strings.Contains(string(data), want) {
			t.Errorf("the plist lacks %q:\n%s", want, data)
		}
	}
	if _, err := os.Stat(filepath.Join(sp.DataDir, "logs", "server")); err != nil {
		t.Errorf("the log directory was not made: %v", err)
	}
	// The job was loaded already (print succeeds): it is unloaded first.
	if got := strings.Join(rec.calls, "; "); got != "launchctl print system/io.moorage.server; launchctl bootout system/io.moorage.server; launchctl bootstrap system "+l.File() {
		t.Errorf("install ran: %s", got)
	}
	st, _ := l.Status()
	if !st.Running || st.PID != 777 || !st.Installed {
		t.Errorf("status: %+v", st)
	}
	rec.calls = nil
	l.Stop()
	l.Restart()
	if got := strings.Join(rec.calls, "; "); got != "launchctl kill SIGTERM system/io.moorage.server; launchctl kickstart -k system/io.moorage.server" {
		t.Errorf("stop and restart ran: %s", got)
	}

	agent := &Launchd{UID: 501, Dir: t.TempDir(), Run: (&recorder{answers: map[string]string{"launchctl print": "!not loaded"}}).run}
	if st, _ := agent.Status(); st.Running || st.Detail != "not loaded" || agent.target() != "gui/501/io.moorage.server" {
		t.Errorf("an agent: %+v %s", st, agent.target())
	}

	sp.Path = "/a&b<c"
	data, _ = l.Render(sp)
	if !strings.Contains(string(data), "/a&amp;b&lt;c") {
		t.Errorf("not escaped:\n%s", data)
	}
}
