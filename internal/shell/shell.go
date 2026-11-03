// Package shell runs manifest commands with an embedded POSIX-compatible
// shell, so that a command string behaves the same on Linux, macOS and
// Windows (docs/05-runtime.md, "Command execution").
package shell

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"
)

// Simple reports whether command is a single simple command — a program,
// its arguments and optional VAR=value assignments in front, without pipes,
// lists, redirections or command substitutions — and if so returns the
// program's resolved path, its arguments (argv[0] first) and the
// environment to run it with: env plus the assignments. Words are expanded
// against env, and globs against the working directory dir.
func Simple(command string, env []string, dir string) (path string, argv, runEnv []string, ok bool, err error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("command %q: %w", command, err)
	}
	if len(file.Stmts) != 1 {
		return "", nil, nil, false, nil
	}
	stmt := file.Stmts[0]
	call, isCall := stmt.Cmd.(*syntax.CallExpr)
	if !isCall || len(call.Args) == 0 || len(stmt.Redirs) > 0 || stmt.Negated || stmt.Background || stmt.Coprocess {
		return "", nil, nil, false, nil
	}
	complex := false
	syntax.Walk(stmt, func(n syntax.Node) bool {
		switch n.(type) {
		case *syntax.CmdSubst, *syntax.ProcSubst:
			complex = true
		}
		return !complex
	})
	if complex {
		return "", nil, nil, false, nil
	}
	cfg := &expand.Config{
		Env:      expand.ListEnviron(env...),
		ReadDir2: func(p string) ([]os.DirEntry, error) { return os.ReadDir(joinDir(dir, p)) },
	}
	runEnv = append([]string(nil), env...)
	for _, a := range call.Assigns {
		if a.Append || a.Array != nil || a.Index != nil {
			return "", nil, nil, false, nil
		}
		value := ""
		if a.Value != nil {
			if value, err = expand.Literal(cfg, a.Value); err != nil {
				return "", nil, nil, false, err
			}
		}
		runEnv = append(runEnv, a.Name.Value+"="+value)
	}
	argv, err = expand.Fields(cfg, call.Args...)
	if err != nil {
		return "", nil, nil, false, err
	}
	if len(argv) == 0 {
		return "", nil, nil, false, nil
	}
	path, err = interp.LookPathDir(dir, expand.ListEnviron(runEnv...), argv[0])
	if err != nil {
		return "", nil, nil, false, fmt.Errorf("%s: %w", argv[0], err)
	}
	return path, argv, runEnv, true, nil
}

func joinDir(dir, p string) string {
	if p == "" || p == "." {
		return dir
	}
	if strings.HasPrefix(p, "/") || (len(p) > 1 && p[1] == ':') {
		return p
	}
	return dir + string(os.PathSeparator) + p
}

// Run runs command in the embedded shell with env in dir until ctx ends, and
// returns its exit status.
func Run(ctx context.Context, command string, env []string, dir string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	file, err := syntax.NewParser().Parse(strings.NewReader(command), "")
	if err != nil {
		return 2, fmt.Errorf("command %q: %w", command, err)
	}
	r, err := interp.New(
		interp.Env(expand.ListEnviron(env...)),
		interp.Dir(dir),
		interp.StdIO(stdin, stdout, stderr),
	)
	if err != nil {
		return 2, err
	}
	err = r.Run(ctx, file)
	var status interp.ExitStatus
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &status):
		return int(status), nil
	case ctx.Err() != nil:
		return 130, nil
	}
	return 1, err
}

// Main is `moor sh -c COMMAND`: it runs COMMAND in the embedded shell with
// this process's environment and working directory, and stops it when the
// process is interrupted or terminated.
func Main(args []string) int {
	if len(args) != 2 || args[0] != "-c" {
		fmt.Fprintln(os.Stderr, "usage: moor sh -c COMMAND")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	status, err := Run(ctx, args[1], os.Environ(), dir, os.Stdin, os.Stdout, os.Stderr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "moor sh:", err)
	}
	return status
}
