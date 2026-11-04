package cli

import (
	"fmt"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/client"
)

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func printProject(cmd *cobra.Command, p apitypes.Project) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Project     %s\nYour role   %s\nEveryone    %s\n", p.Name, orDash(p.Role), orNone(p.Everyone))
	logs := "yes"
	if !p.ViewersSeeLogs {
		logs = "no"
	}
	fmt.Fprintf(out, "Viewers read logs   %s\nApps        %s\n\n", logs, orNone(strings.Join(p.Apps, ", ")))
	if len(p.Members) == 0 {
		fmt.Fprintln(out, "No members.")
		return nil
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "MEMBER\tROLE")
	for _, m := range p.Members {
		fmt.Fprintf(w, "%s\t%s\n", m.Email, m.Role)
	}
	return w.Flush()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func newProjectsCmd() *cobra.Command {
	var conn connection
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "projects",
		Short: "List the projects you have a role in",
		Long: `Apps belong to projects, whose members have roles: viewers see an app and its
logs, deployers also deploy, restart, roll back and scale it, and maintainers
also change its variables, its members and its fate. Administrators of the
server can do everything everywhere.

Every app starts in the default project, which everyone is a maintainer of until
that is changed.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			list, err := c.Projects(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tYOUR ROLE\tEVERYONE")
			for _, p := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\n", p.Name, orDash(p.Role), orNone(p.Everyone))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")

	// run makes a subcommand of a project's name and more, which prints the project.
	run := func(use, short string, args cobra.PositionalArgs, do func(*cobra.Command, *client.Client, []string) (apitypes.Project, error)) *cobra.Command {
		var conn connection
		sub := &cobra.Command{
			Use: use, Short: short, Args: args,
			RunE: func(cmd *cobra.Command, args []string) error {
				c, err := conn.client()
				if err != nil {
					return err
				}
				p, err := do(cmd, c, args)
				if err != nil {
					return err
				}
				return printProject(cmd, p)
			},
		}
		conn.addFlags(sub)
		return sub
	}
	create := run("create NAME", "Create a project", cobra.ExactArgs(1), func(cmd *cobra.Command, c *client.Client, args []string) (apitypes.Project, error) {
		return c.CreateProject(cmd.Context(), args[0])
	})
	show := run("show NAME", "Show a project, its apps and its members", cobra.ExactArgs(1), func(cmd *cobra.Command, c *client.Client, args []string) (apitypes.Project, error) {
		return c.Project(cmd.Context(), args[0])
	})
	var everyone string
	var logs bool
	set := run("set NAME", "Change who may do what in a project without being a member", cobra.ExactArgs(1), func(cmd *cobra.Command, c *client.Client, args []string) (apitypes.Project, error) {
		var change apitypes.ProjectChange
		if cmd.Flags().Changed("everyone") {
			change.Everyone = &everyone
		}
		if cmd.Flags().Changed("viewers-see-logs") {
			change.ViewersSeeLogs = &logs
		}
		return c.ChangeProject(cmd.Context(), args[0], change)
	})
	set.Flags().StringVar(&everyone, "everyone", "", "the role of those who are not members: viewer, deployer, maintainer or none")
	set.Flags().BoolVar(&logs, "viewers-see-logs", true, "whether viewers may read logs")
	var role string
	add := run("add-member NAME EMAIL", "Give someone a role in a project", cobra.ExactArgs(2), func(cmd *cobra.Command, c *client.Client, args []string) (apitypes.Project, error) {
		return c.SetMember(cmd.Context(), args[0], args[1], role)
	})
	add.Flags().StringVar(&role, "role", "deployer", "viewer, deployer or maintainer")
	remove := run("remove-member NAME EMAIL", "Take someone out of a project", cobra.ExactArgs(2), func(cmd *cobra.Command, c *client.Client, args []string) (apitypes.Project, error) {
		return c.RemoveMember(cmd.Context(), args[0], args[1])
	})
	var deleteConn connection
	del := &cobra.Command{
		Use: "delete NAME", Short: "Delete a project that has no apps", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := deleteConn.client()
			if err != nil {
				return err
			}
			if err := c.DeleteProject(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Deleted the project %s.\n", args[0])
			return nil
		},
	}
	deleteConn.addFlags(del)
	cmd.AddCommand(create, show, set, add, remove, del)
	return cmd
}

func newAuditCmd() *cobra.Command {
	var conn connection
	var f client.AuditQuery
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "audit",
		Short: "Show who did what on the server",
		Long: `Show who did what, newest first: every change through the API or the console, every
sign-in, every push and every secret revealed, whether it worked or not, kept for a
year. It takes an administrator.`,
		Example: `  moor audit -n 20
  moor audit --actor ana@example.com --action signin`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			entries, err := c.Audit(cmd.Context(), f)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), entries)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "WHEN\tWHO\tWHAT\tTO\tRESULT\tFROM")
			for _, e := range entries {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", e.At.Local().Format(time.DateTime), e.Actor, e.Action, orDash(e.Target), e.Result, orDash(e.IP))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	flags := cmd.Flags()
	flags.IntVarP(&f.Limit, "number", "n", 50, "how many, up to 200")
	flags.StringVar(&f.Actor, "actor", "", "only what this person did")
	flags.StringVar(&f.Action, "action", "", "only actions with this text")
	flags.StringVar(&f.Target, "target", "", "only what was done to this")
	flags.BoolVar(&asJSON, "json", false, "print as JSON")
	return cmd
}
