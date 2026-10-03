package main

import (
	"strings"
	"testing"
	"time"
)

func TestFavoritesStayPinnedAndRecentCapsAt20(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}, Favorites: []string{"notes"}}
	st.Panes["notes"] = &paneStat{Visits: 2, Last: now - 100, Label: "notes"}
	for i := 0; i < 25; i++ {
		id := string(rune('a'+i))
		st.Panes[id] = &paneStat{Visits: int64(i + 1), Last: now - int64(i)*60, Label: id}
	}
	snap := &snapshot{}
	rows := tabRows(snap, st, tabRecent, sortDefault)
	if len(rows) == 0 || rows[0].PaneID != "notes" || rows[0].Section != "FAVORITES" {
		t.Fatalf("favorite not pinned at top: %+v", rows[:min(3, len(rows))])
	}
	recent := 0
	for _, r := range rows {
		if r.Section == "RECENTLY USED" {
			recent++
		}
	}
	if recent != recentMax {
		t.Fatalf("recent section = %d, want %d", recent, recentMax)
	}
	frame := renderFrame(rows, 0, "", "", false, 90, 24, tabRecent, sortLabel(tabRecent, sortDefault))
	plain := stripANSI(frame)
	for _, want := range []string{"FAVORITES", "MOST USED", "RECENTLY USED", "last used"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("frame missing %q", want)
		}
	}
	if strings.Contains(plain, "v\n") || strings.Contains(plain, "41v") {
		t.Fatalf("bare v metric still present")
	}
	if !strings.Contains(plain, "times") && !strings.Contains(plain, "ago") {
		t.Fatalf("expected times or ago metric, got:\n%s", plain)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
