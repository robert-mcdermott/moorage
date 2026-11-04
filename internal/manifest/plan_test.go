package manifest

import "testing"

func TestForEnv(t *testing.T) {
	one, three := 1, 3
	plan := Plan{
		Env:       map[string]string{"A": "base", "B": "base"},
		Processes: map[string]Process{Web: {Instances: &one}},
		Environments: map[string]EnvPlan{
			"staging": {Env: map[string]string{"B": "staging"}, Instances: map[string]int{Web: three}},
			"preview": {Env: map[string]string{"B": "preview"}},
		},
	}
	staging := plan.ForEnv("staging", "named")
	if staging.Env["A"] != "base" || staging.Env["B"] != "staging" || *staging.Processes[Web].Instances != 3 {
		t.Errorf("staging: %+v %+v", staging.Env, staging.Processes)
	}
	preview := plan.ForEnv("feature-x", "preview")
	if preview.Env["B"] != "preview" || *preview.Processes[Web].Instances != 1 {
		t.Errorf("a preview: %+v %+v", preview.Env, preview.Processes)
	}
	other := plan.ForEnv("other", "named")
	if other.Env["B"] != "base" {
		t.Errorf("another: %+v", other.Env)
	}
	if plan.Env["B"] != "base" || *plan.Processes[Web].Instances != 1 {
		t.Errorf("the plan itself changed: %+v", plan)
	}
}
