package console

import (
	"strings"
	"testing"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

func TestNiceCeiling(t *testing.T) {
	for _, c := range []struct {
		v, top float64
		unit   string
	}{
		{0, 1, "rate"}, {3, 3, "rate"}, {7, 8, "rate"}, {87, 100, "ms"}, {120, 150, "percent"}, {1200, 1500, "ms"},
		{150 * (1 << 20), 150 * (1 << 20), "bytes"}, {3 * (1 << 30), 3 * (1 << 30), "bytes"},
	} {
		top, step := niceCeiling(c.v, c.unit)
		if top != c.top {
			t.Errorf("%v %s: top %v, want %v", c.v, c.unit, top, c.top)
		}
		if n := top / step; n > 5 || n < 1 {
			t.Errorf("%v %s: %v steps", c.v, c.unit, n)
		}
	}
}

func TestAxisValue(t *testing.T) {
	for _, c := range []struct {
		unit string
		v    float64
		want string
	}{
		{"percent", 0.1 * 6, "0.6%"}, {"percent", 0.6000000000000001, "0.6%"}, {"rate", 800, "800"}, {"ms", 1500, "1.5 s"}, {"ms", 4, "4 ms"},
		{"bytes", 30 * (1 << 20), "30 MiB"}, {"bytes", 0, "0 B"},
	} {
		if got := axisValue(c.unit, c.v); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.unit, c.v, got, c.want)
		}
	}
}

func TestFormatValue(t *testing.T) {
	for _, c := range []struct {
		unit string
		v    float64
		want string
	}{
		{"rate", 0, "0/min"}, {"rate", 2.5, "2.50/min"}, {"rate", 12.34, "12.3/min"}, {"rate", 1234, "1234/min"},
		{"ms", 0.4, "0.4 ms"}, {"ms", 42, "42 ms"}, {"ms", 2500, "2.50 s"},
		{"percent", 3.14, "3.1%"}, {"percent", 42.4, "42%"},
		{"bytes", 512, "512 B"}, {"bytes", 1536, "1.5 KiB"}, {"bytes", 150 * (1 << 20), "150 MiB"},
	} {
		if got := formatValue(c.unit, c.v); got != c.want {
			t.Errorf("%s %v: %q, want %q", c.unit, c.v, got, c.want)
		}
	}
}

func TestBuildChart(t *testing.T) {
	to := time.Now().Truncate(time.Minute)
	from := to.Add(-time.Hour)
	points := func(start, end int) []metrics.Point {
		var out []metrics.Point
		for i := start; i < end; i++ {
			out = append(out, metrics.Point{At: from.Add(time.Duration(i) * time.Minute), Avg: float64(i)})
		}
		return out
	}
	chart := metrics.Chart{Key: "cpu", Title: "CPU <now>", Unit: "percent", Lines: []metrics.Line{
		// A gap in the middle, and a line with a name that wants escaping.
		{Name: "web.1", Points: append(points(0, 10), points(30, 40)...)},
		{Name: `we"b<2`, Points: points(59, 60)},
	}}
	marks := []store.ReleaseMark{{At: from.Add(5 * time.Minute), Version: 3, By: `a<b>c`}, {At: from.Add(-time.Hour), Version: 2}}
	v := buildChart(chart, from, to, time.Minute, marks)
	svg := string(v.SVG)
	if strings.Count(svg, "M") < 3 || strings.Count(svg, `class="chart-line s1"`) != 1 || strings.Count(svg, `class="chart-line s2"`) != 1 {
		t.Errorf("the lines, with a gap: %s", svg)
	}
	if strings.Count(svg, `<g class="chart-mark"`) != 1 || !strings.Contains(svg, "v3") || strings.Contains(svg, "v2<") {
		t.Errorf("the marks in range: %s", svg)
	}
	if strings.Contains(svg, "<b>") || strings.Contains(svg, "<now>") {
		t.Errorf("what wants escaping was not: %s", svg)
	}
	if !strings.Contains(svg, `class="chart-dot s2"`) {
		t.Errorf("a point on its own has a dot: %s", svg)
	}
	if len(v.Legend) != 2 || v.Legend[0].Latest != "39%" || v.Legend[1].Latest != "59%" {
		t.Errorf("the legend: %+v", v.Legend)
	}
	if len(v.Table.Head) != 3 || len(v.Table.Rows) == 0 || !strings.Contains(v.Data, `"unit":"percent"`) {
		t.Errorf("the numbers: %+v %s", v.Table.Head, v.Data)
	}
	if empty := buildChart(metrics.Chart{Title: "None"}, from, to, time.Minute, nil); !empty.Empty || empty.SVG != "" {
		t.Errorf("nothing to draw: %+v", empty)
	}
	// The same line keeps its color when a line after it comes and goes.
	short := buildChart(metrics.Chart{Unit: "percent", Lines: chart.Lines[:1]}, from, to, time.Minute, nil)
	if short.Legend[0].Slot != v.Legend[0].Slot {
		t.Errorf("slots: %d, %d", short.Legend[0].Slot, v.Legend[0].Slot)
	}
}

func TestSparkline(t *testing.T) {
	if s := string(sparkline([]float64{0, 1, 2}, `a"b`)); !strings.Contains(s, `aria-label="a&#34;b"`) || !strings.Contains(s, "spark-line") {
		t.Errorf("%s", s)
	}
	if s := string(sparkline(make([]float64, 5), "flat")); !strings.Contains(s, "M0.0 22.0") {
		t.Errorf("nothing at all: %s", s)
	}
}
