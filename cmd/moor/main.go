// Command moor is Moorage: the server, the CLI and (in later releases) the
// shim that supervises each app, in one binary.
package main

import (
	"os"

	"github.com/robert-mcdermott/moorage/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
