package main

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// duration.go parses the snooze syntax for the z dialog.
//
// SOTA shape: Superhuman takes natural language and shows the resolved time
// while you type. Linear CLIs take 1d / 2w shorthand. This parser takes both:
// a count plus a unit, compound sequences, bare units, a/an as one, and the
// words tomorrow and clear. The caller always renders the resolved absolute
// time beside the text, so an ambiguous unit is visible before it commits.
//
// Units: s sec mi min h hr d dy w wk m mo mon y yr and the spelled-out words.
// m means months, matching the captain's 3h 3d 2w 2m scale. Minutes are
// mi or min, never bare m.

var durToken = regexp.MustCompile(`([0-9]+|a|an)?\s*(seconds?|secs?|sec|s|minutes?|mins?|min|mi|hours?|hrs?|hr|h|days?|dys?|dy|d|weeks?|wks?|wk|w|months?|mons?|mon|mo|m|years?|yrs?|yr|y)`)

// parseReminder resolves a snooze expression to an absolute unix time.
// ok is false when the text is not a valid expression. A zero due with ok
// true means "clear the reminder".
func parseReminder(text string, now time.Time) (due int64, ok bool, desc string) {
	s := strings.ToLower(strings.TrimSpace(text))
	s = strings.TrimPrefix(s, "in ")
	switch s {
	case "":
		return 0, false, ""
	case "clear", "none", "off", "x":
		return 0, true, "no reminder"
	case "tomorrow", "tom", "tmr":
		t := now.AddDate(0, 0, 1)
		at := time.Date(t.Year(), t.Month(), t.Day(), 9, 0, 0, 0, now.Location())
		return at.Unix(), true, at.Format("tomorrow 3:04 PM")
	}

	// Compound sequences work: 1d 12h, 2w3d. Stray words fail the parse, so
	// an expression is either fully a duration or it is not one at all.
	matches := durToken.FindAllStringSubmatch(s, -1)
	if matches == nil {
		return 0, false, ""
	}
	if rest := strings.TrimSpace(durToken.ReplaceAllString(s, "")); rest != "" {
		return 0, false, ""
	}

	when := now
	descParts := []string{}
	for _, m := range matches {
		n := 1
		if m[1] != "" && m[1] != "a" && m[1] != "an" {
			n, _ = strconv.Atoi(m[1])
			if n <= 0 {
				return 0, false, ""
			}
		}
		// The unit words all start with their letter, and the alternation
		// only admits these words, so the leading letter is unambiguous once
		// minutes (mi) are matched before months (m).
		switch unit := m[2]; unit[0] {
		case 's':
			when = when.Add(time.Duration(n) * time.Second)
			descParts = append(descParts, plural(n, "second"))
		case 'm':
			if len(unit) > 1 && unit[1] == 'i' {
				when = when.Add(time.Duration(n) * time.Minute)
				descParts = append(descParts, plural(n, "minute"))
			} else {
				when = when.AddDate(0, n, 0)
				descParts = append(descParts, plural(n, "month"))
			}
		case 'h':
			when = when.Add(time.Duration(n) * time.Hour)
			descParts = append(descParts, plural(n, "hour"))
		case 'd':
			when = when.AddDate(0, 0, n)
			descParts = append(descParts, plural(n, "day"))
		case 'w':
			when = when.AddDate(0, 0, 7*n)
			descParts = append(descParts, plural(n, "week"))
		default:
			when = when.AddDate(n, 0, 0)
			descParts = append(descParts, plural(n, "year"))
		}
	}
	return when.Unix(), true, "in " + strings.Join(descParts, " ") + ", " + when.Format("Mon Jan 2 3:04 PM")
}

func plural(n int, unit string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", unit)
	}
	return fmt.Sprintf("%d %ss", n, unit)
}
