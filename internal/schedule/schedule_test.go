package schedule

import (
	"testing"
	"time"
)

func TestNext(t *testing.T) {
	at := time.Date(2026, 3, 14, 10, 30, 20, 0, time.UTC) // a Saturday
	for expr, want := range map[string]string{
		"* * * * *":       "2026-03-14 10:31",
		"0 * * * *":       "2026-03-14 11:00",
		"@hourly":         "2026-03-14 11:00",
		"@daily":          "2026-03-15 00:00",
		"@midnight":       "2026-03-15 00:00",
		"@weekly":         "2026-03-15 00:00",
		"@monthly":        "2026-04-01 00:00",
		"@yearly":         "2027-01-01 00:00",
		"*/15 * * * *":    "2026-03-14 10:45",
		"5,35 * * * *":    "2026-03-14 10:35",
		"0 9-17 * * *":    "2026-03-14 11:00",
		"0 3 * * MON":     "2026-03-16 03:00",
		"0 3 * * 7":       "2026-03-15 03:00",
		"0 3 * * 0":       "2026-03-15 03:00",
		"0 0 1 JAN *":     "2027-01-01 00:00",
		"0 0 13 * FRI":    "2026-03-20 00:00", // Friday the 20th, before the 13th of April
		"0 0 29 2 *":      "2028-02-29 00:00",
		"10-20/5 * * * *": "2026-03-14 11:10",
	} {
		s, err := Parse(expr)
		if err != nil {
			t.Errorf("%q: %v", expr, err)
			continue
		}
		if got := s.Next(at, time.UTC).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%q: %s, want %s", expr, got, want)
		}
	}
}

func TestNextInAZone(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip(err)
	}
	s, _ := Parse("0 3 * * *")
	got := s.Next(time.Date(2026, 3, 14, 12, 0, 0, 0, time.UTC), ny)
	if want := time.Date(2026, 3, 15, 3, 0, 0, 0, ny); !got.Equal(want) {
		t.Errorf("%v, want %v", got, want)
	}
	// 02:30 does not happen on the morning the clocks go forward: it runs on the next day it does.
	s, _ = Parse("30 2 * * *")
	got = s.Next(time.Date(2026, 3, 8, 6, 0, 0, 0, time.UTC), ny)
	if want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny); !got.Equal(want) {
		t.Errorf("across the change of the clocks: %v, want %v", got, want)
	}
}

func TestEvery(t *testing.T) {
	s, err := Parse("@every 15m")
	if err != nil || s.Every() != 15*time.Minute {
		t.Fatalf("%+v %v", s, err)
	}
	at := time.Date(2026, 3, 14, 10, 30, 20, 0, time.UTC)
	if got := s.Next(at, nil); !got.Equal(at.Add(15 * time.Minute)) {
		t.Errorf("%v", got)
	}
	if s, err := Parse("@every 1d"); err != nil || s.Every() != 24*time.Hour {
		t.Errorf("a day: %+v %v", s, err)
	}
}

func TestNever(t *testing.T) {
	s, err := Parse("0 0 31 2 *")
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Next(time.Now(), time.UTC); !got.IsZero() {
		t.Errorf("the 31st of February: %v", got)
	}
}

func TestBad(t *testing.T) {
	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *", "* * * 13 *", "* * * * 8", "a * * * *",
		"*/0 * * * *", "5-1 * * * *", "@never", "@every", "@every soon", "@every 1ms", "@every -5m",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}
