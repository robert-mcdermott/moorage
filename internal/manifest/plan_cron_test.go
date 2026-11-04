package manifest

import "testing"

func TestCronRunsIn(t *testing.T) {
	for _, c := range []struct {
		list      []string
		env, kind string
		want      bool
	}{
		{nil, "production", "production", true},
		{nil, "staging", "named", false},
		{[]string{"staging"}, "staging", "named", true},
		{[]string{"staging"}, "production", "production", false},
		{[]string{"*"}, "production", "production", true},
		{[]string{"*"}, "staging", "named", true},
		{[]string{"*"}, "topic", "preview", false},
		{[]string{"preview"}, "topic", "preview", true},
		{[]string{"preview"}, "staging", "named", false},
		{[]string{"*", "preview"}, "topic", "preview", true},
	} {
		if got := (CronPlan{Environments: c.list}).RunsIn(c.env, c.kind); got != c.want {
			t.Errorf("%v in %s (%s): %v", c.list, c.env, c.kind, got)
		}
	}
}
