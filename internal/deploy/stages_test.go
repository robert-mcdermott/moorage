package deploy

import (
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/logs"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func marker(at int, text string) logs.Line {
	return logs.Line{Time: time.Unix(1000+int64(at), 0).UTC(), Stream: logs.Moorage, Text: text}
}

func TestStages(t *testing.T) {
	now := time.Unix(1100, 0).UTC()
	full := []logs.Line{
		marker(0, "moorage  shop · production · dep_1"),
		marker(2, "▸ detected   go"),
		marker(3, "▸ toolchains go 1.25"),
		marker(5, "▸ install    go mod download"),
		{Time: time.Unix(1006, 0).UTC(), Stream: logs.Stdout, Text: "▸ rollout from the app's own output means nothing"},
		marker(8, "▸ build      go build"),
		marker(10, "▸ built      9 files"),
		marker(11, "▸ release    v2: ./migrate"),
		marker(13, "▸ rollout    web ×2"),
	}
	for name, tc := range map[string]struct {
		dep   store.Deployment
		lines []logs.Line
		want  map[string]string
		took  map[string]time.Duration
	}{
		"live": {
			dep:   store.Deployment{Status: store.Live, FinishedAt: time.Unix(1020, 0)},
			lines: append(append([]logs.Line{}, full...), marker(20, "✔ live       v2")),
			want: map[string]string{
				StagePrepare: StageDone, StageToolchains: StageDone, StageBuild: StageDone, StageRelease: StageDone, StageRollout: StageDone,
			},
			took: map[string]time.Duration{StagePrepare: 3 * time.Second, StageBuild: 6 * time.Second, StageRollout: 7 * time.Second},
		},
		"in the build": {
			dep:   store.Deployment{Status: store.Building},
			lines: full[:5],
			want: map[string]string{
				StagePrepare: StageDone, StageToolchains: StageDone, StageBuild: StageRunning, StageRelease: StagePending, StageRollout: StagePending,
			},
			took: map[string]time.Duration{StageBuild: 95 * time.Second},
		},
		"failed in the build": {
			dep:   store.Deployment{Status: store.Failed, FinishedAt: time.Unix(1009, 0)},
			lines: append(append([]logs.Line{}, full[:6]...), marker(9, "✘ failed     it broke")),
			want: map[string]string{
				StagePrepare: StageDone, StageToolchains: StageDone, StageBuild: StageFailed, StageRelease: StageSkipped, StageRollout: StageSkipped,
			},
			took: map[string]time.Duration{StageBuild: 4 * time.Second},
		},
		"no toolchains or release": {
			dep: store.Deployment{Status: store.Live},
			lines: []logs.Line{
				marker(0, "moorage  shop · production · dep_1"), marker(1, "▸ detected   static"), marker(2, "▸ built      1 files"), marker(3, "✔ live       v1"),
			},
			want: map[string]string{
				StagePrepare: StageDone, StageToolchains: StageSkipped, StageBuild: StageDone, StageRelease: StageSkipped, StageRollout: StageSkipped,
			},
		},
		"a rollback builds nothing": {
			dep: store.Deployment{Status: store.Live, SourceType: "rollback"},
			lines: []logs.Line{
				marker(0, "moorage  shop · production · dep_1"), marker(1, "▸ rollout    web ×1"), marker(4, "✔ live       v3"),
			},
			want: map[string]string{
				StagePrepare: StageSkipped, StageToolchains: StageSkipped, StageBuild: StageSkipped, StageRelease: StageSkipped, StageRollout: StageDone,
			},
			took: map[string]time.Duration{StageRollout: 3 * time.Second},
		},
		"interrupted, without a word in the log": {
			dep:   store.Deployment{Status: store.Failed, FinishedAt: time.Unix(1030, 0)},
			lines: full,
			want: map[string]string{
				StagePrepare: StageDone, StageToolchains: StageDone, StageBuild: StageDone, StageRelease: StageDone, StageRollout: StageFailed,
			},
			took: map[string]time.Duration{StageRollout: 17 * time.Second},
		},
		"superseded in the queue": {
			dep:  store.Deployment{Status: store.Superseded},
			want: map[string]string{StagePrepare: StageSkipped, StageToolchains: StageSkipped, StageBuild: StageSkipped, StageRelease: StageSkipped, StageRollout: StageSkipped},
		},
		"queued": {
			dep:  store.Deployment{Status: store.Queued},
			want: map[string]string{StagePrepare: StagePending, StageToolchains: StagePending, StageBuild: StagePending, StageRelease: StagePending, StageRollout: StagePending},
		},
	} {
		t.Run(name, func(t *testing.T) {
			stages := Stages(tc.dep, tc.lines, now)
			if len(stages) != 5 {
				t.Fatalf("%+v", stages)
			}
			for _, s := range stages {
				if s.Status != tc.want[s.Name] {
					t.Errorf("%s is %s, not %s", s.Name, s.Status, tc.want[s.Name])
				}
				if took, ok := tc.took[s.Name]; ok && s.Took != took {
					t.Errorf("%s took %s, not %s", s.Name, s.Took, took)
				}
			}
		})
	}
}
