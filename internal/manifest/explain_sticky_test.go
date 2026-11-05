// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"bytes"
	"testing"
)

func TestExplainSaysStickyAndProtocol(t *testing.T) {
	yes := true
	one := 1
	mk := func(p Process) Plan {
		p.Expose, p.Instances = &yes, &one
		return Plan{Kind: KindProcess, Runtime: "command", Processes: map[string]Process{"web": p}}
	}
	var a, b, c bytes.Buffer
	Explain(&a, mk(Process{Command: "run"}))
	Explain(&b, mk(Process{Command: "run", Sticky: true}))
	Explain(&c, mk(Process{Command: "run", Protocol: "h2c"}))
	if a.String() == b.String() {
		t.Errorf("sticky changes nothing in what a release says:\n%s", a.String())
	}
	if a.String() == c.String() {
		t.Errorf("h2c changes nothing in what a release says")
	}
}
