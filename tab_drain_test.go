package main

import (
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

func TestOpenTabRestoresSavedTab(t *testing.T) {
	st := &store{Version: 1, TabsInit: true, Tabs: defaultTabs()}
	// Live store: active_tab 2 is Recent. Reopen must not force Inbox.
	st.ActiveTab = 2
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
