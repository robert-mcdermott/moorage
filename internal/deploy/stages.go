package deploy

import (
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// The stages of a deployment, in the order they run.
const (
	StagePrepare    = "prepare"
	StageToolchains = "toolchains"
	StageBuild      = "build"
	StageRelease    = "release"
	StageRollout    = "rollout"
)

// How a stage stands.
const (
	StagePending = "pending" // it has not been reached
	StageRunning = "running"
	StageDone    = "done"
	StageFailed  = "failed"
	StageSkipped = "skipped" // the deployment did not need it, or ended before it
)

// Stage is a step of a deployment, as its log tells it.
type Stage struct {
	Name, Status string
	// Took is how long it took, or has taken so far.
	Took time.Duration
}

// stageOf says which stage a line of Moorage's own in the log begins, if any. The
// deployer and the builder write these (build.Marker): "▸ rollout    web ×2". A build
// step's name is the app's own, so what no other stage claims is the build.
func stageOf(l logs.Line) string {
	if l.Stream != logs.Moorage {
		return ""
	}
	rest, ok := strings.CutPrefix(l.Text, "▸ ")
	if !ok {
		return ""
	}
	word, _, _ := strings.Cut(rest, " ")
	switch word {
	case "source", "detected":
		return StagePrepare
	case "toolchains":
		return StageToolchains
	case "release":
		return StageRelease
	case "rollout", "on_create":
		return StageRollout
	}
	return StageBuild
}

// Stages tells how a deployment went, or is going, from its log.
func Stages(dep store.Deployment, lines []logs.Line, now time.Time) []Stage {
	names := []string{StagePrepare, StageToolchains, StageBuild, StageRelease, StageRollout}
	started := map[string]time.Time{}
	var last time.Time
	for _, l := range lines {
		if l.Stream == logs.Moorage && (strings.HasPrefix(l.Text, "✔") || strings.HasPrefix(l.Text, "✘")) {
			last = l.Time
			continue
		}
		name := stageOf(l)
		if name == "" {
			continue
		}
		if _, seen := started[name]; !seen {
			started[name] = l.Time
		}
	}
	// Preparing begins with the log, and includes waiting for a build slot.
	if _, seen := started[StagePrepare]; len(lines) > 0 && (seen || builds(dep.SourceType)) {
		started[StagePrepare] = lines[0].Time
	}
	finished := store.Final(dep.Status)
	end := now
	switch {
	case !last.IsZero():
		end = last
	case finished && !dep.FinishedAt.IsZero():
		end = dep.FinishedAt
	}
	out := make([]Stage, 0, len(names))
	for i, name := range names {
		s := Stage{Name: name, Status: StagePending}
		at, ran := started[name]
		if !ran {
			if finished {
				s.Status = StageSkipped
			} else if laterStarted(names[i+1:], started) {
				s.Status = StageSkipped
			}
			out = append(out, s)
			continue
		}
		until, cut := end, false
		for _, later := range names[i+1:] {
			if t, ok := started[later]; ok {
				until, cut = t, true
				break
			}
		}
		s.Took = until.Sub(at)
		switch {
		case cut:
			s.Status = StageDone
		case dep.Status == store.Live:
			s.Status = StageDone
		case finished:
			s.Status = StageFailed
		default:
			s.Status = StageRunning
		}
		out = append(out, s)
	}
	return out
}

// builds reports whether deployments of a source type build anything.
func builds(sourceType string) bool {
	return sourceType == SourceUpload || sourceType == SourcePush || sourceType == SourceImage
}

func laterStarted(names []string, started map[string]time.Time) bool {
	for _, name := range names {
		if _, ok := started[name]; ok {
			return true
		}
	}
	return false
}
