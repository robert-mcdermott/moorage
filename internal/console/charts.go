package console

import (
	"encoding/json"
	"fmt"
	"html/template"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/robert-mcdermott/moorage/internal/observe/metrics"
	"github.com/robert-mcdermott/moorage/internal/store"
)

// Charts are drawn here, as SVG, so that a page has them without scripts. Scripts add what follows the
// pointer. Colors are classes, s1 to s8, in the order that was validated for them: a line keeps its
// slot by its place in the chart's own list of names, which are sorted, so that it does not change
// color as others come and go.
const (
	// About the size it is drawn at, side by side, so that its text is the size it says.
	chartWidth, chartHeight                  = 480.0, 190.0
	plotLeft, plotRight, plotTop, plotBottom = 58.0, 8.0, 16.0, 24.0
	maxSlots                                 = 8
)

// chartView is a chart on a page.
type chartView struct {
	Key, Title string
	SVG        template.HTML
	// Data is what the script needs to say what is under the pointer.
	Data   string
	Legend []legendItem
	Empty  bool
	Table  chartTable
	Label  string
}

type legendItem struct {
	Name, Latest string
	Slot         int
}

type chartTable struct {
	Head []string
	Rows [][]string
}

// formatValue writes a value in its unit.
func formatValue(unit string, v float64) string {
	switch unit {
	case "rate":
		switch {
		case v >= 100:
			return fmt.Sprintf("%.0f/min", v)
		case v >= 10:
			return fmt.Sprintf("%.1f/min", v)
		}
		return strings.TrimSuffix(fmt.Sprintf("%.2f", v), ".00") + "/min"
	case "ms":
		if v >= 1000 {
			return fmt.Sprintf("%.2f s", v/1000)
		}
		if v >= 10 {
			return fmt.Sprintf("%.0f ms", v)
		}
		return fmt.Sprintf("%.1f ms", v)
	case "percent":
		if v >= 10 {
			return fmt.Sprintf("%.0f%%", v)
		}
		return fmt.Sprintf("%.1f%%", v)
	case "bytes":
		return chartBytes(v)
	}
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func chartBytes(v float64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	i := 0
	for v >= 1024 && i < len(units)-1 {
		v /= 1024
		i++
	}
	if v >= 100 || i == 0 {
		return fmt.Sprintf("%.0f %s", v, units[i])
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}

// niceCeiling finds the top of an axis: a round number at or above a value, with about four steps.
func niceCeiling(v float64, unit string) (top, step float64) {
	if v <= 0 {
		v = 1
	}
	base := 10.0
	if unit == "bytes" {
		base = 1024
		// Round in the largest unit that fits.
		size := 1.0
		for v/size >= 1024 {
			size *= 1024
		}
		s, t := roundStep(v/size, 10)
		return t * size, s * size
	}
	s, t := roundStep(v, base)
	return t, s
}

func roundStep(v, base float64) (step, top float64) {
	raw := v / 4
	mag := math.Pow(10, math.Floor(math.Log10(raw)))
	for _, m := range []float64{1, 2, 2.5, 5, 10} {
		if s := m * mag; v/s <= 4.0001 {
			step = s
			break
		}
	}
	if step == 0 {
		step = 10 * mag
	}
	return step, math.Ceil(v/step-1e-9) * step
}

// buildChart draws a chart of a view's over a range.
func buildChart(c metrics.Chart, from, to time.Time, step time.Duration, marks []store.ReleaseMark) chartView {
	view := chartView{Key: c.Key, Title: c.Title, Empty: len(c.Lines) == 0, Label: c.Title}
	if view.Empty {
		return view
	}
	plotW, plotH := chartWidth-plotLeft-plotRight, chartHeight-plotTop-plotBottom
	x := func(t time.Time) float64 {
		return plotLeft + plotW*t.Sub(from).Seconds()/to.Sub(from).Seconds()
	}
	top := 0.0
	for _, l := range c.Lines {
		for _, p := range l.Points {
			top = math.Max(top, p.Avg)
		}
	}
	ceiling, tick := niceCeiling(top, c.Unit)
	y := func(v float64) float64 { return plotTop + plotH*(1-v/ceiling) }

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart-svg" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s">`, chartWidth, chartHeight, template.HTMLEscapeString(c.Title+" over this range; the numbers are below the chart"))
	// Hairlines, then the axis's numbers.
	for v := 0.0; v <= ceiling*1.0001; v += tick {
		class := "chart-grid"
		if v == 0 {
			class = "chart-axis"
		}
		fmt.Fprintf(&b, `<line class="%s" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, class, plotLeft, chartWidth-plotRight, y(v), y(v))
		fmt.Fprintf(&b, `<text class="chart-tick" x="%.1f" y="%.1f" text-anchor="end">%s</text>`, plotLeft-8, y(v)+4, template.HTMLEscapeString(axisValue(c.Unit, v)))
	}
	layout := "15:04"
	if to.Sub(from) > 36*time.Hour {
		layout = "Jan 2"
	}
	for i := 0; i <= 4; i++ {
		at := from.Add(time.Duration(i) * to.Sub(from) / 4)
		anchor := "middle"
		switch i {
		case 0:
			anchor = "start"
		case 4:
			anchor = "end"
		}
		fmt.Fprintf(&b, `<text class="chart-tick" x="%.1f" y="%.1f" text-anchor="%s">%s</text>`, x(at), chartHeight-6, anchor, at.Local().Format(layout))
	}
	// The releases, behind the lines.
	for _, m := range marks {
		if m.At.Before(from) || m.At.After(to) {
			continue
		}
		who := ""
		if m.By != "" {
			who = " by " + m.By
		}
		fmt.Fprintf(&b, `<g class="chart-mark"><title>%s</title><line x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, template.HTMLEscapeString(fmt.Sprintf("v%d%s, %s", m.Version, who, m.At.Local().Format("Jan 2 15:04"))), x(m.At), x(m.At), plotTop, chartHeight-plotBottom)
		if len(marks) <= 12 {
			fmt.Fprintf(&b, `<text x="%.1f" y="%.1f" text-anchor="middle">v%d</text>`, x(m.At), plotTop-4, m.Version)
		}
		b.WriteString(`</g>`)
	}
	// The lines: a gap wherever nothing was running, and a dot at each end of what is on its own.
	gap := 5 * step / 2
	type alignedLine struct {
		name   string
		values map[int64]float64
	}
	times := map[int64]bool{}
	aligned := make([]alignedLine, 0, len(c.Lines))
	for i, l := range c.Lines {
		slot := i%maxSlots + 1
		var path strings.Builder
		var prev time.Time
		values := make(map[int64]float64, len(l.Points))
		for j, p := range l.Points {
			values[p.At.Unix()] = p.Avg
			times[p.At.Unix()] = true
			px, py := x(p.At), y(p.Avg)
			if j == 0 || p.At.Sub(prev) > gap {
				isolated := j+1 == len(l.Points) || l.Points[j+1].At.Sub(p.At) > gap
				if isolated {
					fmt.Fprintf(&b, `<circle class="chart-dot s%d" cx="%.1f" cy="%.1f" r="3"/>`, slot, px, py)
				}
				fmt.Fprintf(&path, "M%.1f %.1f", px, py)
			} else {
				fmt.Fprintf(&path, "L%.1f %.1f", px, py)
			}
			prev = p.At
		}
		fmt.Fprintf(&b, `<path class="chart-line s%d" d="%s"/>`, slot, path.String())
		latest := "—"
		if n := len(l.Points); n > 0 {
			latest = formatValue(c.Unit, l.Points[n-1].Avg)
		}
		view.Legend = append(view.Legend, legendItem{Name: l.Name, Latest: latest, Slot: slot})
		aligned = append(aligned, alignedLine{l.Name, values})
	}
	b.WriteString(`</svg>`)
	view.SVG = template.HTML(b.String())

	// What the script needs, and what those without it can read, from the same numbers.
	keys := make([]int64, 0, len(times))
	for k := range times {
		keys = append(keys, k)
	}
	sortInt64(keys)
	type dataLine struct {
		Name   string     `json:"name"`
		Slot   int        `json:"slot"`
		Values []*float64 `json:"values"`
	}
	data := struct {
		Unit  string     `json:"unit"`
		From  int64      `json:"from"`
		To    int64      `json:"to"`
		Left  float64    `json:"left"`
		Right float64    `json:"right"`
		Top   float64    `json:"top"`
		Bot   float64    `json:"bottom"`
		W     float64    `json:"width"`
		H     float64    `json:"height"`
		Times []int64    `json:"times"`
		Lines []dataLine `json:"lines"`
	}{c.Unit, from.Unix(), to.Unix(), plotLeft, chartWidth - plotRight, plotTop, chartHeight - plotBottom, chartWidth, chartHeight, keys, nil}
	view.Table.Head = []string{"When"}
	for i, l := range aligned {
		line := dataLine{Name: l.name, Slot: i%maxSlots + 1, Values: make([]*float64, len(keys))}
		for j, k := range keys {
			if v, ok := l.values[k]; ok {
				rounded := math.Round(v*100) / 100
				line.Values[j] = &rounded
			}
		}
		data.Lines = append(data.Lines, line)
		view.Table.Head = append(view.Table.Head, l.name)
	}
	if encoded, err := json.Marshal(data); err == nil {
		view.Data = string(encoded)
	}
	stride := max(1, len(keys)/24)
	for j := len(keys) - 1; j >= 0; j -= stride {
		row := []string{time.Unix(keys[j], 0).Local().Format("Jan 2 15:04")}
		for _, l := range aligned {
			if v, ok := l.values[keys[j]]; ok {
				row = append(row, formatValue(c.Unit, v))
			} else {
				row = append(row, "—")
			}
		}
		view.Table.Rows = append(view.Table.Rows, row)
	}
	return view
}

func sortInt64(list []int64) {
	for i := 1; i < len(list); i++ {
		for j := i; j > 0 && list[j-1] > list[j]; j-- {
			list[j-1], list[j] = list[j], list[j-1]
		}
	}
}

// axisValue writes a value on an axis, as short as it can be.
func axisValue(unit string, v float64) string {
	short := func(v float64) string {
		return strconv.FormatFloat(v, 'g', 4, 64)
	}
	switch unit {
	case "rate":
		return short(v)
	case "ms":
		if v >= 1000 {
			return short(v/1000) + " s"
		}
		return short(v) + " ms"
	case "percent":
		return short(v) + "%"
	case "bytes":
		units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
		i := 0
		for v >= 1024 && i < len(units)-1 {
			v /= 1024
			i++
		}
		return short(v) + " " + units[i]
	}
	return formatValue(unit, v)
}

// sparkline draws a little line of values, with no axes, to be read for its shape beside its value.
func sparkline(values []float64, label string) template.HTML {
	const w, h, pad = 96.0, 24.0, 2.0
	top := 0.0
	for _, v := range values {
		top = math.Max(top, v)
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="spark" viewBox="0 0 %.0f %.0f" role="img" aria-label="%s" preserveAspectRatio="none"><path class="spark-line" d="`, w, h, template.HTMLEscapeString(label))
	for i, v := range values {
		px := w * float64(i) / math.Max(1, float64(len(values)-1))
		py := h - pad
		if top > 0 {
			py = h - pad - (h-2*pad)*v/top
		}
		if i == 0 {
			fmt.Fprintf(&b, "M%.1f %.1f", px, py)
		} else {
			fmt.Fprintf(&b, "L%.1f %.1f", px, py)
		}
	}
	b.WriteString(`"/></svg>`)
	return template.HTML(b.String())
}
