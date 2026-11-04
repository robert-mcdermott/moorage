package manifest

import "testing"

func TestWithFormation(t *testing.T) {
	two := 2
	plan := Plan{Processes: map[string]Process{"web": {Instances: &two}, "worker": {Instances: &two}}}
	scaled := plan.WithFormation(map[string]int{"web": 5, "gone": 9})
	if *scaled.Processes["web"].Instances != 5 || *scaled.Processes["worker"].Instances != 2 {
		t.Errorf("%+v", scaled.Processes)
	}
	if _, ok := scaled.Processes["gone"]; ok {
		t.Errorf("%+v", scaled.Processes)
	}
	if *plan.Processes["web"].Instances != 2 {
		t.Errorf("the plan itself changed: %d", *plan.Processes["web"].Instances)
	}
	zero := plan.WithFormation(map[string]int{"worker": 0})
	if *zero.Processes["worker"].Instances != 0 {
		t.Errorf("%+v", zero.Processes)
	}
}
