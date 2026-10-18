package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/RooseveltAdvisors/vimline"
)

func TestHelpViewportScrolling(t *testing.T) {
	entries := allHelpEntries()
	if len(entries) < 15 {
		t.Fatalf("expected at least 15 help entries, got %d", len(entries))
	}

	// Test small viewport heights (e.g. 10 lines)
	w, h := 80, 10
	fv := vimline.View{}

	for sel := 0; sel < len(entries); sel++ {
		out := renderHelp(w, h, fv, sel, 0)
		lines := strings.Split(out, "\n")
		if len(lines) > h {
			t.Fatalf("height %d exceeded: got %d lines", h, len(lines))
		}

		// Verify selection line is present in the output
		expectedKey := entries[sel].key
		found := false
		for _, line := range lines {
			if strings.Contains(line, expectedKey) && strings.Contains(line, keySelectBg) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("sel=%d (key=%q) not visible in viewport of height %d:\n%s", sel, expectedKey, h, out)
		}
	}
}

func TestJevViewportScrolling(t *testing.T) {
	// Create 50 dummy Jev items across FIRST, BURY, PROJ
	var items []jevItem
	for i := 0; i < 20; i++ {
		items = append(items, jevItem{feature: fmt.Sprintf("status:test_%d", i), value: 0.5, section: "FIRST"})
	}
	for i := 0; i < 15; i++ {
		items = append(items, jevItem{feature: fmt.Sprintf("age:old_%d", i), value: -0.4, section: "BURY"})
	}
	for i := 0; i < 15; i++ {
		items = append(items, jevItem{feature: fmt.Sprintf("label:proj_%d", i), value: 0.8, section: "PROJ"})
	}

	w, h := 80, 12
	names := []string{"Inbox", "All"}

	for sel := 0; sel < len(items); sel++ {
		out := renderJev(w, h, names, 0, items, sel, 0, 10, nil)
		lines := strings.Split(out, "\n")
		if len(lines) > h {
			t.Fatalf("height %d exceeded: got %d lines", h, len(lines))
		}

		// Verify selected row is present in the output with keySelectBg
		desc := featureDescription(items[sel].feature)
		found := false
		for _, line := range lines {
			if strings.Contains(line, desc) && strings.Contains(line, keySelectBg) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("sel=%d (feature=%q) not visible in viewport of height %d:\n%s", sel, items[sel].feature, h, out)
		}
	}
}

func TestJevSelectableItemsOrderAndPartition(t *testing.T) {
	st := &store{
		Version: 1,
		Panes:   map[string]*paneStat{},
		Jev: &jevState{
			Weights: map[string]float64{
				"status:blocked": 0.9,
				"status:working": -0.6,
				"age:fresh":      0.3,
				"label:portal":   0.8,
				"label:docs":     -0.5,
			},
		},
	}
	items := jevSelectableItems(st)
	if len(items) != 5 {
		t.Fatalf("got %d items, want 5", len(items))
	}
	// FIRST comes first, then BURY, then PROJ (label:)
	if items[0].section != "FIRST" || items[1].section != "FIRST" {
		t.Fatalf("expected first 2 items to be FIRST: %+v", items[:2])
	}
	if items[2].section != "BURY" {
		t.Fatalf("expected item 2 to be BURY: %+v", items[2])
	}
	if items[3].section != "PROJ" || items[4].section != "PROJ" {
		t.Fatalf("expected items 3 and 4 to be PROJ: %+v", items[3:])
	}
}

func TestStableNavigationWithoutItemShifting(t *testing.T) {
	var rows []row
	for i := 0; i < 20; i++ {
		rows = append(rows, row{
			PaneID: fmt.Sprintf("p%d", i),
			Label:  fmt.Sprintf("pane-%02d", i),
			Status: "idle",
		})
	}

	w, h := 80, 14 // visible rows ~ 7
	fv := vimline.View{Label: "/"}

	// Simulate pressing j down to row 10, then k back up to row 7.
	// As we press k, the visible slice [start..start+visible] must remain
	// rock-solid, and lines on screen must NOT shift down.
	scrollOffset := 0
	visible := 7

	// Move down to row 10
	for sel := 0; sel <= 10; sel++ {
		if sel >= scrollOffset+visible {
			scrollOffset = sel - visible + 1
		}
	}
	// At sel 10, scrollOffset should be 10 - 7 + 1 = 4. Slice is rows 4..10.
	if scrollOffset != 4 {
		t.Fatalf("expected scrollOffset=4, got %d", scrollOffset)
	}

	frame10 := renderFrame(rows, 10, scrollOffset, fv, "", w, h, []string{"Inbox"}, 0, "both", "")

	// Now press k: sel becomes 9. Since 9 is within [4..10], scrollOffset must NOT change.
	sel := 9
	if sel < scrollOffset {
		scrollOffset = sel
	}
	if scrollOffset != 4 {
		t.Fatalf("scrollOffset changed to %d on k, expected 4", scrollOffset)
	}
	frame9 := renderFrame(rows, 9, scrollOffset, fv, "", w, h, []string{"Inbox"}, 0, "both", "")

	// Extract lines for rows 4..10 in both frames.
	// The rows on screen must be in the exact same positions.
	lines10 := strings.Split(stripANSI(frame10), "\n")
	lines9 := strings.Split(stripANSI(frame9), "\n")

	// Verify pane-04 is on the exact same line in both frames
	idx10, idx9 := -1, -1
	for i, l := range lines10 {
		if strings.Contains(l, "pane-04") {
			idx10 = i
			break
		}
	}
	for i, l := range lines9 {
		if strings.Contains(l, "pane-04") {
			idx9 = i
			break
		}
	}
	if idx10 != idx9 || idx10 == -1 {
		t.Fatalf("pane-04 shifted position between sel=10 and sel=9: line %d vs line %d", idx10, idx9)
	}
}

func TestInboxRetainsActivePaneAcrossJumps(t *testing.T) {
	st := &store{
		Version:  1,
		Panes:    map[string]*paneStat{"p1": {Last: 100}},
		TabsInit: true,
		Tabs:     defaultTabs(),
	}
	snap := &snapshot{
		FocusedPaneID: "p1",
		Panes: []paneRec{
			{PaneID: "p1", AgentStatus: "working"},
			{PaneID: "p2", AgentStatus: "blocked"},
			{PaneID: "p3", AgentStatus: "done"},
		},
	}
	// p1 is focused. It MUST remain in the inbox list!
	inbox := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	foundP1 := false
	for _, r := range inbox {
		if r.PaneID == "p1" {
			foundP1 = true
			break
		}
	}
	if !foundP1 {
		t.Fatal("focused pane p1 was removed from inbox list")
	}

	// Focusing p2 should keep the exact same inbox items without shifting
	snap.FocusedPaneID = "p2"
	inbox2 := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	if len(inbox2) != len(inbox) {
		t.Fatalf("inbox length changed after focus switch: %d vs %d", len(inbox2), len(inbox))
	}
	for i := range inbox {
		if inbox[i].PaneID != inbox2[i].PaneID {
			t.Fatalf("inbox items shifted after focus switch at index %d: %q vs %q", i, inbox[i].PaneID, inbox2[i].PaneID)
		}
	}
}

func TestHelpEntriesSearchableAndJInFooter(t *testing.T) {
	all := allHelpEntries()
	if len(all) < 25 {
		t.Fatalf("expected at least 25 help entries, got %d", len(all))
	}

	// Verify key shortcuts are searchable
	testQueries := []struct {
		query   string
		wantKey string
	}{
		{"J", "J"},
		{"jev", "J"},
		{"teach", "i"},
		{"enter", "enter"},
		{"ctrl-d", "ctrl-d / ctrl-u"},
		{"snooze", "z"},
		{"note", "n"},
		{"undo", "u"},
		{"filter", "b w i d a"},
	}

	for _, tc := range testQueries {
		matches := filterHelpEntries(all, tc.query)
		found := false
		for _, m := range matches {
			if strings.Contains(m.key, tc.wantKey) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("query %q did not match expected key %q in help entries", tc.query, tc.wantKey)
		}
	}

	// Verify that 'J' is explicitly shown on the page footer in renderFrame
	rows := []row{{PaneID: "p1", Label: "build", Status: "working"}}
	frame := renderFrame(rows, 0, 0, vimline.View{Label: "/"}, "", 100, 16, []string{"Inbox"}, 0, "both", "")
	plain := stripANSI(frame)

	if !strings.Contains(plain, "J jev") {
		t.Fatalf("expected 'J jev' to be shown in the page footer:\n%s", plain)
	}
	if !strings.Contains(plain, "? help") {
		t.Fatalf("expected '? help' to be shown in the page footer:\n%s", plain)
	}
}

func TestRenderFieldWrapsLinesWhenWidthExceeded(t *testing.T) {
	longText := "prioritize stripe billing integration because client is waiting on live api keys for launch"
	fv := vimline.View{
		Label:   "reason",
		Text:    longText,
		Cursor:  len([]rune(longText)), // cursor at end
		Mode:    vimline.ModeInsert,
		Focused: true,
	}

	width := 40
	rendered := renderField(fv, width)
	lines := strings.Split(rendered, "\n")

	if len(lines) < 2 {
		t.Fatalf("expected long text to wrap to multiple lines, got %d lines:\n%s", len(lines), rendered)
	}

	// Verify that each line's visual width does not exceed the allowed width
	for i, l := range lines {
		plain := stripANSI(l)
		colCount := len([]rune(plain))
		if colCount > width {
			t.Fatalf("line %d visual length %d exceeds width %d: %q", i, colCount, width, plain)
		}
	}

	// Verify screen emit splits on newlines and fits within screen
	sc := newScreen(width, 10, 5)
	sc.emit(rendered)
	if len(sc.lines) != len(lines) {
		t.Fatalf("expected screen to contain %d lines, got %d", len(lines), len(sc.lines))
	}
}
