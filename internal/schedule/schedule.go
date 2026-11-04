// Package schedule reads when cron jobs run: five-field cron expressions, the usual names for them
// (@hourly, @daily), and @every 15m.
package schedule

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule says when something runs next.
type Schedule struct {
	every                         time.Duration
	minute, hour, dom, month, dow uint64
	domRestricted, dowRestricted  bool
	text                          string
}

var aliases = map[string]string{
	"@yearly": "0 0 1 1 *", "@annually": "0 0 1 1 *", "@monthly": "0 0 1 * *", "@weekly": "0 0 * * 0",
	"@daily": "0 0 * * *", "@midnight": "0 0 * * *", "@hourly": "0 * * * *",
}

var monthNames = map[string]int{"jan": 1, "feb": 2, "mar": 3, "apr": 4, "may": 5, "jun": 6, "jul": 7, "aug": 8, "sep": 9, "oct": 10, "nov": 11, "dec": 12}
var dayNames = map[string]int{"sun": 0, "mon": 1, "tue": 2, "wed": 3, "thu": 4, "fri": 5, "sat": 6}

// Parse reads a schedule.
func Parse(expr string) (Schedule, error) {
	expr = strings.TrimSpace(expr)
	s := Schedule{text: expr}
	if rest, ok := strings.CutPrefix(expr, "@every "); ok {
		d, err := parseDuration(strings.TrimSpace(rest))
		if err != nil {
			return s, err
		}
		if d < time.Second {
			return s, fmt.Errorf("%q is too often: at most once a second", expr)
		}
		s.every = d
		return s, nil
	}
	if alias, ok := aliases[expr]; ok {
		expr = alias
	} else if strings.HasPrefix(expr, "@") {
		return s, fmt.Errorf("%q is not a schedule: @hourly, @daily, @weekly, @monthly, @yearly or @every 15m", expr)
	}
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return s, fmt.Errorf("%q is not a schedule: it needs five fields (minute hour day month weekday), or a name such as @daily", s.text)
	}
	var err error
	if s.minute, _, err = parseField(fields[0], 0, 59, nil); err != nil {
		return s, fmt.Errorf("the minute of %q: %w", s.text, err)
	}
	if s.hour, _, err = parseField(fields[1], 0, 23, nil); err != nil {
		return s, fmt.Errorf("the hour of %q: %w", s.text, err)
	}
	if s.dom, s.domRestricted, err = parseField(fields[2], 1, 31, nil); err != nil {
		return s, fmt.Errorf("the day of %q: %w", s.text, err)
	}
	if s.month, _, err = parseField(fields[3], 1, 12, monthNames); err != nil {
		return s, fmt.Errorf("the month of %q: %w", s.text, err)
	}
	if s.dow, s.dowRestricted, err = parseField(fields[4], 0, 7, dayNames); err != nil {
		return s, fmt.Errorf("the weekday of %q: %w", s.text, err)
	}
	if s.dow&(1<<7) != 0 { // both 0 and 7 are Sunday
		s.dow |= 1
	}
	return s, nil
}

func parseDuration(s string) (time.Duration, error) {
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.ParseFloat(days, 64)
		if err != nil || n <= 0 {
			return 0, fmt.Errorf("%q is not a duration such as 15m", s)
		}
		return time.Duration(n * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 15m", s)
	}
	return d, nil
}

func parseField(field string, low, high int, names map[string]int) (bits uint64, restricted bool, err error) {
	restricted = field != "*"
	for _, part := range strings.Split(field, ",") {
		spec, stepText, hasStep := strings.Cut(part, "/")
		step := 1
		if hasStep {
			if step, err = strconv.Atoi(stepText); err != nil || step <= 0 {
				return 0, false, fmt.Errorf("%q is not a step", stepText)
			}
		}
		from, to := low, high
		switch {
		case spec == "*":
		case strings.Contains(spec, "-"):
			a, b, _ := strings.Cut(spec, "-")
			if from, err = parseValue(a, low, high, names); err != nil {
				return 0, false, err
			}
			if to, err = parseValue(b, low, high, names); err != nil {
				return 0, false, err
			}
			if from > to {
				return 0, false, fmt.Errorf("%q runs backwards", spec)
			}
		default:
			if from, err = parseValue(spec, low, high, names); err != nil {
				return 0, false, err
			}
			if !hasStep {
				to = from
			}
		}
		for v := from; v <= to; v += step {
			bits |= 1 << v
		}
	}
	return bits, restricted, nil
}

func parseValue(s string, low, high int, names map[string]int) (int, error) {
	if v, ok := names[strings.ToLower(s)]; ok {
		return v, nil
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	if v < low || v > high {
		return 0, fmt.Errorf("%d is not between %d and %d", v, low, high)
	}
	return v, nil
}

// Every is how long between runs of an @every schedule, or 0 for one that goes by the clock.
func (s Schedule) Every() time.Duration { return s.every }

// String is the schedule as it was written.
func (s Schedule) String() string { return s.text }

func (s Schedule) matches(t time.Time) bool {
	if s.minute&(1<<t.Minute()) == 0 || s.hour&(1<<t.Hour()) == 0 || s.month&(1<<int(t.Month())) == 0 {
		return false
	}
	dom := s.dom&(1<<t.Day()) != 0
	dow := s.dow&(1<<int(t.Weekday())) != 0
	// As cron has always had it: with both restricted, either will do.
	if s.domRestricted && s.dowRestricted {
		return dom || dow
	}
	return dom && dow
}

// Next is the first time after a time that it runs, in a location, or the zero time if it never
// does, as on the 31st of February.
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	if s.every > 0 {
		return after.Add(s.every)
	}
	if loc == nil {
		loc = time.Local
	}
	t := after.In(loc).Truncate(time.Minute).Add(time.Minute)
	limit := t.AddDate(5, 0, 0)
	for t.Before(limit) {
		if s.month&(1<<int(t.Month())) == 0 {
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
			continue
		}
		if s.matches(t) {
			return t
		}
		t = t.Add(time.Minute)
	}
	return time.Time{}
}
