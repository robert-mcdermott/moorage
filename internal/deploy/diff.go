package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/manifest"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// ResolvedManifest says in plain words what a build's plan does.
func ResolvedManifest(b store.Build) (string, error) {
	var plan manifest.Plan
	if err := json.Unmarshal(b.Plan, &plan); err != nil {
		return "", err
	}
	plan.Defaults()
	var buf bytes.Buffer
	manifest.Explain(&buf, plan)
	return buf.String(), nil
}

// DiffReleases says what changed from one release of an environment to another: its
// build, its resolved manifest, and which variables changed. It says nothing of the
// variables' values.
func DiffReleases(ctx context.Context, db *store.DB, app, env string, from, to int) (apitypes.ReleaseDiff, error) {
	diff := apitypes.ReleaseDiff{From: from, To: to, Manifest: []apitypes.DiffLine{}, Vars: []apitypes.VarDiff{}}
	relFrom, buildFrom, err := db.ReleaseVersion(ctx, app, env, from)
	if err != nil {
		return diff, err
	}
	relTo, buildTo, err := db.ReleaseVersion(ctx, app, env, to)
	if err != nil {
		return diff, err
	}
	diff.BuildChanged = buildFrom.ID != buildTo.ID
	before, err := ResolvedManifest(buildFrom)
	if err != nil {
		return diff, err
	}
	after, err := ResolvedManifest(buildTo)
	if err != nil {
		return diff, err
	}
	diff.Manifest = diffLines(splitLines(before), splitLines(after))
	diff.Vars = diffVars(relFrom.Config, relTo.Config)
	return diff, nil
}

func splitLines(s string) []string {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// diffLines is a longest-common-subsequence diff: enough for a page of text.
func diffLines(a, b []string) []apitypes.DiffLine {
	lcs := make([][]int, len(a)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	out := []apitypes.DiffLine{}
	i, j := 0, 0
	for i < len(a) || j < len(b) {
		switch {
		case i < len(a) && j < len(b) && a[i] == b[j]:
			out = append(out, apitypes.DiffLine{Op: apitypes.DiffSame, Text: a[i]})
			i++
			j++
		case j < len(b) && (i == len(a) || lcs[i][j+1] > lcs[i+1][j]):
			out = append(out, apitypes.DiffLine{Op: apitypes.DiffAdded, Text: b[j]})
			j++
		default:
			out = append(out, apitypes.DiffLine{Op: apitypes.DiffRemoved, Text: a[i]})
			i++
		}
	}
	return out
}

// diffVars compares what two releases ran with. A secret's sealed value is the same
// bytes until someone sets it again, even to what it was.
func diffVars(before, after []store.ConfigVar) []apitypes.VarDiff {
	was := map[string]store.ConfigVar{}
	for _, v := range before {
		was[v.Name] = v
	}
	out := []apitypes.VarDiff{}
	for _, v := range after {
		old, had := was[v.Name]
		delete(was, v.Name)
		switch {
		case !had:
			out = append(out, apitypes.VarDiff{Name: v.Name, Change: apitypes.DiffAddedVar, Secret: v.Secret})
		case old.Value != v.Value || !bytes.Equal(old.Sealed, v.Sealed) || old.Secret != v.Secret || old.Build != v.Build:
			out = append(out, apitypes.VarDiff{Name: v.Name, Change: apitypes.DiffChangedVar, Secret: v.Secret})
		}
	}
	for _, v := range was {
		out = append(out, apitypes.VarDiff{Name: v.Name, Change: apitypes.DiffRemovedVar, Secret: v.Secret})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
