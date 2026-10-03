package main

import (
	"strings"
	"testing"
	"time"
)

func TestFormShowsEveryChoice(t *testing.T) {
	frame := stripANSI(renderForm(80, 28, []string{"All", "Favorites", "Recent"}, 2, 1, true, "Recent", "last", 20, "", "", "", ""))
	for _, want := range []string{"Edit tab", "Oldest", "Needs me", "Not pinned", "This workspace", "Blocked", "Shows the 20 panes"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("form missing %q:\n%s", want, frame)
		}
	}
	sort, limit, only, where, state := stepFormValue(1, 1, "last", 20, "", "", "")
	if sort != "oldest" || limit != 20 || only != "" || where != "" || state != "" {
		t.Fatalf("sort step = %s %d %s %s %s", sort, limit, only, where, state)
	}
}

func TestCustomFilters(t *testing.T) {
	here := row{PaneID: "a", WorkspaceID: "w1", Fav: true, Status: "blocked"}
	away := row{PaneID: "b", WorkspaceID: "w2", Status: "idle"}
	if !rowAllowed(here, customTab{Only: "favorites", Where: "here", State: "blocked"}, "w1") {
		t.Fatal("matching row was filtered out")
	}
	if rowAllowed(away, customTab{Where: "here"}, "w1") {
		t.Fatal("other workspace was kept")
	}
	if rowAllowed(here, customTab{Only: "unpinned"}, "w1") {
		t.Fatal("pinned row stayed in the unpinned filter")
	}
}

func TestCustomTabsDoNotPinFavorites(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}, TabsInit: true, Tabs: defaultTabs()}
	st.Panes["notes"] = &paneStat{Visits: 2, Last: now - 5000, Label: "notes"}
	st.Favorites = []string{"notes"}
	var panes []paneRec
	panes = append(panes, paneRec{PaneID: "notes"})
	for i := 0; i < 25; i++ {
		id := string(rune('a' + i))
		st.Panes[id] = &paneStat{Visits: int64(i + 1), Last: now - int64(i)*60, Label: id}
		panes = append(panes, paneRec{PaneID: id})
	}
	snap := &snapshot{Panes: panes}
	recent := rowsFor(snap, st, view{Kind: "custom", Idx: 0}, sortDefault)
	if len(recent) != 20 {
		t.Fatalf("recent len = %d", len(recent))
	}
	for _, r := range recent {
		if r.PaneID == "notes" {
			t.Fatal("old favorite was pinned into Recent")
		}
	}
	frame := renderFrame(recent, 0, "", "", false, 90, 16, viewNames(st), 2, "last")
	plain := stripANSI(frame)
	if !strings.Contains(strings.Split(plain, "\n")[0], "Recent") {
		t.Fatalf("tab bar missing:\n%s", plain)
	}
	if strings.Contains(plain, "last 20") || strings.Contains(plain, "to search") {
		t.Fatalf("description or search prompt leaked:\n%s", plain)
	}
	all := rowsFor(snap, st, view{Kind: "all"}, sortDefault)
	if len(all) != 26 {
		t.Fatalf("all len = %d", len(all))
	}
}
