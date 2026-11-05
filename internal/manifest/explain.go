package manifest

import (
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// Explain says in plain words what a plan will do: the resolved
func Explain(w io.Writer, plan Plan) {
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
	if plan.Kind == KindStatic {
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
	if plan.Kind == KindStatic || plan.StaticFallback {
		dir := "the first of dist, build, out, public, _site, site and . with an index.html"
		if plan.Static != nil && plan.Static.Dir != "" {
			dir = plan.Static.Dir
		} else if plan.StaticFallback {
			dir = "the build's dist/, build/ or out/ with an index.html"
		}
		section("static", "serves "+dir)
	}
	var procs []string
	for _, name := range SortedProcesses(plan.Processes) {
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
		detail := fmt.Sprintf("  ×%d · %s", *p.Instances, check)
		if p.Protocol == "h2c" {
			detail += " · speaks h2c"
		}
		if p.Sticky {
			detail += " · sticky"
		}
		procs = append(procs, fmt.Sprintf("%s: %s", name, command), detail)
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
	if plan.Kind == KindProcess {
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

// explainContainer says what an app that runs in a container will run.
func explainContainer(section func(string, ...string), c *ContainerPlan, plan Plan) {
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

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return strconv.FormatFloat(float64(n)/float64(div), 'f', 1, 64) + " " + string("KMGTPE"[exp]) + "iB"
}
