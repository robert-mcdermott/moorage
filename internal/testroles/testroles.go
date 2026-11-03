// Package testroles lets a test binary play the parts of the moor binary: a
// server under test starts shims and shells by running its own executable,
// and the app it deploys can be the test binary too.
package testroles

import (
	"os"
	"path/filepath"
	"time"

	"github.com/robert-mcdermott/moorage/internal/driver/native"

	"github.com/robert-mcdermott/moorage/internal/shell"
	"github.com/robert-mcdermott/moorage/internal/shim"
	"github.com/robert-mcdermott/moorage/internal/testapp"
)

// Environments that make the test binary play a shim or the shell.
var (
	ShimEnv  = []string{"MOOR_TEST_ROLE=shim"}
	ShellEnv = []string{"MOOR_TEST_ROLE=sh"}
)

// Run plays the role named by MOOR_TEST_ROLE and exits. It returns at once
// when there is none; call it first in TestMain.
func Run() {
	role := os.Getenv("MOOR_TEST_ROLE")
	if role == "" {
		return
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == role {
		args = args[1:]
	}
	switch role {
	case "shim":
		os.Exit(shim.Main(args))
	case "sh":
		os.Exit(shell.Main(args))
	case "app":
		os.Exit(testapp.Main())
	}
	os.Exit(2)
}

// AppCommand is a manifest command that runs the test binary as the app.
func AppCommand() string {
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	return "MOOR_TEST_ROLE=app '" + exe + "'"
}

// StopInstances stops every instance a server with this data directory left
// running. A server leaves its apps running when it stops — by design — so
// tests must stop them before their temporary directories are removed.
func StopInstances(dataDir string) {
	d := native.New(filepath.Join(dataDir, "run", "shims"), nil, nil)
	for _, id := range d.List() {
		d.Stop(id, 5*time.Second)
	}
}
