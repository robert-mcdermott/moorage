package cli

import (
	"errors"
	"fmt"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
)

func newNotificationsCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
		read   bool
	)
	cmd := &cobra.Command{
		Use:   "notifications",
		Short: "Show what you have been told, and set up where notifications go",
		Long: `Show what you have been told: of failed deployments, apps that keep crashing and cron jobs
that fail in the projects you maintain, and of your own deployments that fail. Channels and rules
send the same to webhooks, chat and email.`,
		Example: `  moor notifications
  moor notifications --read
  moor notifications channels add alerts --kind slack
  moor notifications rules add --channel alerts --project web`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			if read {
				return c.ReadNotifications(cmd.Context())
			}
			inbox, err := c.Notifications(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), inbox)
			}
			if len(inbox.Items) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "You have been told nothing yet.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			for _, n := range inbox.Items {
				mark := " "
				if n.ReadAt == nil {
					mark = "*"
				}
				where := orDash(n.App)
				if n.Env != "" && n.Env != "production" {
					where += " · " + n.Env
				}
				fmt.Fprintf(w, "%s %s\t%s\t%s\n", mark, ago(n.At), where, oneLine(n.Summary))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if inbox.Unread > 0 {
				fmt.Fprintf(cmd.OutOrStdout(), "\n%d not read yet. Mark them as read with: moor notifications --read\n", inbox.Unread)
			}
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.Flags().BoolVar(&read, "read", false, "mark all you have been told as read")
	cmd.AddCommand(newChannelsCmd(), newRulesCmd())
	return cmd
}

func newChannelsCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "channels",
		Short: "List where notifications can go",
		Long:  `List where notifications can go. Adding, testing and removing channels takes an admin, who also sees where each one goes.`,
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			channels, err := c.NotificationChannels(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), channels)
			}
			if len(channels) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "There are no channels. An admin can add one with: moor notifications channels add NAME --kind webhook")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "NAME\tKIND\tGOES TO\tLAST SENT\tPROBLEM")
			for _, ch := range channels {
				sent := "-"
				if ch.SentAt != nil {
					sent = ago(*ch.SentAt)
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", ch.Name, ch.Kind, orDash(ch.Target), sent, orDash(oneLine(ch.Error)))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newChannelAddCmd(), newChannelRemoveCmd(), newChannelTestCmd())
	return cmd
}

func newChannelAddCmd() *cobra.Command {
	var (
		conn connection
		in   apitypes.NewNotificationChannel
		to   []string
	)
	cmd := &cobra.Command{
		Use:   "add NAME",
		Short: "Add a channel",
		Long: `Add somewhere notifications can go. A webhook, slack, teams or discord channel asks for its
address, which is a secret, unless --url gives it, and also reads it from standard input. Email
asks for the mail server's password if --username gives one to sign in with. A webhook's signing
secret is shown once, here. All of it is sealed with the server's master key.`,
		Example: `  moor notifications channels add alerts --kind slack
  moor notifications channels add ops --kind webhook --url https://example.com/hook
  moor notifications channels add mail --kind email --host smtp.example.com --from moorage@example.com --to ops@example.com`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			in.Name, in.To = args[0], to
			switch in.Kind {
			case "email":
				if in.Username != "" {
					password, err := readHidden(cmd, "Password of the mail server", "the mail server's password")
					if err != nil {
						return err
					}
					in.Password = password
				}
			default:
				if in.URL == "" {
					address, err := readHidden(cmd, "Address of the webhook", "the webhook's address")
					if err != nil {
						return err
					}
					in.URL = strings.TrimSpace(address)
				}
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			created, err := c.AddNotificationChannel(cmd.Context(), in)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Added the channel %s. Send it a test with: moor notifications channels test %s\n", created.Name, created.Name)
			if created.Secret != "" {
				fmt.Fprintf(out, "\nWhat signs its messages, shown only this once:\n\n  %s\n", created.Secret)
			}
			fmt.Fprintf(out, "\nNothing goes to it until a rule says so: moor notifications rules add --channel %s\n", created.Name)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&in.Kind, "kind", "webhook", "webhook, slack, teams, discord or email")
	cmd.Flags().StringVar(&in.URL, "url", "", "the webhook's address")
	cmd.Flags().StringVar(&in.Host, "host", "", "the mail server")
	cmd.Flags().IntVar(&in.Port, "port", 0, "the mail server's port (587 if not given; 465 means TLS from the start)")
	cmd.Flags().StringVar(&in.Username, "username", "", "whom to sign in to the mail server as")
	cmd.Flags().StringVar(&in.From, "from", "", "the address mail comes from")
	cmd.Flags().StringSliceVar(&to, "to", nil, "the addresses mail goes to")
	return cmd
}

func newChannelRemoveCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "remove NAME",
		Short: "Remove a channel, and the rules that use it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			if err := c.RemoveNotificationChannel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Removed the channel %s, and the rules that used it.\n", args[0])
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

func newChannelTestCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "test NAME",
		Short: "Send a channel a test, and say how it went",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			if _, err := c.TestNotificationChannel(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "It went to %s.\n", args[0])
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}

func newRulesCmd() *cobra.Command {
	var (
		conn   connection
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "rules",
		Short: "List which events go to which channel",
		Long: `List which events go to which channel, of what you can see. A rule of the whole server takes
an admin, and one of a project or an app takes a maintainer of it.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			rules, err := c.NotificationRules(cmd.Context())
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), rules)
			}
			if len(rules) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "There are no rules you can see. Add one with: moor notifications rules add --channel NAME")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(w, "ID\tFOR\tCHANNEL\tEVENTS")
			for _, r := range rules {
				scope := "the whole server"
				switch {
				case r.App != "":
					scope = "app " + r.App
				case r.Project != "":
					scope = "project " + r.Project
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.ID, scope, r.Channel, strings.Join(r.Events, ", "))
			}
			return w.Flush()
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print as JSON")
	cmd.AddCommand(newRuleAddCmd(), newRuleRemoveCmd())
	return cmd
}

// failureEvents are what a rule asks for unless it says.
var failureEvents = []string{"deployment.failed", "instance.crashloop", "cron.failed", "server.disk_low", "backup.failed", "server.certificate_failed", "server.engine_down"}

func newRuleAddCmd() *cobra.Command {
	var (
		conn connection
		in   apitypes.NewNotificationRule
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Send events to a channel",
		Long: `Send events to a channel: of an app, or a project, or the whole server if neither is given.
Without --events it asks for failures: deployment.failed, instance.crashloop, cron.failed,
server.disk_low, backup.failed, server.certificate_failed and server.engine_down. It can also ask for instance.unhealthy and deployment.live, or for * to have them
all. A channel that is told something went wrong is also told when it has come right.`,
		Example: `  moor notifications rules add --channel alerts
  moor notifications rules add --channel alerts --project web
  moor notifications rules add --channel ops --app shop --events deployment.failed,deployment.live`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if in.Channel == "" {
				return usageError{errors.New("say which channel, with --channel")}
			}
			if len(in.Events) == 0 {
				in.Events = failureEvents
			}
			c, err := conn.client()
			if err != nil {
				return err
			}
			rule, err := c.AddNotificationRule(cmd.Context(), in)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Added the rule %s: %s goes to %s.\n", rule.ID, strings.Join(rule.Events, ", "), rule.Channel)
			return nil
		},
	}
	conn.addFlags(cmd)
	cmd.Flags().StringVar(&in.Channel, "channel", "", "the channel it goes to")
	cmd.Flags().StringVar(&in.Project, "project", "", "only for this project")
	cmd.Flags().StringVar(&in.App, "app", "", "only for this app")
	cmd.Flags().StringSliceVar(&in.Events, "events", nil, "what it asks for")
	return cmd
}

func newRuleRemoveCmd() *cobra.Command {
	var conn connection
	cmd := &cobra.Command{
		Use:   "remove ID",
		Short: "Remove a rule",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := conn.client()
			if err != nil {
				return err
			}
			if err := c.RemoveNotificationRule(cmd.Context(), args[0]); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Removed the rule.")
			return nil
		},
	}
	conn.addFlags(cmd)
	return cmd
}
