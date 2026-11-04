package manifest

import "time"

// Plan is the resolved description of how a build is made and run, stored
// with the build: what `moor manifest explain` prints. Kind "static" is a
// directory served as is; kind "process" is a directory whose processes are
// run. A plan received with an upload (a static site, an artifact) only
// says how to serve or run it; a plan the server resolves from source also
// says how to build it.
type Plan struct {
	Kind    string `json:"kind"`
	Runtime string `json:"runtime,omitempty"`
	// Toolchains are put on PATH at build and run time. Before a build their
	// versions are as requested (possibly partial); after it, exact.
	Toolchains []Toolchain `json:"toolchains,omitempty"`
	// Steps are the build commands, run in order in the app directory.
	Steps        []Step            `json:"steps,omitempty"`
	BuildEnv     map[string]string `json:"build_env,omitempty"`
	BuildTimeout Duration          `json:"build_timeout,omitempty"`
	// Exclude lists globs removed from the build once it is done.
	Exclude []string `json:"exclude,omitempty"`
	// Path lists directories of the app put first on PATH at build and run
	// time, such as .venv/bin.
	Path []string `json:"path,omitempty"`
	// RunEnv holds the runtime's own variables (PYTHONUNBUFFERED, NODE_ENV);
	// [env] overrides them.
	RunEnv map[string]string `json:"run_env,omitempty"`
	// DirEnv holds variables whose values are directories of the app, made
	// absolute when the app runs (VIRTUAL_ENV = .venv).
	DirEnv map[string]string `json:"dir_env,omitempty"`

	Static *Static `json:"static,omitempty"`
	// Root is the directory served for a static plan, relative to the
	// build: "site" for an uploaded site, "app/<dir>" for a built one.
	Root string `json:"root,omitempty"`
	// StaticFallback serves the build's output directory when the app turns
	// out to have nothing to run (a Node app without a start script).
	StaticFallback bool `json:"static_fallback,omitempty"`

	// Container, if set, says that the app runs in containers.
	Container *ContainerPlan `json:"container,omitempty"`

	Processes map[string]Process `json:"processes,omitempty"`
	Release   *Release           `json:"release,omitempty"`
	Deploy    Deploy             `json:"deploy"`
	Env       map[string]string  `json:"env,omitempty"`

	// Notes say what detection found and decided.
	Notes []string `json:"notes,omitempty"`
}

// ContainerPlan says how an app's image is made and how its containers run.
type ContainerPlan struct {
	// File, Context, Target and Args build the image (the dockerfile runtime).
	File    string            `json:"file,omitempty"`
	Context string            `json:"context,omitempty"`
	Target  string            `json:"target,omitempty"`
	Args    map[string]string `json:"args,omitempty"`
	// From is the image asked for (the image runtime).
	From string `json:"from,omitempty"`
	// Image is what runs, settled by the build: the built tag, or From pinned
	// to its digest, so that rollbacks and restarts run exactly the same image.
	Image string `json:"image,omitempty"`
	// Port is where the app listens inside the container. It is settled by the
	// build: the manifest's, or the image's first EXPOSE, or else DefaultContainerPort.
	Port int `json:"port,omitempty"`
	// User runs the container as someone other than the image's default.
	User string `json:"user,omitempty"`
	// Mount is where the environment's data directory appears.
	Mount string `json:"mount"`
	// Memory (bytes), CPU (cores) and PIDs limit each container; 0 for none.
	Memory int64   `json:"memory,omitempty"`
	CPU    float64 `json:"cpu,omitempty"`
	PIDs   int     `json:"pids,omitempty"`
}

// DefaultContainerPort is where a container's app is taken to listen when
// neither the manifest nor the image says.
const DefaultContainerPort = 8080

// DefaultMount is where a container's data directory appears by default.
const DefaultMount = "/data"

// Toolchain is a toolchain a build needs.
type Toolchain struct {
	Name string `json:"name"`
	// Version is a version or a partial one ("24", "1.26", "3.13"), empty
	// for the latest, or "system" for the one on the host's PATH.
	Version string `json:"version,omitempty"`
	// From says where the version came from (go.mod, .nvmrc, …).
	From string `json:"from,omitempty"`
}

// Step is one build command.
type Step struct {
	Name    string `json:"name"`
	Command string `json:"command"`
}

// Plan kinds.
const (
	KindStatic  = "static"
	KindProcess = "process"
)

// Validate checks a plan received with an upload.
func (p *Plan) Validate() error {
	if len(p.Steps) > 0 || len(p.Toolchains) > 0 || len(p.Path) > 0 || len(p.DirEnv) > 0 || p.Root != "" || p.Release != nil || p.Container != nil {
		return &planError{"an uploaded plan cannot carry build steps, toolchains, containers or release commands; deploy the source to have it built"}
	}
	switch p.Kind {
	case KindStatic:
		if p.Static == nil {
			p.Static = &Static{}
		}
		return p.Static.Validate()
	case KindProcess:
		if err := ValidateProcesses(p.Processes); err != nil {
			return err
		}
		for k := range p.Env {
			if !envName.MatchString(k) {
				return &planError{"env: " + k + " is not a valid variable name"}
			}
		}
		return p.Deploy.Validate()
	}
	return &planError{"plan kind " + p.Kind + " is not supported"}
}

// Defaults fills in the defaults of every process, of [deploy] and of
// [release].
func (p *Plan) Defaults() {
	for name, proc := range p.Processes {
		proc.Defaults(name)
		p.Processes[name] = proc
	}
	p.Deploy.Defaults()
	if p.Static != nil {
		p.Static.Defaults()
	}
	if p.Release != nil && p.Release.Timeout == 0 {
		p.Release.Timeout = Duration(10 * time.Minute)
	}
	if p.BuildTimeout == 0 && (len(p.Steps) > 0 || p.Container != nil) {
		p.BuildTimeout = Duration(20 * time.Minute)
	}
}

type planError struct{ msg string }

func (e *planError) Error() string { return e.msg }

// MaxInstances is how many instances of a process an environment may run.
const MaxInstances = 20

// WithFormation returns the plan with as many instances of its processes as the
// formation says. Those it does not name keep the manifest's number, and one the plan
// does not have is passed over: a later release may have it again.
func (p Plan) WithFormation(formation map[string]int) Plan {
	if len(formation) == 0 {
		return p
	}
	processes := make(map[string]Process, len(p.Processes))
	for name, proc := range p.Processes {
		if n, ok := formation[name]; ok {
			proc.Instances = &n
		}
		processes[name] = proc
	}
	p.Processes = processes
	return p
}
