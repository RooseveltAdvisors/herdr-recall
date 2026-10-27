package main

import (
	"strings"
	"testing"
	"time"
)

// e removes the row from the Inbox queue. Reference tabs (Recent, Most used, All)
// keep every pane with a check mark, so all 50 history slots remain populated.
func TestEDrainsInboxOnly(t *testing.T) {
	now := time.Now().Unix()
	st := &store{
		Version: 1, TabsInit: true, Tabs: defaultTabs(), Panes: map[string]*paneStat{},
	}
	snap := &snapshot{Panes: []paneRec{
		{PaneID: "keep", TabID: "t1", WorkspaceID: "w1", AgentStatus: "working"},
		{PaneID: "gone", TabID: "t2", WorkspaceID: "w1", AgentStatus: "done"},
	}, Tabs: []tabRec{{TabID: "t1", Label: "keep"}, {TabID: "t2", Label: "gone"}}}
	st.Panes["keep"] = &paneStat{Visits: 5, Last: now - 10, Label: "keep"}
	st.Panes["gone"] = &paneStat{Visits: 4, Last: now - 20, Label: "gone"}

	// e on gone, exactly as the handler does.
	ps := st.ensurePane("gone")
	ps.Done = true
	ps.DoneAt = now
	ps.DoneState = baselineState(liveStatusWord(snap, "gone"))
	st.removeFromInbox("gone")

	// Inbox excludes it: row leaves the triage queue.
	inbox := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	for _, r := range inbox {
		if r.PaneID == "gone" {
			t.Fatal("done row still in the Inbox queue")
		}
	}

	// Reference tabs (All, Recent) keep it so history is never cut down to a handful.
	for _, v := range []view{{Kind: "all"}, {Kind: "custom", Idx: 0}} {
		found := false
		for _, r := range rowsFor(snap, st, v, sortDefault) {
			if r.PaneID == "gone" {
				found = true
				if !r.Done {
					t.Fatalf("expected done=true in %s view", v.Kind)
				}
				break
			}
		}
		if !found {
			t.Fatalf("done row vanished from %s reference view; history was lost", v.Kind)
		}
	}
}

func TestTabJumpLetters(t *testing.T) {
	st := &store{Version: 1, TabsInit: true, Tabs: append(defaultTabs(), customTab{Name: "Infra"})}
	got := tabJumpLetters(st)
	name := func(letter string) string {
		idx, ok := got[letter]
		if !ok {
			t.Fatalf("missing letter %s in %v", letter, got)
		}
		return viewName(st, views(st)[idx])
	}
	if name("i") != "Inbox" || name("l") != "Later" || name("r") != "Recent" || name("f") != "Favorites" || name("m") != "Most used" || name("a") != "All" {
		t.Fatalf("fixed tabs = %v", got)
	}
	if name("n") != "Infra" {
		t.Fatalf("custom letter = %s, want Infra", name("n"))
	}
	if _, stolen := got["i"]; !stolen || name("i") != "Inbox" {
		t.Fatal("custom tab stole i")
	}
}

func TestLaterListsOnlyReminders(t *testing.T) {
	now := time.Now().Unix()
	st := &store{
		Version: 2, TabsInit: true, Tabs: defaultTabs(),
		Panes: map[string]*paneStat{
			"soon": {Label: "soon", RemindAt: now + 100, Last: now},
			"due":  {Label: "due", RemindAt: now - 10, Last: now},
			"none": {Label: "none", Last: now},
		},
	}
	snap := &snapshot{Panes: []paneRec{
		{PaneID: "soon", AgentStatus: "idle"},
		{PaneID: "due", AgentStatus: "idle"},
		{PaneID: "none", AgentStatus: "working"},
	}}
	got := rowsFor(snap, st, view{Kind: "later"}, sortDefault)
	if strings.Join(paneIDs(got), ",") != "due,soon" {
		t.Fatalf("later = %v, want due then soon", paneIDs(got))
	}
}

func TestOpenTabRestoresSavedTab(t *testing.T) {
	st := &store{Version: 1, TabsInit: true, Tabs: defaultTabs()}
	// After Later was inserted, Recent is index 3. A saved 3 reopens Recent.
	st.ActiveTab = 3
	got := openTab(st)
	if viewName(st, views(st)[got]) != "Recent" {
		t.Fatalf("reopen = %s, want Recent", viewName(st, views(st)[got]))
	}
	// A fresh store has no saved tab. First open is Inbox.
	fresh := &store{Version: 1, TabsInit: true, Tabs: defaultTabs()}
	got = openTab(fresh)
	if viewName(fresh, views(fresh)[got]) != "Inbox" {
		t.Fatalf("first open = %s, want Inbox", viewName(fresh, views(fresh)[got]))
	}
	// Favorites is index 0, which looks unset until a session saves the flag.
	st.ActiveTab = 0
	st.ActiveTabSet = true
	got = openTab(st)
	if viewName(st, views(st)[got]) != "Favorites" {
		t.Fatalf("favorites reopen = %s, want Favorites", viewName(st, views(st)[got]))
	}
}
