package deploy

import (
	"reflect"
	"testing"

	"github.com/robert-mcdermott/moorage/internal/apitypes"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func TestDiffLines(t *testing.T) {
	line := func(op, text string) apitypes.DiffLine { return apitypes.DiffLine{Op: op, Text: text} }
	same, add, del := apitypes.DiffSame, apitypes.DiffAdded, apitypes.DiffRemoved
	for name, tc := range map[string]struct {
		a, b []string
		want []apitypes.DiffLine
	}{
		"nothing":   {want: []apitypes.DiffLine{}},
		"the same":  {[]string{"x", "y"}, []string{"x", "y"}, []apitypes.DiffLine{line(same, "x"), line(same, "y")}},
		"a change":  {[]string{"x", "y", "z"}, []string{"x", "Y", "z"}, []apitypes.DiffLine{line(same, "x"), line(del, "y"), line(add, "Y"), line(same, "z")}},
		"added":     {[]string{"x"}, []string{"x", "y"}, []apitypes.DiffLine{line(same, "x"), line(add, "y")}},
		"removed":   {[]string{"x", "y"}, []string{"y"}, []apitypes.DiffLine{line(del, "x"), line(same, "y")}},
		"all anew":  {[]string{"x"}, []string{"y"}, []apitypes.DiffLine{line(del, "x"), line(add, "y")}},
		"from none": {nil, []string{"x"}, []apitypes.DiffLine{line(add, "x")}},
	} {
		if got := diffLines(tc.a, tc.b); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestDiffVars(t *testing.T) {
	before := []store.ConfigVar{
		{Name: "KEPT", Value: "1"}, {Name: "GONE", Value: "2"}, {Name: "CHANGED", Value: "3"},
		{Name: "SEALED", Sealed: []byte("a"), Secret: true}, {Name: "RESEALED", Sealed: []byte("b"), Secret: true},
	}
	after := []store.ConfigVar{
		{Name: "KEPT", Value: "1"}, {Name: "NEW", Value: "4"}, {Name: "CHANGED", Value: "5"},
		{Name: "SEALED", Sealed: []byte("a"), Secret: true}, {Name: "RESEALED", Sealed: []byte("c"), Secret: true},
		{Name: "NEWSECRET", Sealed: []byte("d"), Secret: true},
	}
	want := []apitypes.VarDiff{
		{Name: "CHANGED", Change: apitypes.DiffChangedVar},
		{Name: "GONE", Change: apitypes.DiffRemovedVar},
		{Name: "NEW", Change: apitypes.DiffAddedVar},
		{Name: "NEWSECRET", Change: apitypes.DiffAddedVar, Secret: true},
		{Name: "RESEALED", Change: apitypes.DiffChangedVar, Secret: true},
	}
	if got := diffVars(before, after); !reflect.DeepEqual(got, want) {
		t.Errorf("%+v", got)
	}
}
