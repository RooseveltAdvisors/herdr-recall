package main

import (
	"testing"
	"time"
)

// The captain's report: Recent showed 10 rows with the limit set to 50,
// because done rows consumed limit slots before the done filter ran.
func TestDoneRowsDoNotConsumeLimitSlots(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, TabsInit: true, Panes: map[string]*paneStat{}, Tabs: []customTab{
		{Name: "Recent", Sort: "last", Limit: 10, Fixed: true},
	}}
	var panes []paneRec
	for i := 0; i < 20; i++ {
		id := string(rune('a' + i))
		done := i < 10 // 10 done, 10 live
		st.Panes[id] = &paneStat{Visits: int64(20 - i), Last: now - int64(i), Label: id, Done: done, DoneState: "working"}
		panes = append(panes, paneRec{PaneID: id, TabID: "t1", WorkspaceID: "w1", AgentStatus: "working"})
	}
	snap := &snapshot{Panes: panes, Tabs: []tabRec{{TabID: "t1", Label: "one"}}}
	got := rowsFor(snap, st, view{Kind: "custom", Idx: 0}, sortDefault)
	if len(got) != 10 {
		t.Fatalf("rows = %d, want all 10 live rows (limit 10 must not be eaten by done rows)", len(got))
	}
	for _, r := range got {
		if r.Done {
			t.Fatalf("done row %q listed", r.Label)
		}
	}
}

// Enter on the top Inbox row was a no-op because the top row was the pane
// the captain was sitting in, bumped to most-recent by opening the picker.
func TestInboxExcludesThePaneYouAreIn(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, TabsInit: true, Tabs: defaultTabs(), Panes: map[string]*paneStat{
		"mine":  {Visits: 9, Last: now, Label: "mine"},
		"other": {Visits: 2, Last: now - 50, Label: "other"},
	}}
	snap := &snapshot{
		FocusedPaneID: "mine",
		Panes:         []paneRec{{PaneID: "mine", TabID: "t1", AgentStatus: "idle"}, {PaneID: "other", TabID: "t2", AgentStatus: "blocked"}},
	}
	got := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	for _, r := range got {
		if r.PaneID == "mine" {
			t.Fatal("the pane the captain is in tops the triage list; Enter on it is a no-op")
		}
	}
	if len(got) == 0 {
		t.Fatal("the other pane should still be triaged")
	}
}
