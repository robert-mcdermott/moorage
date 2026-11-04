package metrics

import "time"

// Range is a stretch of time a page or a client can ask for.
type Range struct {
	Key, Label string
	Span       time.Duration
	// Points is about how many a chart of it has.
	Points int
}

// Ranges are those there are, the shortest first.
var Ranges = []Range{
	{"live", "Live", 15 * time.Minute, 90},
	{"1h", "1 hour", time.Hour, 120},
	{"24h", "24 hours", 24 * time.Hour, 288},
	{"7d", "7 days", 7 * 24 * time.Hour, 336},
	{"30d", "30 days", 30 * 24 * time.Hour, 360},
}

// RangeOf reads a range's key, or gives the hour.
func RangeOf(key string) (Range, bool) {
	for _, r := range Ranges {
		if r.Key == key {
			return r, true
		}
	}
	return Ranges[1], key == ""
}
