package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/robert-mcdermott/moorage/internal/build"
	"github.com/robert-mcdermott/moorage/internal/manifest"
)

func newManifestCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "manifest",
		Short: "Check and explain moorage.toml",
		Long: `moorage.toml tells Moorage how to build and run an app. These commands
check it, show what Moorage will do with an app, and print the manifest's
JSON Schema for editors.`,
		Args: cobra.NoArgs,
	}
	cmd.AddCommand(newManifestValidateCmd(), newManifestExplainCmd(), newManifestSchemaCmd())
	return cmd
}

func newManifestValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate [DIR]",
		Short: "Check moorage.toml",
		Long: `Check the moorage.toml in DIR (default: the current directory): its syntax,
keys and values. Warnings point at values that look like credentials and at
settings this version accepts but does not act on yet.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := firstOf(firstArg(args), ".")
			m, err := manifest.Load(dir)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			path := filepath.Join(dir, manifest.FileName)
			if m == nil {
				fmt.Fprintf(out, "There is no %s: Moorage detects how to build the app (see moor manifest explain).\n", path)
				return nil
			}
			warnings := manifest.Lint(m)
			for _, w := range warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", w)
			}
			fmt.Fprintf(out, "%s is valid", path)
			if len(warnings) > 0 {
				fmt.Fprintf(out, " (%d warnings)", len(warnings))
			}
			fmt.Fprintln(out, ".")
			return nil
		},
	}
}

func newManifestExplainCmd() *cobra.Command {
	var asJSON bool
	var goos string
	cmd := &cobra.Command{
		Use:   "explain [DIR]",
		Short: "Show how Moorage will build and run an app",
		Long: `Show the plan Moorage makes for the app in DIR (default: the current
directory) from its moorage.toml and its files: the runtime, the toolchains,
the build commands, the processes and their environment. The server makes
the same plan when it builds the app; toolchain versions are resolved then.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := firstOf(firstArg(args), ".")
			plan, _, err := build.Resolve(dir, goos)
			if err != nil {
				var u *build.UnresolvedError
				if errors.As(err, &u) {
					return errors.New(u.Msg)
				}
				return err
			}
			if asJSON {
				return printJSON(cmd.OutOrStdout(), plan)
			}
			explain(cmd.OutOrStdout(), plan)
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the plan as JSON")
	cmd.Flags().StringVar(&goos, "os", runtime.GOOS, "operating system of the server (linux, darwin or windows)")
	return cmd
}

func explain(w io.Writer, plan manifest.Plan) {
	section := func(name string, lines ...string) {
		if len(lines) == 0 {
			return
		}
		fmt.Fprintf(w, "%-12s%s\n", name, lines[0])
		for _, l := range lines[1:] {
			fmt.Fprintf(w, "%-12s%s\n", "", l)
		}
	}
	kind := plan.Runtime
	if plan.Kind == manifest.KindStatic {
		kind += " (a static site)"
	}
	section("runtime", kind)
	if c := plan.Container; c != nil {
		explainContainer(section, c, plan)
	}
	var tcs []string
	for _, t := range plan.Toolchains {
		v := t.Version
		if v == "" {
			v = "latest"
		}
		s := t.Name + " " + v
		if t.From != "" {
			s += "  (" + t.From + ")"
		}
		tcs = append(tcs, s)
	}
	section("toolchains", tcs...)
	var steps []string
	for _, s := range plan.Steps {
		steps = append(steps, fmt.Sprintf("%-8s %s", s.Name+":", s.Command))
	}
	if len(steps) == 0 && plan.Container == nil {
		steps = []string{"(nothing to build)"}
	}
	section("build", steps...)
	if plan.BuildTimeout > 0 && len(plan.Steps) > 0 {
		section("", "timeout "+plan.BuildTimeout.Std().String())
	}
	if plan.Kind == manifest.KindStatic || plan.StaticFallback {
		dir := "the first of dist, build, out, public, _site, site and . with an index.html"
		if plan.Static != nil && plan.Static.Dir != "" {
			dir = plan.Static.Dir
		} else if plan.StaticFallback {
			dir = "the build's dist/, build/ or out/ with an index.html"
		}
		section("static", "serves "+dir)
	}
	var procs []string
	for _, name := range manifest.SortedProcesses(plan.Processes) {
		p := plan.Processes[name]
		check := "health: tcp"
		switch {
		case p.Health.Path != "":
			check = "health: GET " + p.Health.Path
		case !*p.Expose:
			check = "health: running for " + p.Health.MinUptime.Std().String()
		}
		command := p.Command
		if command == "" {
			command = "(what the image says to run)"
		}
		procs = append(procs, fmt.Sprintf("%s: %s", name, command), fmt.Sprintf("  ×%d · %s", *p.Instances, check))
	}
	section("processes", procs...)
	if plan.Release != nil {
		var rel []string
		if plan.Release.Command != "" {
			rel = append(rel, "every deploy: "+plan.Release.Command)
		}
		if plan.Release.OnCreate != "" {
			rel = append(rel, "first deploy: "+plan.Release.OnCreate)
		}
		section("release", rel...)
	}
	if plan.Kind == manifest.KindProcess {
		section("deploy", fmt.Sprintf("%s · drain %s · keep %d releases", plan.Deploy.Strategy, plan.Deploy.Drain.Std(), plan.Deploy.KeepReleases))
	}
	if len(plan.Path) > 0 {
		section("path", strings.Join(plan.Path, ", ")+"  (before the toolchains and the system PATH)")
	}
	section("env", sortedVars(plan.RunEnv, plan.Env)...)
	section("build env", sortedVars(plan.BuildEnv)...)
	section("notes", plan.Notes...)
}

// sortedVars lists KEY=value lines; later maps win.
func sortedVars(maps ...map[string]string) []string {
	vars := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			vars[k] = v
		}
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = k + "=" + strconv.Quote(vars[k])
	}
	return out
}

func newManifestSchemaCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "schema",
		Short: "Print the JSON Schema of moorage.toml",
		Long: `Print the JSON Schema of moorage.toml, for editors that validate and
complete TOML files (for example with a "#:schema ./moorage.schema.json"
comment at the top of the manifest).`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := cmd.OutOrStdout().Write(manifest.Schema())
			return err
		},
	}
}

// explainContainer says what an app that runs in a container will run.
func explainContainer(section func(string, ...string), c *manifest.ContainerPlan, plan manifest.Plan) {
	if c.From != "" {
		section("image", c.From+"  (pulled, and pinned to its digest, when deploying)")
	} else {
		build := "build -f " + c.File + " " + c.Context
		if c.Target != "" {
			build += " --target " + c.Target
		}
		section("image", build)
	}
	port := "the lowest port the image exposes, or 8080"
	if c.Port != 0 {
		port = strconv.Itoa(c.Port)
	}
	section("port", port)
	section("data", c.Mount)
	var limits []string
	if c.User != "" {
		limits = append(limits, "user "+c.User)
	}
	if c.Memory > 0 {
		limits = append(limits, "memory "+humanBytes(c.Memory))
	}
	if c.CPU > 0 {
		limits = append(limits, "cpu "+strconv.FormatFloat(c.CPU, 'f', -1, 64))
	}
	if c.PIDs > 0 {
		limits = append(limits, "pids "+strconv.Itoa(c.PIDs))
	}
	section("limits", limits...)
}
