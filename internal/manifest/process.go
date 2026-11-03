package manifest

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pelletier/go-toml/v2"
)

// Duration is a length of time written as "500ms", "30s", "15m", "2h" or
// "7d".
type Duration time.Duration

// UnmarshalText parses a duration, accepting a "d" (day) unit.
func (d *Duration) UnmarshalText(b []byte) error {
	s := strings.TrimSpace(string(b))
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n < 0 {
			return fmt.Errorf("%q is not a duration such as 30s, 15m or 7d", s)
		}
		*d = Duration(n * float64(24*time.Hour))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil || v < 0 {
		return fmt.Errorf("%q is not a duration such as 30s, 15m or 7d", s)
	}
	*d = Duration(v)
	return nil
}

// MarshalText formats the duration as Go does ("1m30s").
func (d Duration) MarshalText() ([]byte, error) {
	return []byte(time.Duration(d).String()), nil
}

// Std returns the duration as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// Process is one [processes.<name>] entry: a long-running command.
type Process struct {
	Command     string   `toml:"command" json:"command"`
	Instances   *int     `toml:"instances" json:"instances,omitempty"`
	Expose      *bool    `toml:"expose" json:"expose,omitempty"`
	Health      Health   `toml:"health" json:"health"`
	StopSignal  string   `toml:"stop_signal" json:"stop_signal,omitempty"`
	StopTimeout Duration `toml:"stop_timeout" json:"stop_timeout,omitempty"`
	Restart     string   `toml:"restart" json:"restart,omitempty"`
	// Sleep and Protocol configure features of later releases.
	Sleep    *bool  `toml:"sleep" json:"sleep,omitempty"`
	Protocol string `toml:"protocol" json:"protocol,omitempty"`
}

// Health is a process's health check.
type Health struct {
	Path      string   `toml:"path" json:"path,omitempty"`
	Command   string   `toml:"command" json:"command,omitempty"`
	Expect    string   `toml:"expect" json:"expect,omitempty"`
	Interval  Duration `toml:"interval" json:"interval,omitempty"`
	Timeout   Duration `toml:"timeout" json:"timeout,omitempty"`
	Startup   Duration `toml:"startup" json:"startup,omitempty"`
	Threshold int      `toml:"threshold" json:"threshold,omitempty"`
	MinUptime Duration `toml:"min_uptime" json:"min_uptime,omitempty"`
}

// Deploy is the [deploy] table: how a release replaces the previous one.
type Deploy struct {
	Strategy     string   `toml:"strategy" json:"strategy,omitempty"`
	Drain        Duration `toml:"drain" json:"drain,omitempty"`
	KeepReleases int      `toml:"keep_releases" json:"keep_releases,omitempty"`
}

// Web is the process that receives HTTP traffic.
const Web = "web"

var processName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,29}$`)

// Defaults fills in a process's unset fields.
func (p *Process) Defaults(name string) {
	if p.Instances == nil {
		one := 1
		p.Instances = &one
	}
	if p.Expose == nil {
		expose := name == Web
		p.Expose = &expose
	}
	if p.StopSignal == "" {
		p.StopSignal = "TERM"
	}
	if p.StopTimeout == 0 {
		p.StopTimeout = Duration(30 * time.Second)
	}
	if p.Restart == "" {
		p.Restart = "always"
	}
	h := &p.Health
	if h.Interval == 0 {
		h.Interval = Duration(10 * time.Second)
	}
	if h.Timeout == 0 {
		h.Timeout = Duration(3 * time.Second)
	}
	if h.Startup == 0 {
		h.Startup = Duration(60 * time.Second)
	}
	if h.Threshold == 0 {
		h.Threshold = 3
	}
	if h.MinUptime == 0 {
		h.MinUptime = Duration(5 * time.Second)
	}
}

// Defaults fills in the [deploy] defaults.
func (d *Deploy) Defaults() {
	if d.Strategy == "" {
		d.Strategy = "rolling"
	}
	if d.Drain == 0 {
		d.Drain = Duration(30 * time.Second)
	}
	if d.KeepReleases == 0 {
		d.KeepReleases = 5
	}
}

// ValidateProcesses checks the processes of a plan, reporting every problem.
func ValidateProcesses(procs map[string]Process) error { return validateProcesses(procs, true) }

// validateProcesses checks processes; in a manifest, web may leave its
// command to the runtime's preset.
func validateProcesses(procs map[string]Process, requireWebCommand bool) error {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }
	if len(procs) == 0 {
		add("[processes] declares no process")
	}
	for _, name := range sortedNames(procs) {
		proc := procs[name]
		at := "processes." + name
		if !processName.MatchString(name) {
			add("%s: a process name must be lowercase letters, digits and hyphens, starting with a letter", at)
		}
		if strings.TrimSpace(proc.Command) == "" && (requireWebCommand || name != Web) {
			add("%s.command is required", at)
		}
		if proc.Instances != nil && (*proc.Instances < 0 || *proc.Instances > 20) {
			add("%s.instances must be between 0 and 20", at)
		}
		switch proc.Restart {
		case "", "always", "on-failure", "never":
		default:
			add("%s.restart %q must be \"always\", \"on-failure\" or \"never\"", at, proc.Restart)
		}
		switch strings.ToUpper(strings.TrimPrefix(proc.StopSignal, "SIG")) {
		case "", "TERM", "INT", "QUIT", "HUP", "USR1", "USR2":
		default:
			add("%s.stop_signal %q must be TERM, INT, QUIT, HUP, USR1 or USR2", at, proc.StopSignal)
		}
		if proc.Health.Path != "" && !strings.HasPrefix(proc.Health.Path, "/") {
			add("%s.health.path %q must start with /", at, proc.Health.Path)
		}
		if proc.Health.Command != "" {
			add("%s.health.command is not supported yet; use health.path or the default TCP check", at)
		}
		if proc.Health.Threshold < 0 {
			add("%s.health.threshold must be positive", at)
		}
		if proc.Protocol != "" && proc.Protocol != "http" {
			add("%s.protocol %q is not supported yet", at, proc.Protocol)
		}
	}
	if len(p) > 0 {
		return fmt.Errorf("invalid [processes]:\n  - %s", strings.Join(p, "\n  - "))
	}
	return nil
}

// ValidateDeploy checks the [deploy] table.
func (d Deploy) Validate() error {
	switch d.Strategy {
	case "", "rolling", "recreate":
	default:
		return fmt.Errorf("deploy.strategy %q must be \"rolling\" or \"recreate\"", d.Strategy)
	}
	if d.KeepReleases < 0 {
		return errors.New("deploy.keep_releases must be positive")
	}
	return nil
}

// decodeProcesses turns the raw [processes] table into processes: each
// entry is a command string or a table.
func decodeProcesses(raw map[string]any) (map[string]Process, error) {
	out := map[string]Process{}
	for name, v := range raw {
		switch v := v.(type) {
		case string:
			out[name] = Process{Command: v}
		case map[string]any:
			data, err := toml.Marshal(v)
			if err != nil {
				return nil, err
			}
			var proc Process
			dec := toml.NewDecoder(bytes.NewReader(data))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&proc); err != nil {
				var strict *toml.StrictMissingError
				if errors.As(err, &strict) {
					var keys []string
					for _, e := range strict.Errors {
						keys = append(keys, strings.Join(e.Key(), "."))
					}
					return nil, fmt.Errorf("[processes.%s]: unknown key %s", name, strings.Join(keys, ", "))
				}
				return nil, fmt.Errorf("[processes.%s]: %v", name, err)
			}
			out[name] = proc
		default:
			return nil, fmt.Errorf("processes.%s must be a command string or a table", name)
		}
	}
	return out, nil
}

func sortedNames(procs map[string]Process) []string {
	names := make([]string, 0, len(procs))
	for n := range procs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SortedProcesses returns the names of procs in order, web first.
func SortedProcesses(procs map[string]Process) []string {
	names := sortedNames(procs)
	sort.SliceStable(names, func(i, j int) bool { return names[i] == Web && names[j] != Web })
	return names
}
