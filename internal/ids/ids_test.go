package ids

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

var format = regexp.MustCompile(`^app_[0-7][0-9A-HJKMNP-TV-Z]{25}$`)

func TestNewFormat(t *testing.T) {
	id := New(App)
	if !format.MatchString(id) {
		t.Fatalf("New(App) = %q, does not match %s", id, format)
	}
}

func TestNewIncreasing(t *testing.T) {
	// Many identifiers fall into the same millisecond; they must still
	// strictly increase.
	prev := New(App)
	for i := 0; i < 20000; i++ {
		id := New(App)
		if id <= prev {
			t.Fatalf("identifier %d not increasing: %q after %q", i, id, prev)
		}
		prev = id
	}
}

func TestParseRoundTrip(t *testing.T) {
	before := time.Now().Add(-time.Millisecond)
	id := New(Deployment)
	after := time.Now().Add(time.Millisecond)
	prefix, created, err := Parse(id)
	if err != nil {
		t.Fatal(err)
	}
	if prefix != Deployment {
		t.Errorf("prefix = %q, want %q", prefix, Deployment)
	}
	if created.Before(before) || created.After(after) {
		t.Errorf("created = %v, want between %v and %v", created, before, after)
	}
	if _, _, err := Parse(strings.ToLower(id)); err != nil {
		t.Errorf("lowercase identifier rejected: %v", err)
	}
}

func TestEncodeBounds(t *testing.T) {
	if got := encode(0, [10]byte{}); got != "00000000000000000000000000" {
		t.Errorf("encode(0, zeros) = %q", got)
	}
	var ones [10]byte
	for i := range ones {
		ones[i] = 0xff
	}
	if got := encode(1<<48-1, ones); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Errorf("encode(max, ones) = %q", got)
	}
	// One random bit at the bottom shows in the last character only.
	if got := encode(0, [10]byte{9: 1}); got != "00000000000000000000000001" {
		t.Errorf("encode(0, 1) = %q", got)
	}
	// The top random bit is bit 79, the highest of character 10's five bits
	// (0b10000 = 16 = "G").
	if got := encode(0, [10]byte{0: 0x80}); got != "0000000000G000000000000000" {
		t.Errorf("encode(0, top bit) = %q", got)
	}
}

func TestParseErrors(t *testing.T) {
	for _, id := range []string{
		"",
		"01JBXK3M8QZ5V4RRFFQ69G5FAV",      // no prefix
		"x_01JBXK3M8QZ5V4RRFFQ69G5FAV",    // prefix too short
		"Apps_01JBXK3M8QZ5V4RRFFQ69G5FAV", // uppercase prefix
		"app_01JBXK3M8QZ5V4RRFFQ69G5FA",   // too short
		"app_01JBXK3M8QZ5V4RRFFQ69G5FAVX", // too long
		"app_01JBXK3M8QZ5V4RRFFQ69G5FAU",  // U is not in the alphabet
		"app_81JBXK3M8QZ5V4RRFFQ69G5FAV",  // timestamp overflow
	} {
		if _, _, err := Parse(id); err == nil {
			t.Errorf("Parse(%q) succeeded", id)
		}
	}
}

func TestIncrementOverflow(t *testing.T) {
	r := [10]byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0xff}
	if !increment(&r) || r != [10]byte{8: 1} {
		t.Errorf("increment carried wrongly: %v", r)
	}
	var full [10]byte
	for i := range full {
		full[i] = 0xff
	}
	if increment(&full) {
		t.Error("increment of the maximum did not report overflow")
	}
}

func TestNewPanicsOnBadPrefix(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("New(\"Bad\") did not panic")
		}
	}()
	New("Bad")
}
