package main

import (
	"strings"
	"testing"
	"time"

	"github.com/RooseveltAdvisors/vimline"
)

// The captain's report: e must drain the list being looked at, on every tab.
func TestEDrainsEveryTabAndURestores(t *testing.T) {
	now := time.Now().Unix()
	st := &store{
		Version: 1, TabsInit: true, Tabs: defaultTabs(),
		Panes: map[string]*paneStat{
			"keep":  {Visits: 5, Last: now - 10, Label: "keep"},
			"gone":  {Visits: 4, Last: now - 20, Label: "gone"},
			"snooz": {Visits: 3, Last: now - 30, Label: "snooz", RemindAt: now + 600, Done: true, DoneState: "idle", SnoozeState: "idle"},
		},
	}
	snap := &snapshot{Panes: []paneRec{
		{PaneID: "keep", TabID: "t1", WorkspaceID: "w1", AgentStatus: "working"},
		{PaneID: "gone", TabID: "t2", WorkspaceID: "w1", AgentStatus: "done"},
		{PaneID: "snooz", TabID: "t3", WorkspaceID: "w1", AgentStatus: "idle"},
	}, Tabs: []tabRec{{TabID: "t1", Label: "keep"}, {TabID: "t2", Label: "gone"}, {TabID: "t3", Label: "snooz"}}}

	// Mark gone done, exactly as the e handler does.
	ps := st.ensurePane("gone")
	ps.Done = true
	ps.DoneAt = now
	ps.DoneState = baselineState(liveStatusWord(snap, "gone"))
	st.removeFromInbox("gone")

	for _, v := range []view{{Kind: "all"}, {Kind: "favorites"}, {Kind: "custom", Idx: 0}} {
		got := rowsFor(snap, st, v, sortDefault)
		for _, r := range got {
			if r.PaneID == "gone" {
				t.Fatalf("done row still listed in %s view", v.Kind)
			}
		}
	}
	// The snoozed row stays with its badge.
	all := rowsFor(snap, st, view{Kind: "all"}, sortDefault)
	found := false
	for _, r := range all {
		if r.PaneID == "snooz" {
			found = true
		}
	}
	if !found {
		t.Fatal("snoozed row hidden from All; it must keep its ~ badge")
	}

	// The row the captain sees after e: gone from the frame entirely.
	frame := stripANSI(renderFrame(rowsFor(snap, st, view{Kind: "all"}, sortDefault), 0, vimline.View{Label: "/"}, "", 90, 12, viewNames(st), 4, "newest", ""))
	if strings.Contains(frame, "gone") {
		t.Fatal("done row still rendered")
	}
	if !strings.Contains(frame, "snooz") {
		t.Fatal("snoozed row missing from the frame")
	}

	// u restores the row to the lists.
	ps.Done = false
	ps.DoneState = ""
	st.addToInbox("gone")
	all = rowsFor(snap, st, view{Kind: "all"}, sortDefault)
	found = false
	for _, r := range all {
		if r.PaneID == "gone" {
			found = true
		}
	}
	if !found {
		t.Fatal("undo did not return the row to the list")
	}

}
