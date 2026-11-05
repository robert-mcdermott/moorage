package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
	"github.com/robert-mcdermott/moorage/internal/names"
)

// followRun prints what a run says, and ends as it ends: with its exit status, for scripts.
// Interrupting it stops the run.
func followRun(cmd *cobra.Command, c *client.Client, app, id string) error {
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
	run, err := c.FollowRun(ctx, app, id, func(l apitypes.LogLine) error {
		if l.Stream == "E" || l.Stream == "stderr" {
			fmt.Fprintln(errOut, l.Text)
		} else {
			fmt.Fprintln(out, l.Text)
		}
		return nil
	})
	if ctx.Err() != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := c.StopRun(stopCtx, app, id); err != nil {
			return fmt.Errorf("stopping it: %w", err)
		}
		return exitStatus{130}
	}
	if err != nil {
		return err
	}
	switch {
	case run.Status == "succeeded":
		return nil
	case run.ExitCode != nil:
		return exitStatus{*run.ExitCode}
	}
	return fmt.Errorf("the run %s %s: %s", run.ID, run.Status, run.Error)
}

// joinTerminal joins someone's terminal to a command's, and ends as it ends. In a terminal, every key goes to the command,
// Ctrl-C included, so going away takes the command ending, or the connection doing so.
func joinTerminal(cmd *cobra.Command, c *client.Client, app, env string, o client.TerminalOptions, interactive bool) error {
	fd := int(os.Stdin.Fd())
	raw := o.TTY && interactive
	if o.TTY {
		if !term.IsTerminal(fd) {
			return errors.New("standard input is not a terminal, so there is none to join: leave out -t")
		}
		cols, rows, err := term.GetSize(fd)
		if err != nil {
			return fmt.Errorf("the size of this terminal: %w", err)
		}
		o.Rows, o.Cols, o.Term = rows, cols, os.Getenv("TERM")
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt)
	defer stop()
	t, err := c.Terminal(ctx, app, env, o)
	if err != nil {
		return err
	}
	defer t.Close()
	restored := func() {}
	if raw {
		restoreOutput := prepareTerminalOutput()
		before, err := term.MakeRaw(fd)
		if err != nil {
			restoreOutput()
			return fmt.Errorf("take over this terminal: %w", err)
		}
		var once sync.Once
		restored = func() {
			once.Do(func() {
				term.Restore(fd, before)
				restoreOutput()
			})
		}
		defer restored()
		go watchTerminalSize(ctx, fd, func(rows, cols int) { t.Resize(rows, cols) })
	}
	if interactive {
		go func() {
			if raw {
				copyKeys(t, os.Stdin)
				return
			}
			io.Copy(t, cmd.InOrStdin())
			if !o.TTY {
				t.CloseInput()
			}
		}()
	} else {
		t.CloseInput()
	}
	_, err = io.Copy(cmd.OutOrStdout(), t)
	restored()
	code, problem, over := t.Ended()
	switch {
	case !over && ctx.Err() != nil:
		return exitStatus{130}
	case !over && err != nil:
		return fmt.Errorf("the connection ended: %w", err)
	case !over:
		return errors.New("the connection ended")
	case code != nil && *code == 0:
		return nil
	case code != nil:
		return exitStatus{*code}
	}
	return errors.New(problem)
}

func newExecCmd() *cobra.Command {
	var f appFlags
	var env, timeout string
	var interactive, tty bool
	cmd := &cobra.Command{
		Use:   "exec INSTANCE [flags] -- COMMAND [ARG...]",
		Short: "Run a command in a running container",
		Long: `Run a command in a container that is running, as seen in moor ps, in among what it has
open and has written, which a new one-off instance would not be. moor ends with the
command's own exit status. What is said in it is not kept.

An app that is not in a container has nothing of its own to go into, as a process
sees the same files from anywhere: moor run has them, with its variables and its user.`,
		Example: `  moor exec ins_01J… -it -- /bin/sh
  moor exec ins_01J… -- ps aux`,
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			return joinTerminal(cmd, c, app, env, client.TerminalOptions{Instance: args[0], Command: commandFrom(args[1:]), Timeout: timeout, TTY: tty}, interactive)
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().StringVar(&timeout, "timeout", "", "how long it may take, such as 10m (default 1h)")
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "send what is typed, or standard input, to it")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "give it a terminal")
	return cmd
}

func newRunCmd() *cobra.Command {
	var f appFlags
	var env, timeout string
	var detach, interactive, tty bool
	cmd := &cobra.Command{
		Use:   "run [flags] -- COMMAND [ARG...]",
		Short: "Run a command in an environment's current release",
		Long: `Run a command as a one-off instance of an environment's current release, with its
variables and data directory, and show what it says. moor ends with the command's own
exit status, and interrupting moor stops it. It takes an hour at the most, unless
--timeout says otherwise, up to a day.

With -it, it has a terminal of its own that you are at, for a shell or anything else
that asks you things. All the keys go to it, Ctrl-C too, and leaving stops it. What is said
in one is not kept, since a password can be typed into it, and what it says is not
kept from you either: its secrets are there to be seen. With -i and no -t, it has standard
input, without a terminal.`,
		Example: `  moor run -- python manage.py migrate
  moor run --env staging --timeout 10m -- ./seed.sh
  moor run --detach -- ./rebuild-index
  moor run -it -- /bin/sh
  moor run -i -- ./import.sh < data.csv`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			if interactive || tty {
				if detach {
					return errors.New("there is no one to be at it, if it is detached: leave out --detach, or -i and -t")
				}
				return joinTerminal(cmd, c, app, env, client.TerminalOptions{Command: commandFrom(args), Timeout: timeout, TTY: tty}, interactive)
			}
			run, err := c.StartRun(cmd.Context(), app, env, apitypes.NewRun{Command: commandFrom(args), Timeout: timeout})
			if err != nil {
				return err
			}
			if detach {
				fmt.Fprintf(cmd.OutOrStdout(), "Run %s is going. See it with: moor cron log %s\n", run.ID, run.ID)
				return nil
			}
			return followRun(cmd, c, app, run.ID)
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().StringVar(&timeout, "timeout", "", "how long it may take, such as 10m (default 1h)")
	cmd.Flags().BoolVar(&detach, "detach", false, "start it and return")
	cmd.Flags().BoolVarP(&interactive, "interactive", "i", false, "send what is typed, or standard input, to it")
	cmd.Flags().BoolVarP(&tty, "tty", "t", false, "give it a terminal")
	return cmd
}

func newCronCmd() *cobra.Command {
	var f appFlags
	var env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "cron",
		Short: "List an environment's cron jobs",
		Long: `Cron jobs are declared in moorage.toml as [[cron]], and run as one-off instances
of the current release. The last 100 runs of each are kept, with what they said.`,
		Example: `  moor cron
  moor cron run nightly-cleanup
  moor cron runs nightly-cleanup
  moor cron log run_01J…`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			jobs, err := c.CronJobs(cmd.Context(), app, env)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), jobs)
			}
			if len(jobs) == 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "%s of %s has no cron jobs. Declare them in moorage.toml, as [[cron]].\n", env, app)
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "JOB\tSCHEDULE\tNEXT\tLAST\tCOMMAND")
			for _, j := range jobs {
				next, last := "-", "-"
				if !j.Next.IsZero() {
					next = j.Next.Local().Format("Jan 02 15:04")
				}
				if j.Last != nil {
					last = j.Last.Status + ", " + ago(j.Last.StartedAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", j.Name, j.Schedule, next, last, oneLine(j.Command))
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newCronRunCmd(), newCronRunsCmd(), newCronLogCmd())
	return cmd
}

func newCronRunCmd() *cobra.Command {
	var f appFlags
	var env string
	var detach bool
	cmd := &cobra.Command{
		Use:   "run JOB",
		Short: "Run a cron job now, whatever its schedule says",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			run, err := c.RunCronJob(cmd.Context(), app, env, args[0])
			if err != nil {
				return err
			}
			if detach {
				fmt.Fprintf(cmd.OutOrStdout(), "Run %s is going. See it with: moor cron log %s\n", run.ID, run.ID)
				return nil
			}
			return followRun(cmd, c, app, run.ID)
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&detach, "detach", false, "start it and return")
	return cmd
}

func newCronRunsCmd() *cobra.Command {
	var f appFlags
	var env string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "runs [JOB]",
		Short: "List an environment's runs, newest first; a job's, if it is named",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			list, err := c.Runs(cmd.Context(), app, env, firstArg(args))
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No runs yet.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "RUN\tJOB\tSTATUS\tSTARTED\tTOOK\tBY")
			for _, r := range list {
				took := "-"
				if !r.FinishedAt.IsZero() {
					took = r.FinishedAt.Sub(r.StartedAt).Round(100 * time.Millisecond).String()
				}
				by := r.CreatedBy
				if by == "" {
					by = r.Trigger
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, orDash(r.Job), r.Status, ago(r.StartedAt), took, by)
			}
			return w.Flush()
		},
	}
	f.add(cmd)
	cmd.Flags().StringVar(&env, "env", names.Production, "environment")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}

func newCronLogCmd() *cobra.Command {
	var f appFlags
	cmd := &cobra.Command{
		Use:   "log RUN",
		Short: "Show what a run said, and go on showing it until it ends",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			run, err := c.FollowRun(cmd.Context(), app, args[0], func(l apitypes.LogLine) error {
				fmt.Fprintln(cmd.OutOrStdout(), l.Text)
				return nil
			})
			if err != nil {
				return err
			}
			if run.Status != "succeeded" {
				fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", run.Status, run.Error)
			}
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

// commandFrom is the command line that a server runs for what was given after --. One word is a command line as it stands, with
// whatever a shell would make of it. Several are the words of a command, each as it was given: without quoting them, the
// server's shell would read sh -c "echo a; echo b" as something else altogether.
func commandFrom(args []string) string {
	if len(args) == 1 {
		return args[0]
	}
	words := make([]string, len(args))
	for i, arg := range args {
		words[i] = shellWord(arg)
	}
	return strings.Join(words, " ")
}

// shellWord quotes a word for a POSIX shell, if it needs it. Between single quotes, everything but a single quote stands for itself.
func shellWord(word string) string {
	plain := word != ""
	for _, r := range word {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_@%+=:,./-", r)) {
			plain = false
			break
		}
	}
	if plain {
		return word
	}
	return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
}
