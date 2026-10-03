package main

import (
	"strings"
	"testing"
	"time"
)

func TestTabsStaySeparateAndRecentCapsAt20(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}, Favorites: []string{"notes"}}
	st.Panes["notes"] = &paneStat{Visits: 2, Last: now - 5000, Label: "notes"}
	for i := 0; i < 25; i++ {
		id := string(rune('a' + i))
		st.Panes[id] = &paneStat{Visits: int64(i + 1), Last: now - int64(i)*60, Label: id}
	}
	snap := &snapshot{}

	recent := tabRows(snap, st, tabRecent, sortDefault)
	if len(recent) != recentMax {
		t.Fatalf("recent len = %d, want %d", len(recent), recentMax)
	}
	for _, r := range recent {
		if r.PaneID == "notes" {
			t.Fatal("old favorite must not be pinned into the recent tab")
		}
	}

	favs := tabRows(snap, st, tabFavorites, sortDefault)
	if len(favs) != 1 || favs[0].PaneID != "notes" {
		t.Fatalf("favorites tab = %+v", favs)
	}

	all := tabRows(snap, st, tabAll, sortDefault)
	if len(all) != 26 {
		t.Fatalf("all len = %d, want 26", len(all))
	}

	frame := renderFrame(recent, 0, "", "", false, 90, 24, tabRecent, sortDefault)
	plain := stripANSI(frame)
	for _, want := range []string{"Recent", "Most used", "Favorites", "All", "last 20 you opened"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("frame missing %q\n%s", want, plain)
		}
	}
	if strings.Contains(plain, "FAVORITES") {
		t.Fatalf("pinned favorites section still in the recent frame:\n%s", plain)
	}
}
