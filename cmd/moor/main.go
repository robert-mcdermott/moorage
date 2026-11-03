// Command moor is Moorage: the server, the command line and the shim that
// supervises each app, in one binary.
package main

import (
	"os"

	"github.com/robert-mcdermott/moorage/internal/cli"
	"github.com/robert-mcdermott/moorage/internal/shell"
	"github.com/robert-mcdermott/moorage/internal/shim"
)

func main() {
	// The server runs its helpers as modes of this binary; they are
	// dispatched before the command line is set up, to start quickly.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "shim":
			os.Exit(shim.Main(os.Args[2:]))
		case "sh":
			os.Exit(shell.Main(os.Args[2:]))
		}
	}
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
