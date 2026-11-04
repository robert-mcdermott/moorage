package cli

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/names"
)

type fileFlags struct {
	appFlags
	env string
}

func (f *fileFlags) add(cmd *cobra.Command) {
	f.appFlags.add(cmd)
	cmd.Flags().StringVar(&f.env, "env", names.Production, "environment")
}

func newFilesCmd() *cobra.Command {
	var f fileFlags
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "files [PATH]",
		Short: "Look at what is in an app's data directory",
		Long: `An environment's data directory is where its app keeps what it means to keep, and each
environment has its own. This lists a directory within it, from the top if you do not say
which. A link that leads out of it is not followed, and neither is anything else an
app might leave there to mislead the server.`,
		Example: `  moor files
  moor files uploads --env staging
  moor files get backups/nightly.sql
  moor files put seed.sql
  moor files rm old.log`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			at := ""
			if len(args) > 0 {
				at = args[0]
			}
			list, err := c.DataFiles(cmd.Context(), app, f.env, at)
			if err != nil {
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), list)
			}
			if len(list.Files) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "There is nothing here.")
				return nil
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			fmt.Fprintln(w, "NAME\tSIZE\tMODIFIED")
			for _, file := range list.Files {
				name, size := file.Name, humanBytes(file.Size)
				switch file.Kind {
				case "directory":
					name, size = name+"/", "-"
				case "link":
					name, size = name+"@", "-"
				case "other":
					name, size = name+"?", "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\n", name, size, ago(file.Modified))
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if list.More {
				fmt.Fprintln(cmd.OutOrStdout(), "There is more than this.")
			}
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	cmd.AddCommand(newFilesGetCmd(), newFilesPutCmd(), newFilesMkdirCmd(), newFilesRmCmd())
	return cmd
}

func newFilesGetCmd() *cobra.Command {
	var f fileFlags
	cmd := &cobra.Command{
		Use:   "get PATH [DESTINATION]",
		Short: "Copy a file out of a data directory",
		Long:  "Copy a file out of a data directory, to the name it has here, or to DESTINATION. - is standard output.",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			to := path.Base(args[0])
			if len(args) > 1 {
				to = args[1]
			}
			from, err := c.GetDataFile(cmd.Context(), app, f.env, args[0])
			if err != nil {
				return err
			}
			defer from.Close()
			if to == "-" {
				_, err := io.Copy(cmd.OutOrStdout(), from)
				return err
			}
			if info, err := os.Stat(to); err == nil && info.IsDir() {
				to = filepath.Join(to, path.Base(args[0]))
			}
			out, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
			if err != nil {
				return err
			}
			n, err := io.Copy(out, from)
			if closed := out.Close(); err == nil {
				err = closed
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "Copied %s to %s.\n", humanBytes(n), to)
			return nil
		},
	}
	f.add(cmd)
	return cmd
}

func newFilesPutCmd() *cobra.Command {
	var f fileFlags
	var replace bool
	cmd := &cobra.Command{
		Use:   "put FILE [PATH]",
		Short: "Copy a file into a data directory",
		Long: `Copy a file into a data directory, at the top with the name it has unless PATH says where. It
appears all at once, belongs to the app, and stops at something already there unless
--replace says otherwise. Up to 1 GB. The directory has to be there: moor files mkdir.`,
		Example: `  moor files put seed.sql
  moor files put build/index.bin indexes/current.bin --replace`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			to := filepath.Base(args[0])
			if len(args) > 1 {
				to = args[1]
			}
			from, err := os.Open(args[0])
			if err != nil {
				return err
			}
			defer from.Close()
			put, err := c.PutDataFile(cmd.Context(), app, f.env, to, from, replace)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is in the data directory of %s of %s, at %s.\n", humanBytes(put.Size), f.env, app, to)
			return nil
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVar(&replace, "replace", false, "take the place of what is already there")
	return cmd
}

func newFilesMkdirCmd() *cobra.Command {
	var f fileFlags
	cmd := &cobra.Command{
		Use:   "mkdir PATH",
		Short: "Make a directory in a data directory",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			return c.MakeDataDir(cmd.Context(), app, f.env, args[0])
		},
	}
	f.add(cmd)
	return cmd
}

func newFilesRmCmd() *cobra.Command {
	var f fileFlags
	var recursive bool
	cmd := &cobra.Command{
		Use:   "rm PATH",
		Short: "Remove something from a data directory",
		Long:  "Remove a file, a link or an empty directory from a data directory, or with -r a directory and all that is in it. There is no way back but a backup.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			app, c, err := f.open()
			if err != nil {
				return err
			}
			return c.RemoveData(cmd.Context(), app, f.env, args[0], recursive)
		},
	}
	f.add(cmd)
	cmd.Flags().BoolVarP(&recursive, "recursive", "r", false, "a directory and all that is in it")
	return cmd
}
