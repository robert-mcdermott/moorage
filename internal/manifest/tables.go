package manifest

import (
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // so that a job's timezone means the same on every system
	"unicode"

	"github.com/robert-mcdermott/moorage/internal/schedule"
	"slices"
	"sort"
)

// Runtimes an app may name in [app].runtime.
var Runtimes = []string{"auto", "static", "go", "python", "node", "bun", "deno", "binary", "command", "dockerfile", "image"}

// Go is the [go] table.
type Go struct {
	Version string   `toml:"version" json:"version,omitempty"`
	Main    string   `toml:"main" json:"main,omitempty"`
	Binary  string   `toml:"binary" json:"binary,omitempty"`
	LDFlags *string  `toml:"ldflags" json:"ldflags,omitempty"`
	Tags    []string `toml:"tags" json:"tags,omitempty"`
	CGO     bool     `toml:"cgo" json:"cgo,omitempty"`
}

// Python is the [python] table.
type Python struct {
	Version string   `toml:"version" json:"version,omitempty"`
	Manager string   `toml:"manager" json:"manager,omitempty"`
	Groups  []string `toml:"groups" json:"groups,omitempty"`
	Extras  []string `toml:"extras" json:"extras,omitempty"`
}

// JS is the [node], [bun] or [deno] table.
type JS struct {
	Version        string `toml:"version" json:"version,omitempty"`
	PackageManager string `toml:"package_manager" json:"package_manager,omitempty"`
	BuildScript    string `toml:"build_script" json:"build_script,omitempty"`
	Prune          *bool  `toml:"prune" json:"prune,omitempty"`
}

// Container is the [container] table.
type Container struct {
	File    string            `toml:"file"`
	Context string            `toml:"context"`
	Target  string            `toml:"target"`
	Args    map[string]string `toml:"args"`
	Image   string            `toml:"image"`
	Port    int               `toml:"port"`
	User    string            `toml:"user"`
}

// Build is the [build] table.
type Build struct {
	Command string            `toml:"command" json:"command,omitempty"`
	Pre     string            `toml:"pre" json:"pre,omitempty"`
	Post    string            `toml:"post" json:"post,omitempty"`
	Env     map[string]string `toml:"env" json:"env,omitempty"`
	Timeout Duration          `toml:"timeout" json:"timeout,omitempty"`
	Exclude []string          `toml:"exclude" json:"exclude,omitempty"`
}

// Release is the [release] table: commands run once per deployment.
type Release struct {
	Command  string   `toml:"command" json:"command,omitempty"`
	OnCreate string   `toml:"on_create" json:"on_create,omitempty"`
	Timeout  Duration `toml:"timeout" json:"timeout,omitempty"`
}

// Cron is one [[cron]] job.
type Cron struct {
	Name         string   `toml:"name"`
	Schedule     string   `toml:"schedule"`
	Command      string   `toml:"command"`
	Timezone     string   `toml:"timezone"`
	Timeout      Duration `toml:"timeout"`
	Overlap      string   `toml:"overlap"`
	Environments []string `toml:"environments"`
}

// Route is one [[routes]] entry.
type Route struct {
	Path        string `toml:"path"`
	Process     string `toml:"process"`
	Static      bool   `toml:"static"`
	StripPrefix bool   `toml:"strip_prefix"`
}

// Var is one [vars.<NAME>] declaration.
type Var struct {
	Description string `toml:"description"`
	Required    bool   `toml:"required"`
	Secret      bool   `toml:"secret"`
	Generate    string `toml:"generate"`
}

// Resources is the [resources] table.
type Resources struct {
	Memory Size    `toml:"memory"`
	CPU    float64 `toml:"cpu"`
	PIDs   int     `toml:"pids"`
}

// Storage is the [storage] table.
type Storage struct {
	Mount string `toml:"mount"`
}

// Previews is the [previews] table.
type Previews struct {
	Enabled    *bool    `toml:"enabled"`
	Branches   []string `toml:"branches"`
	TTL        Duration `toml:"ttl"`
	SleepAfter string   `toml:"sleep_after"`
	Access     string   `toml:"access"`
	Data       string   `toml:"data"`
}

// Environment is one [environments.<name>] table of overrides.
type Environment struct {
	Branch     string                         `toml:"branch"`
	Env        map[string]string              `toml:"env"`
	Resources  *Resources                     `toml:"resources"`
	Processes  map[string]EnvironmentInstance `toml:"processes"`
	SleepAfter string                         `toml:"sleep_after"`
	Access     string                         `toml:"access"`
}

// EnvironmentInstance overrides a process's instance count.
type EnvironmentInstance struct {
	Instances *int `toml:"instances"`
}

// Service is one [services.<name>] table: something that an app declares, and Moorage runs and connects for it (spec/13-services.md).
type Service struct {
	Type string `toml:"type"`
	// Version is left out for the default, which is recorded when the service is made and does not move by itself.
	Version string `toml:"version"`
	// Image replaces the usual image, which is the way to get extensions.
	Image string `toml:"image"`
	// Env is another name for the variable that it gives the app, needed for a second of one type.
	Env    string `toml:"env"`
	Memory Size   `toml:"memory"`
	// Previews says whether a preview has one of its own: "own", the default, or "none".
	Previews string `toml:"previews"`
	// Seed and Sanitize say what a preview starts with, if not empty.
	Seed     string `toml:"seed"`
	Sanitize string `toml:"sanitize"`
}

// The types of service.
const (
	ServicePostgres = "postgres"
	ServiceS3       = "s3"
)

// DefaultPostgres is the version of PostgreSQL that a service is made with, unless it says. What is made stays at what it was made with, as this moves.
const DefaultPostgres = "18"

// SanitizeNone says, under sanitize, that what is in production is fit to copy as it is.
const SanitizeNone = "none"

var (
	postgresVersions = []string{"15", "16", "17", "18", "19"}
	serviceName      = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

// AddressVariable is the variable that gives an app its address: its own name for it, or the usual one for the type.
func (s Service) AddressVariable() string {
	if s.Env != "" {
		return s.Env
	}
	if s.Type == ServiceS3 {
		return s3Default
	}
	return "DATABASE_URL"
}

// s3Default is what a service of S3 gives an app under the usual names.
const s3Default = "S3_ENDPOINT"

// Variables are what a service gives an app. A service of S3 has several, under the name in env, and under the names that S3 clients look for if it is the only one.
func (s ServicePlan) Variables() []string {
	if s.Type != ServiceS3 {
		return []string{s.Env, s.Env + "_PASSWORD"}
	}
	prefix := s.Env
	if prefix == s3Default {
		prefix = "S3"
	}
	names := []string{prefix + "_ENDPOINT", prefix + "_BUCKET", prefix + "_REGION", prefix + "_ACCESS_KEY_ID", prefix + "_SECRET_ACCESS_KEY"}
	if s.Env == s3Default {
		names = append(names, "AWS_ENDPOINT_URL", "AWS_REGION", "AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY")
	}
	return names
}

// AddressVariable is the variable that says where a service is: setting it on an environment itself says that the environment goes elsewhere.
func (s ServicePlan) AddressVariable() string { return s.Variables()[0] }

// Plan is what a build keeps of a service.
func (s Service) Plan() ServicePlan {
	previews := s.Previews
	if previews == "" {
		previews = "own"
	}
	return ServicePlan{Type: s.Type, Version: s.Version, Image: s.Image, Env: s.AddressVariable(), Memory: int64(s.Memory), Previews: previews, Seed: s.Seed, Sanitize: s.Sanitize}
}

// checkServices says what is wrong with [services].
func checkServices(add func(string, ...any), services map[string]Service) {
	names := make([]string, 0, len(services))
	for name := range services {
		names = append(names, name)
	}
	sort.Strings(names)
	addresses := map[string]string{}
	for _, name := range names {
		s := services[name]
		where := "services." + name
		if !serviceName.MatchString(name) {
			add("%s: a service's name is lower case letters, digits and dashes, starting with a letter, up to 31 of them", where)
		}
		if !oneOf(s.Type, []string{ServicePostgres, ServiceS3}) {
			add("%s.type %q must be postgres or s3", where, s.Type)
			continue
		}
		if s.Type == ServiceS3 && s.Version != "" {
			add("%s.version: s3 has no version to choose", where)
		}
		if s.Type == ServicePostgres && s.Version != "" && !oneOf(s.Version, postgresVersions) {
			add("%s.version %q must be one of %s", where, s.Version, strings.Join(postgresVersions, ", "))
		}
		if s.Image != "" {
			if err := ValidateImage(s.Image); err != nil {
				add("%s.image: %v", where, err)
			} else if tag := imageTag(s.Image); tag == "" || tag == "latest" {
				add("%s.image %q needs a tag of its own: latest would change under a database's data", where, s.Image)
			}
			if s.Type == ServiceS3 {
				add("%s.image: s3 runs one server, and this cannot replace it", where)
			}
		}
		if s.Env != "" && !envName.MatchString(s.Env) {
			add("%s.env: %q is not a valid variable name", where, s.Env)
		}
		for _, variable := range s.Plan().Variables() {
			if other, taken := addresses[variable]; taken {
				add("%s and services.%s would both set %s: give one of them env", "services."+other, name, variable)
				break
			}
			addresses[variable] = name
			if reservedVariable(variable) {
				add("%s.env: %s is not a name that an app can be given, as Moorage or the system needs it", where, variable)
			}
		}
		if s.Memory < 0 {
			add("%s.memory must not be negative", where)
		}
		if s.Previews != "" && !oneOf(s.Previews, []string{"own", "none"}) {
			add("%s.previews %q must be own or none", where, s.Previews)
		}
		if s.Seed != "" && s.Seed != "production" {
			add("%s.seed %q must be production", where, s.Seed)
		}
		if s.Sanitize != "" && s.Seed == "" {
			add("%s.sanitize is for what a seed copies, and there is no seed", where)
		}
		if s.Seed != "" && s.Sanitize == "" {
			add("%s.seed copies production's data into previews: say what removes what should not be copied, with sanitize, or say sanitize = \"none\"", where)
		}
		if s.Type == ServiceS3 && s.Sanitize != "" && s.Sanitize != SanitizeNone {
			add("%s.sanitize: s3 has no script to run, so that it can only be none", where)
		}
		if strings.HasPrefix(s.Sanitize, "/") || slices.Contains(strings.Split(s.Sanitize, "/"), "..") {
			add("%s.sanitize %q must be a path in the app", where, s.Sanitize)
		}
	}
}

// reservedVariable says that a service should not set this, as what runs depends on what it holds.
func reservedVariable(name string) bool {
	return strings.HasPrefix(name, "MOORAGE_") || slices.Contains([]string{"PATH", "HOME", "PORT", "HOST", "TMPDIR", "TEMP", "TMP", "LD_PRELOAD", "LD_LIBRARY_PATH"}, name)
}

// imageTag is the tag of an image reference, or its digest if it has one.
func imageTag(ref string) string {
	if _, digest, ok := strings.Cut(ref, "@"); ok {
		return digest
	}
	last := ref[strings.LastIndex(ref, "/")+1:]
	_, tag, _ := strings.Cut(last, ":")
	return tag
}

// Size is an amount of bytes written as "512MiB", "2GiB", "300M" or a
// plain number of bytes.
type Size int64

var sizeUnits = map[string]int64{
	"": 1, "B": 1,
	"K": 1000, "KB": 1000, "KiB": 1 << 10,
	"M": 1000 * 1000, "MB": 1000 * 1000, "MiB": 1 << 20,
	"G": 1000 * 1000 * 1000, "GB": 1000 * 1000 * 1000, "GiB": 1 << 30,
}

var sizePattern = regexp.MustCompile(`^([0-9]+(?:\.[0-9]+)?)\s*([A-Za-z]*)$`)

// UnmarshalText parses a size.
func (s *Size) UnmarshalText(b []byte) error {
	m := sizePattern.FindStringSubmatch(strings.TrimSpace(string(b)))
	if m == nil {
		return fmt.Errorf("%q is not a size such as 512MiB or 2GiB", b)
	}
	unit, ok := sizeUnits[m[2]]
	if !ok {
		return fmt.Errorf("%q is not a size such as 512MiB or 2GiB", b)
	}
	n, _ := strconv.ParseFloat(m[1], 64)
	*s = Size(n * float64(unit))
	return nil
}

// MarshalText formats the size in bytes.
func (s Size) MarshalText() ([]byte, error) { return []byte(strconv.FormatInt(int64(s), 10)), nil }

var (
	toolchainPattern = regexp.MustCompile(`^[a-z][a-z0-9-]*@[A-Za-z0-9.*+-]+$`)
	cronField        = regexp.MustCompile(`^[0-9A-Za-z*,/-]+$`)
)

// validateTables checks the tables beyond [static], [processes] and
// [deploy]; it returns every problem found.
func (m *Manifest) validateTables() []string {
	var p []string
	add := func(format string, args ...any) { p = append(p, fmt.Sprintf(format, args...)) }

	if m.App.Runtime != "" && !oneOf(m.App.Runtime, Runtimes) {
		add("app.runtime %q must be one of %s", m.App.Runtime, strings.Join(Runtimes, ", "))
	}
	if m.App.Shell != "" && !oneOf(m.App.Shell, []string{"embedded", "system"}) {
		add("app.shell %q must be embedded or system", m.App.Shell)
	}
	for _, t := range m.App.Toolchains {
		if !toolchainPattern.MatchString(t) {
			add("app.toolchains: %q must be name@version, such as node@24 or java@system", t)
		}
	}
	if m.App.Root != "" && !relativePath(m.App.Root) {
		add("app.root %q must be a path inside the repository", m.App.Root)
	}
	if m.Python != nil && m.Python.Manager != "" && !oneOf(m.Python.Manager, []string{"uv", "uv-pip"}) {
		add("python.manager %q must be uv or uv-pip", m.Python.Manager)
	}
	for table, js := range map[string]*JS{"node": m.Node, "bun": m.Bun, "deno": m.Deno} {
		if js != nil && js.PackageManager != "" && !oneOf(js.PackageManager, []string{"npm", "pnpm", "yarn", "bun"}) {
			add("%s.package_manager %q must be npm, pnpm, yarn or bun", table, js.PackageManager)
		}
	}
	if m.Build != nil {
		checkEnv(&p, "build.env", m.Build.Env)
		for _, g := range m.Build.Exclude {
			if _, err := path.Match(g, ""); err != nil || !relativePath(strings.TrimSuffix(g, "/**")) {
				add("build.exclude: %q is not a valid pattern inside the app", g)
			}
		}
	}
	names := map[string]bool{}
	for i, c := range m.Cron {
		where := fmt.Sprintf("cron[%d]", i)
		if c.Name == "" {
			add("%s: name is required", where)
		} else if names[c.Name] {
			add("%s: the name %q is used twice", where, c.Name)
		}
		names[c.Name] = true
		if c.Command == "" {
			add("%s: command is required", where)
		}
		if c.Timeout.Std() > MaxRunTimeout {
			add("%s: timeout must be a day at the most", where)
		}
		if !validSchedule(c.Schedule) {
			add("%s: schedule %q must be five cron fields, such as \"0 3 * * *\", or @hourly, @daily, @every 15m", where, c.Schedule)
		}
		if c.Timezone != "" {
			if _, err := time.LoadLocation(c.Timezone); err != nil {
				add("%s: timezone %q is not a time zone such as Europe/Paris", where, c.Timezone)
			}
		}
		if c.Overlap != "" && !oneOf(c.Overlap, []string{"skip", "allow", "replace"}) {
			add("%s: overlap %q must be skip, allow or replace", where, c.Overlap)
		}
	}
	for i, r := range m.Routes {
		where := fmt.Sprintf("routes[%d]", i)
		if !strings.HasPrefix(r.Path, "/") {
			add("%s: path %q must start with /", where, r.Path)
		}
		if (r.Process == "") == !r.Static {
			add("%s: give either process or static = true", where)
		}
		if r.Process != "" && m.Processes != nil {
			if _, ok := m.Processes[r.Process]; !ok {
				add("%s: there is no process %q", where, r.Process)
			}
		}
	}
	for name, v := range m.Vars {
		if !envName.MatchString(name) {
			add("vars: %q is not a valid variable name", name)
		}
		if v.Generate != "" && !oneOf(v.Generate, []string{"secret", "uuid"}) {
			add("vars.%s.generate %q must be secret or uuid", name, v.Generate)
		}
	}
	if m.Resources != nil {
		checkResources(&p, "resources", *m.Resources)
	}
	if c := m.Container; c != nil {
		if c.Port < 0 || c.Port > 65535 {
			add("container.port %d is not a port", c.Port)
		}
		if c.Image != "" && ValidateImage(c.Image) != nil {
			add("container.image %q is not an image reference", c.Image)
		}
		for _, f := range []struct{ key, value string }{{"file", c.File}, {"context", c.Context}} {
			if f.value != "" && (path.IsAbs(f.value) || strings.Contains(f.value, "..") || strings.Contains(f.value, `\`)) {
				add("container.%s %q must be a path inside the app", f.key, f.value)
			}
		}
		for name := range c.Args {
			if !envName.MatchString(name) {
				add("container.args: %q is not a valid argument name", name)
			}
		}
	}
	if s := m.Storage; s != nil && s.Mount != "" && (!path.IsAbs(s.Mount) || s.Mount == "/") {
		add("storage.mount %q must be an absolute path inside the container, such as /data", s.Mount)
	}
	if m.Previews != nil {
		if m.Previews.Access != "" && !oneOf(m.Previews.Access, accessModes) {
			add("previews.access %q must be one of %s", m.Previews.Access, strings.Join(accessModes, ", "))
		}
		if m.Previews.Data != "" && !oneOf(m.Previews.Data, []string{"empty", "copy"}) {
			add("previews.data %q must be empty or copy", m.Previews.Data)
		}
	}
	if m.Previews != nil && m.Previews.SleepAfter != "" && m.Previews.SleepAfter != "off" {
		var d Duration
		if err := d.UnmarshalText([]byte(m.Previews.SleepAfter)); err != nil {
			add("previews.sleep_after: %v (or \"off\")", err)
		}
	}
	for name, e := range m.Environments {
		where := "environments." + name
		checkEnv(&p, where+".env", e.Env)
		if e.Resources != nil {
			checkResources(&p, where+".resources", *e.Resources)
		}
		if e.Access != "" && !oneOf(e.Access, accessModes) {
			add("%s.access %q must be one of %s", where, e.Access, strings.Join(accessModes, ", "))
		}
		if e.SleepAfter != "" && e.SleepAfter != "off" {
			var d Duration
			if err := d.UnmarshalText([]byte(e.SleepAfter)); err != nil {
				add("%s.sleep_after: %v (or \"off\")", where, err)
			}
		}
		for proc, o := range e.Processes {
			if o.Instances != nil && *o.Instances < 0 {
				add("%s.processes.%s.instances must not be negative", where, proc)
			}
		}
	}
	checkServices(add, m.Services)
	return p
}

var accessModes = []string{"public", "password", "login", "share"}

func checkEnv(p *[]string, where string, env map[string]string) {
	for k := range env {
		if !envName.MatchString(k) {
			*p = append(*p, fmt.Sprintf("%s: %q is not a valid variable name", where, k))
		}
	}
}

func checkResources(p *[]string, where string, r Resources) {
	if r.Memory < 0 || r.CPU < 0 || r.PIDs < 0 {
		*p = append(*p, where+": limits must not be negative")
	}
}

func validSchedule(s string) bool {
	_, err := schedule.Parse(s)
	return err == nil
}

// relativePath reports whether p is a slash-separated path that stays
// inside its base directory.
func relativePath(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, `\`) || (len(p) > 1 && p[1] == ':') {
		return false
	}
	c := path.Clean(p)
	return c != ".." && !strings.HasPrefix(c, "../")
}

func oneOf(s string, options []string) bool {
	for _, o := range options {
		if s == o {
			return true
		}
	}
	return false
}

// ValidateImage checks an image's name: it goes on the engine's command line.
func ValidateImage(ref string) error {
	if ref == "" || strings.HasPrefix(ref, "-") || strings.ContainsFunc(ref, unicode.IsSpace) {
		return fmt.Errorf("%q is not an image reference, such as ghcr.io/acme/api:1.4", ref)
	}
	return nil
}
