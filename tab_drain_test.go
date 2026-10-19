package main

import (
	"testing"
	"time"
)

// e removes the row from the list being triaged. All view keeps it with a check
// so the captain can always find any thread.
func TestEDrainsTriageViewsAndAllRetains(t *testing.T) {
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

	// Inbox and triage tabs exclude it so the queue drains.
	for _, v := range []view{{Kind: "inbox"}, {Kind: "custom", Idx: 0}} {
		for _, r := range rowsFor(snap, st, v, sortDefault) {
			if r.PaneID == "gone" {
				t.Fatalf("done row still in %s view; did not drain on e", v.Kind)
			}
		}
	}

	// All view keeps it so the captain can always find any thread.
	foundAll := false
	for _, r := range rowsFor(snap, st, view{Kind: "all"}, sortDefault) {
		if r.PaneID == "gone" {
			foundAll = true
			if !r.Done {
				t.Fatal("expected done=true in All view")
			}
			break
		}
	}
	if !foundAll {
		t.Fatal("done row vanished from All view; the captain lost it")
	}
}
