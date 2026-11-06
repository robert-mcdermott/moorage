// Package container runs app instances in containers, through the Podman or
// Docker command line (spec/05-runtime.md, "Container driver details"). The two
// accept the same lifecycle commands, so there is one code path, and everything
// it does is a command an operator can reproduce by hand.
package container

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// waitForDescendants is how long a command's output is waited for once it has ended or been told to. What it left running holds
// the pipe, and could for as long as it likes, which must not hold up going away.
const waitForDescendants = 2 * time.Second

// ErrNoEngine is returned when neither Podman nor Docker is installed.
var ErrNoEngine = errors.New("this server has no container engine: install Podman or Docker, or set engine under [containers] in server.toml")

// Engine is a container engine's command line.
type Engine struct {
	// Bin is the program, and Kind what it is: "podman" or "docker".
	Bin, Kind string
	// Auth, if set, returns registry credentials in the format of a
	// config.json, or nothing if there are none.
	Auth func(ctx context.Context) ([]byte, error)
	// TmpDir is where a temporary credentials file goes.
	TmpDir string
}

// Find returns the engine a setting names: "auto" for Podman if it is
// installed, or else Docker, or the path of a program.
func Find(setting string) (*Engine, error) {
	var bin string
	if setting == "" || setting == "auto" {
		for _, name := range []string{"podman", "docker"} {
			if p, err := exec.LookPath(name); err == nil {
				bin = p
				break
			}
		}
		if bin == "" {
			return nil, ErrNoEngine
		}
	} else {
		p, err := exec.LookPath(setting)
		if err != nil {
			return nil, fmt.Errorf("the container engine %q: %w", setting, err)
		}
		bin = p
	}
	out, err := exec.Command(bin, "--version").Output()
	if err != nil {
		return nil, fmt.Errorf("the container engine %s: %w", bin, err)
	}
	kind := "docker"
	if strings.Contains(strings.ToLower(string(out)), "podman") {
		kind = "podman"
	}
	return &Engine{Bin: bin, Kind: kind}, nil
}

// FindAll returns every engine that a setting could mean. With "auto", a server takes whichever answers, so that what it ran may be in either.
func FindAll(setting string) []*Engine {
	names := []string{setting}
	if setting == "" || setting == "auto" {
		names = []string{"podman", "docker"}
	}
	var engines []*Engine
	for _, name := range names {
		e, err := Find(name)
		if err != nil {
			continue
		}
		// Podman can answer to the name of Docker, with what is in one store.
		if slices.ContainsFunc(engines, func(other *Engine) bool { return other.Kind == e.Kind }) {
			continue
		}
		engines = append(engines, e)
	}
	return engines
}

// Check reports the engine's version, and whether it answers.
func (e *Engine) Check(ctx context.Context) (version string, err error) {
	out, err := e.output(ctx, nil, "--version")
	if err != nil {
		return "", err
	}
	version = strings.TrimSpace(string(out))
	_, err = e.output(ctx, nil, "ps", "-q")
	return version, err
}

// EngineError is a command of the engine that failed, with what it said.
type EngineError struct {
	Args   []string
	Stderr string
	Err    error
}

func (e *EngineError) Error() string {
	if e.Stderr != "" {
		return e.Stderr
	}
	return fmt.Sprintf("%s: %v", strings.Join(e.Args, " "), e.Err)
}

func (e *EngineError) Unwrap() error { return e.Err }

// IsNotFound reports whether the engine said there is no such container or image.
func IsNotFound(err error) bool {
	var ee *EngineError
	if !errors.As(err, &ee) {
		return false
	}
	msg := strings.ToLower(ee.Stderr)
	// An engine that cannot reach its daemon says "no such file or directory" of its socket, and that says nothing of the container. A daemon that is down should not be
	// taken for a container that has gone.
	for _, unreachable := range []string{"cannot connect", "unable to connect", "failed to connect", "error during connect", "dial unix", "dial tcp", "connection refused", "no such file or directory", "no such host"} {
		if strings.Contains(msg, unreachable) {
			return false
		}
	}
	return strings.Contains(msg, "no such") || strings.Contains(msg, "not found") || strings.Contains(msg, "does not exist") || strings.Contains(msg, "not known")
}

// isNoSuchVolume reports whether the engine said in as many words that there is no such volume. It is narrower than IsNotFound on purpose: an engine can say "not found" of its runtime,
// or of a context, and taking that for a lost volume would have what the volume holds replaced by its last dump.
func isNoSuchVolume(err error) bool {
	var ee *EngineError
	return errors.As(err, &ee) && strings.Contains(strings.ToLower(ee.Stderr), "no such volume")
}

// output runs a command and returns what it writes to standard output.
func (e *Engine) output(ctx context.Context, env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = waitForDescendants
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return out, &EngineError{Args: args, Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return out, nil
}

// stream runs a command, passing on what it writes.
func (e *Engine) stream(ctx context.Context, env []string, stdout, stderr io.Writer, args ...string) error {
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = waitForDescendants
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Run(); err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return &EngineError{Args: args, Err: err}
	}
	return nil
}

// withAuth returns the environment that gives a command the registry
// credentials, and a function that removes them: a temporary file, readable only
// by this user, for as long as the command runs. Podman reads REGISTRY_AUTH_FILE
// and Docker the config.json in DOCKER_CONFIG.
func (e *Engine) withAuth(ctx context.Context) ([]string, func(), error) {
	nothing := func() {}
	if e.Auth == nil {
		return nil, nothing, nil
	}
	data, err := e.Auth(ctx)
	if err != nil {
		return nil, nothing, fmt.Errorf("registry credentials: %w", err)
	}
	if len(data) == 0 {
		return nil, nothing, nil
	}
	if e.TmpDir != "" {
		if err := os.MkdirAll(e.TmpDir, 0o700); err != nil {
			return nil, nothing, err
		}
	}
	dir, err := os.MkdirTemp(e.TmpDir, "registry-")
	if err != nil {
		return nil, nothing, err
	}
	file := filepath.Join(dir, "config.json")
	if err := os.WriteFile(file, data, 0o600); err != nil {
		os.RemoveAll(dir)
		return nil, nothing, err
	}
	return []string{"DOCKER_CONFIG=" + dir, "REGISTRY_AUTH_FILE=" + file}, func() { os.RemoveAll(dir) }, nil
}

// Credential is a registry's username and password.
type Credential struct {
	Host, Username, Password string
}

// AuthFile returns credentials as a config.json, which both engines read.
func AuthFile(creds []Credential) []byte {
	if len(creds) == 0 {
		return nil
	}
	type auth struct {
		Auth string `json:"auth"`
	}
	auths := map[string]auth{}
	for _, c := range creds {
		a := auth{base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Password))}
		auths[c.Host] = a
		switch c.Host { // the one registry with more than one name
		case "docker.io", "index.docker.io", "registry-1.docker.io":
			auths["docker.io"] = a
			auths["https://index.docker.io/v1/"] = a
		}
	}
	data, _ := json.Marshal(map[string]any{"auths": auths})
	return data
}

// BuildSpec describes an image to build.
type BuildSpec struct {
	// Dir is the app's source; File and Context are relative to it.
	Dir, File, Context, Target string
	Tag                        string
	Labels                     map[string]string
	// Args are the manifest's build arguments.
	Args map[string]string
	// Vars are variables of the server's flagged for builds: they may be
	// secrets, so their values reach the engine through its environment.
	Vars map[string]string
}

// Build builds an image.
func (e *Engine) Build(ctx context.Context, b BuildSpec, stdout, stderr io.Writer) error {
	env, cleanup, err := e.withAuth(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	env = append(env, "BUILDKIT_PROGRESS=plain") // one line per step, for a log
	args := []string{"build", "-f", filepath.Join(b.Dir, filepath.FromSlash(b.File)), "-t", b.Tag}
	if b.Target != "" {
		args = append(args, "--target", b.Target)
	}
	for _, k := range sortedKeys(b.Labels) {
		args = append(args, "--label", k+"="+b.Labels[k])
	}
	for _, k := range sortedKeys(b.Args) {
		args = append(args, "--build-arg", k+"="+b.Args[k])
	}
	for _, k := range sortedKeys(b.Vars) {
		args = append(args, "--build-arg", k)
		env = append(env, k+"="+b.Vars[k])
	}
	args = append(args, filepath.Join(b.Dir, filepath.FromSlash(b.Context)))
	return e.stream(ctx, env, stdout, stderr, args...)
}

// Pull fetches an image.
func (e *Engine) Pull(ctx context.Context, ref string, stdout, stderr io.Writer) error {
	env, cleanup, err := e.withAuth(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	return e.stream(ctx, env, stdout, stderr, "pull", ref)
}

// ImageInfo is what the engine says of an image.
type ImageInfo struct {
	ID          string   `json:"Id"`
	RepoDigests []string `json:"RepoDigests"`
	Size        int64    `json:"Size"`
	Config      struct {
		ExposedPorts map[string]any `json:"ExposedPorts"`
		Env          []string       `json:"Env"`
		Volumes      map[string]any `json:"Volumes"`
	} `json:"Config"`
}

// InspectImage describes an image.
func (e *Engine) InspectImage(ctx context.Context, ref string) (ImageInfo, error) {
	out, err := e.output(ctx, nil, "image", "inspect", ref)
	if err != nil {
		return ImageInfo{}, err
	}
	var infos []ImageInfo
	if err := json.Unmarshal(out, &infos); err != nil {
		return ImageInfo{}, fmt.Errorf("read what the engine says of %s: %w", ref, err)
	}
	if len(infos) == 0 {
		return ImageInfo{}, fmt.Errorf("the engine says nothing of %s", ref)
	}
	return infos[0], nil
}

// RemoveImage removes an image, if nothing uses it.
func (e *Engine) RemoveImage(ctx context.Context, ref string) error {
	_, err := e.output(ctx, nil, "image", "rm", ref)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// Pinned returns an image reference that always means this image: ref at its
// digest, or, for an image no registry has seen, its ID.
func (i ImageInfo) Pinned(ref string) string {
	if strings.Contains(ref, "@sha256:") {
		return ref
	}
	repo := ref
	if slash := strings.LastIndex(repo, "/"); strings.Contains(repo[slash+1:], ":") {
		repo = repo[:strings.LastIndex(repo, ":")]
	}
	for _, d := range i.RepoDigests {
		name, _, _ := strings.Cut(d, "@")
		if name == repo || strings.HasSuffix(name, "/"+repo) {
			return d
		}
	}
	if len(i.RepoDigests) > 0 {
		return i.RepoDigests[0]
	}
	return i.ID
}

// FirstExposedPort returns the lowest port an image exposes, or 0.
func (i ImageInfo) FirstExposedPort() int {
	lowest := 0
	for p := range i.Config.ExposedPorts {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err == nil && n > 0 && (lowest == 0 || n < lowest) {
			lowest = n
		}
	}
	return lowest
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// combined runs a command and returns what it writes to standard output and standard error, in the order that it wrote them.
func (e *Engine) combined(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, e.Bin, args...)
	cmd.Env = os.Environ()
	cmd.WaitDelay = waitForDescendants
	out, err := cmd.CombinedOutput()
	if err != nil && !errors.Is(err, exec.ErrWaitDelay) {
		return string(out), &EngineError{Args: args, Stderr: string(out), Err: err}
	}
	return string(out), nil
}
