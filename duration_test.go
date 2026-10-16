package main

import (
	"testing"
	"time"
)

func TestParseReminderShorthand(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration // for fixed units
		mon  int           // for calendar months
		desc string
	}{
		{"3h", 3 * time.Hour, 0, "in 3 hours"},
		{"3d", 72 * time.Hour, 0, "in 3 days"},
		{"2w", 14 * 24 * time.Hour, 0, "in 2 weeks"},
		{"2m", 0, 2, "in 2 months"},
		{"2mo", 0, 2, "in 2 months"},
		{"20min", 20 * time.Minute, 0, "in 20 minutes"},
		{"20mi", 20 * time.Minute, 0, "in 20 minutes"},
		{"45s", 45 * time.Second, 0, "in 45 seconds"},
		{"1d12h", 36 * time.Hour, 0, "in 1 day 12 hours"},
		{"h", time.Hour, 0, "in 1 hour"},
		{"an hour", time.Hour, 0, "in 1 hour"},
		{"in 2 hours", 2 * time.Hour, 0, "in 2 hours"},
	}
	for _, c := range cases {
		due, ok, desc := parseReminder(c.in, now)
		if !ok {
			t.Fatalf("%q did not parse", c.in)
		}
		if c.mon > 0 {
			want := now.AddDate(0, c.mon, 0).Unix()
			if due != want {
				t.Fatalf("%q due = %d, want %d", c.in, due, want)
			}
		} else if due != now.Add(c.want).Unix() {
			t.Fatalf("%q due = %d, want %d", c.in, due, now.Add(c.want).Unix())
		}
		if len(desc) < len(c.desc) || desc[:len(c.desc)] != c.desc {
			t.Fatalf("%q desc = %q, want prefix %q", c.in, desc, c.desc)
		}
	}
}

func TestParseReminderSpecialAndInvalid(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	due, ok, desc := parseReminder("clear", now)
	if !ok || due != 0 || desc != "no reminder" {
		t.Fatalf("clear = %d %v %q", due, ok, desc)
	}
	due, ok, _ = parseReminder("tomorrow", now)
	if !ok || due != now.AddDate(0, 0, 1).Truncate(24*time.Hour).Add(9*time.Hour).Unix() {
		// tomorrow is next day at 09:00 local
		want := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC).Unix()
		if due != want {
			t.Fatalf("tomorrow due = %d, want %d", due, want)
		}
	}
	for _, bad := range []string{"", "banana", "3 parsecs", "2w extra"} {
		if _, ok, _ := parseReminder(bad, now); ok {
			t.Fatalf("%q parsed but should not", bad)
		}
	}
}
