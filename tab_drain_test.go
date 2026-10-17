package main

import (
	"testing"
	"time"
)

// e removes the row from the Inbox. Reference tabs keep it with a check,
// so the captain can always find a thread they already triaged.
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

	// Inbox excludes it.
	inbox := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	for _, r := range inbox {
		if r.PaneID == "gone" {
			t.Fatal("done row still in the Inbox")
		}
	}
	// Reference tabs keep it.
	for _, v := range []view{{Kind: "all"}, {Kind: "custom", Idx: 0}} {
		found := false
		for _, r := range rowsFor(snap, st, v, sortDefault) {
			if r.PaneID == "gone" {
				found = true
			}
		}
		if !found {
			t.Fatalf("done row vanished from %s view; the captain lost it", v.Kind)
		}
	}
}
