package manifest

// Plan is the resolved description of how a build runs, stored with the
// build. Kind "static" is a directory served as is; kind "process" is a
// directory whose processes are run (an uploaded artifact for now; built
// from source in a later release).
type Plan struct {
	Kind      string             `json:"kind"`
	Static    *Static            `json:"static,omitempty"`
	Processes map[string]Process `json:"processes,omitempty"`
	Deploy    Deploy             `json:"deploy"`
	Env       map[string]string  `json:"env,omitempty"`
}

// Plan kinds.
const (
	KindStatic  = "static"
	KindProcess = "process"
)

// Validate checks a plan received from a client.
func (p *Plan) Validate() error {
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

// Defaults fills in the defaults of every process and of [deploy].
func (p *Plan) Defaults() {
	for name, proc := range p.Processes {
		proc.Defaults(name)
		p.Processes[name] = proc
	}
	p.Deploy.Defaults()
	if p.Static != nil {
		p.Static.Defaults()
	}
}

type planError struct{ msg string }

func (e *planError) Error() string { return e.msg }
