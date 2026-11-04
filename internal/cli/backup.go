package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/backup"
	"github.com/robert-mcdermott/moorage/internal/buildinfo"
	"github.com/robert-mcdermott/moorage/internal/config"
	"github.com/robert-mcdermott/moorage/internal/ids"
	"github.com/robert-mcdermott/moorage/internal/platform"
	"github.com/robert-mcdermott/moorage/internal/secrets"
	"github.com/robert-mcdermott/moorage/internal/server"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// s3FromEnvironment is an S3-compatible service's credentials, by the names that tools use for them.
func s3FromEnvironment() backup.Credentials {
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = os.Getenv("AWS_DEFAULT_REGION")
	}
	return backup.Credentials{
		Endpoint: os.Getenv("AWS_ENDPOINT_URL"), Region: region,
		AccessKey: os.Getenv("AWS_ACCESS_KEY_ID"), SecretKey: os.Getenv("AWS_SECRET_ACCESS_KEY"), SessionToken: os.Getenv("AWS_SESSION_TOKEN"),
	}
}

// backupPlace says where backups go: where it is told, and otherwise where the server was told.
func backupPlace(ctx context.Context, cfg config.Config, master secrets.MasterKey, to string) (backup.Destination, error) {
	layout := server.NewLayout(cfg.DataDir)
	if to != "" {
		return backup.Parse(to, s3FromEnvironment())
	}
	fallback := &backup.Service{DataDir: cfg.DataDir}
	// Only if there is one: opening it would make one, and a restore on a new machine would then find a server there.
	if _, err := os.Stat(layout.Database); err != nil {
		return backup.Parse(fallback.DefaultDestination(), backup.Credentials{})
	}
	db, err := store.Open(ctx, layout.Database)
	if err != nil {
		return backup.Parse(fallback.DefaultDestination(), backup.Credentials{})
	}
	defer db.Close()
	saved, err := db.BackupConfig(ctx)
	if err != nil {
		return backup.Parse(fallback.DefaultDestination(), backup.Credentials{})
	}
	keyring, err := secrets.Open(ctx, master, db)
	if err != nil {
		return nil, err
	}
	return (&backup.Service{DataDir: cfg.DataDir, Keyring: keyring}).DestinationOf(saved)
}

func newServerBackupCmd() *cobra.Command {
	var cfgFlag, to string
	var includeKeys bool
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Make a backup of this server",
		Long: `Make a backup of this server, which can go on running: its database, repositories, builds,
apps' data and certificates. It goes where --to says, or where the server has been told: a path, or
s3://bucket/prefix, with the service's details in AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY, AWS_REGION
and AWS_ENDPOINT_URL.

It leaves out the master key, without which its secrets cannot be read, so that a backup that goes
astray gives them away to nobody. Keep that key safe, and apart. --include-keys puts it and the
server's other keys in the backup, which then has to be kept as safe as they are: only to a path.`,
		Example: `  moor server backup
  moor server backup --to /mnt/backups
  moor server backup --to s3://acme-backups/moorage`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			layout := server.NewLayout(cfg.DataDir)
			master, err := secrets.LoadMasterKey(layout.MasterKey, os.Getenv, false)
			if err != nil {
				return fmt.Errorf("this takes the server's master key: %w", err)
			}
			// Stopped, it cleans up after itself, and says that it stopped.
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			dest, err := backupPlace(ctx, cfg, master, to)
			if err != nil {
				return err
			}
			if includeKeys && strings.HasPrefix(dest.String(), "s3://") {
				return usageError{errors.New("--include-keys is for a path: in a bucket, the keys would be one mistake from anyone who wants them")}
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Backing up to %s\n", dest)
			record := noteBackup(ctx, layout.Database, dest.String())
			result, err := backup.Run(ctx, dest, backup.Options{
				DataDir: cfg.DataDir, IncludeKeys: includeKeys, Keys: master, KeyFingerprint: backup.Fingerprint(master),
				Version: buildinfo.Version, Domain: cfg.Domain,
				Log: func(format string, args ...any) { fmt.Fprintf(out, "  "+format+"\n", args...) },
			})
			record(result, err)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nBacked up: %s, %d files, %s.\n", result.Name, result.Files, humanBytes(result.Size))
			if len(result.Changed) > 0 {
				fmt.Fprintf(out, "%d files changed while they were read, which is usual for a running app's: see the documentation about databases.\n", len(result.Changed))
			}
			if includeKeys {
				fmt.Fprintln(out, "It holds the master key, and with that all of this server's secrets: keep it safe.")
			} else {
				fmt.Fprintf(out, "To restore it you need the master key, which it leaves out: keep %s safe, and apart from it.\n", master.Path)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "the server's configuration file")
	cmd.Flags().StringVar(&to, "to", "", "where it goes: a path, or s3://bucket/prefix")
	cmd.Flags().BoolVar(&includeKeys, "include-keys", false, "include the master key and the server's other keys")
	cmd.AddCommand(newBackupListCmd(), newBackupVerifyCmd())
	return cmd
}

// noteBackup records a backup made from the command line beside those the server made, if it can.
func noteBackup(ctx context.Context, database, dest string) func(backup.Result, error) {
	db, err := store.Open(ctx, database)
	if err != nil {
		return func(backup.Result, error) {}
	}
	b := store.Backup{ID: ids.New(ids.Backup), StartedAt: time.Now().UTC(), Status: store.BackupRunning, Trigger: "command", Destination: dest}
	if err := db.AddBackup(ctx, b); err != nil {
		db.Close()
		return func(backup.Result, error) {}
	}
	return func(result backup.Result, failed error) {
		defer db.Close()
		b.FinishedAt, b.Status, b.Name, b.SizeBytes = time.Now().UTC(), store.BackupDone, result.Name, result.Size
		if failed != nil {
			b.Status, b.Error = store.BackupFailed, failed.Error()
		}
		_ = db.FinishBackup(context.WithoutCancel(ctx), b)
	}
}

func newBackupListCmd() *cobra.Command {
	var cfgFlag, to string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the backups that are there",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			master, err := secrets.LoadMasterKey(server.NewLayout(cfg.DataDir).MasterKey, os.Getenv, false)
			if err != nil {
				return fmt.Errorf("this takes the server's master key: %w", err)
			}
			dest, err := backupPlace(cmd.Context(), cfg, master, to)
			if err != nil {
				return err
			}
			list, err := dest.List(cmd.Context())
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(list) == 0 {
				fmt.Fprintf(out, "There are no backups at %s.\n", dest)
				return nil
			}
			w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
			fmt.Fprintf(w, "NAME\tSIZE\tMADE\n")
			for _, s := range list {
				fmt.Fprintf(w, "%s\t%s\t%s\n", s.Name, humanBytes(s.Size), ago(s.At))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nAt %s\n", dest)
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "the server's configuration file")
	cmd.Flags().StringVar(&to, "to", "", "where to look: a path, or s3://bucket/prefix")
	return cmd
}

func newBackupVerifyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "verify SOURCE",
		Short: "Read a backup through, and say whether it could be restored",
		Long: `Read a backup through, and say whether it could be restored: the path of its file, or
s3://bucket/prefix/name. Nothing changes.`,
		Example: `  moor server backup verify /mnt/backups/moorage-20261104T120000Z.tar.gz`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			r, digest, err := backup.Open(cmd.Context(), args[0], s3FromEnvironment())
			if err != nil {
				return err
			}
			defer r.Close()
			m, sum, err := backup.Verify(cmd.Context(), r, digest)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "It is complete: %d files, %s, made by Moorage %s %s.\n", sum.Files, humanBytes(sum.Bytes), m.Version, ago(m.CreatedAt))
			if digest == "" {
				fmt.Fprintln(out, "Nothing beside it says what it should hash to, so that a change to it could go unseen.")
			}
			if m.IncludesKeys {
				fmt.Fprintln(out, "It holds the master key.")
			} else {
				fmt.Fprintf(out, "To restore it you need the master key it was made with, %s.\n", m.KeyFingerprint)
			}
			return nil
		},
	}
	return cmd
}

func newServerRestoreCmd() *cobra.Command {
	var cfgFlag string
	var force bool
	cmd := &cobra.Command{
		Use:   "restore SOURCE",
		Short: "Restore a backup to this server, which has to be stopped",
		Long: `Restore a backup: the path of its file, or s3://bucket/prefix/name. The server has to be
stopped. It reads all of the backup first, so that a damaged one changes nothing, and then stops any
apps still running, and moves what the data directory held aside rather than deleting it.

A backup without keys needs the master key it was made with to be in place first.`,
		Example: `  moor server restore /mnt/backups/moorage-20261104T120000Z.tar.gz
  moor server restore s3://acme-backups/moorage/moorage-20261104T120000Z.tar.gz --force`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, cfg, err := loadServerConfig(cfgFlag)
			if err != nil {
				return err
			}
			layout := server.NewLayout(cfg.DataDir)
			if err := layout.Prepare(); err != nil {
				return err
			}
			lock, err := platform.LockFile(layout.LockFile)
			if errors.Is(err, platform.ErrLocked) {
				return errors.New("the server is running: stop it first, with moor server stop")
			}
			if err != nil {
				return err
			}
			defer lock.Unlock()
			fingerprint := ""
			var keys backup.Keyed
			if master, err := secrets.LoadMasterKey(layout.MasterKey, os.Getenv, false); err == nil {
				fingerprint, keys = backup.Fingerprint(master), master
			}
			r, digest, err := backup.Open(cmd.Context(), args[0], s3FromEnvironment())
			if err != nil {
				return err
			}
			defer r.Close()
			out := cmd.OutOrStdout()
			restored, err := backup.Restore(cmd.Context(), r, digest, backup.RestoreOptions{
				DataDir: cfg.DataDir, KeyFingerprint: fingerprint, Keys: keys, Force: force,
				Log:        func(format string, args ...any) { fmt.Fprintf(out, "  "+format+"\n", args...) },
				BeforeSwap: func() error { stopApps(cmd, cfg); return nil },
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nRestored the backup made %s, by Moorage %s: %d files.\n", ago(restored.CreatedAt), restored.Version, restored.Files)
			if restored.Aside != "" {
				fmt.Fprintf(out, "What was there is in %s: delete it once you are happy.\n", restored.Aside)
			}
			if restored.Version != buildinfo.Version {
				fmt.Fprintf(out, "It was made by %s, and this is %s: the server brings it up to date as it starts.\n", restored.Version, buildinfo.Version)
			}
			fmt.Fprintln(out, "Start the server, and it starts the apps again: moor server start")
			return nil
		},
	}
	cmd.Flags().StringVar(&cfgFlag, "config", "", "the server's configuration file")
	cmd.Flags().BoolVar(&force, "force", false, "move aside what the data directory holds, and restore over it")
	return cmd
}
