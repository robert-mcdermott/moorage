package service

import (
	"runtime"
	"testing"
)

// What each service manager writes, with paths that have to be escaped, some of them in the text of what really makes a definition.
func TestConfigIn(t *testing.T) {
	if runtime.GOOS != "windows" {
		for _, config := range []string{
			"/etc/moorage/server.toml",
			"/Library/Application Support/Moorage/server.toml",
			"/Volumes/Bob's Drive/R&D/100%/server.toml",
			`/odd "quoted" \\path/server.toml`,
			"/less<than>more/server.toml",
		} {
			spec := Spec{Exe: "/usr/local/bin/moor", Config: config, DataDir: "/var/lib/moorage", Path: "/usr/bin:/bin"}
			plist, err := (&Launchd{}).Render(spec)
			if err != nil {
				t.Fatal(err)
			}
			if got := ConfigIn(plist); got != config {
				t.Errorf("launchd: %q, and not %q\n%s", got, config, plist)
			}
			unit, err := (&Systemd{}).Render(spec)
			if err != nil {
				t.Fatal(err)
			}
			if got := ConfigIn(unit); got != config {
				t.Errorf("systemd: %q, and not %q\n%s", got, config, unit)
			}
		}
	}
	for command, config := range map[string]string{
		`"C:\Program Files\Moorage\moor.exe" server run --config C:\ProgramData\Moorage\server.toml`: `C:\ProgramData\Moorage\server.toml`,
		`C:\moor.exe server run --config "C:\My Data\server.toml"`:                                   `C:\My Data\server.toml`,
		`C:\moor.exe server run --config "C:\My Data\odd\"name.toml"`:                                `C:\My Data\odd"name.toml`,
		`C:\moor.exe server run`: "",
	} {
		if got := ConfigIn([]byte(command)); got != config {
			t.Errorf("%s: %q, and not %q", command, got, config)
		}
	}
	if got := ConfigIn([]byte("<plist><string>moor</string></plist>")); got != "" {
		t.Errorf("a plist with none: %q", got)
	}
}
