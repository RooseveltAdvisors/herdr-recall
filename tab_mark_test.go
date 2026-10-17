package main

import (
	"github.com/RooseveltAdvisors/vimline"
	"strings"
	"testing"
	"time"
)

// The captain's report: pressing e on a non-Inbox tab showed no change.
// The triage check must be visible on the row itself.
func TestDoneRowShowsTheTriageCheck(t *testing.T) {
	now := time.Now().Unix()
	rows := []row{
		{PaneID: "a", Label: "alpha", Status: "working", Last: now, Done: true},
		{PaneID: "b", Label: "beta", Status: "working", Last: now},
	}
	frame := stripANSI(renderFrame(rows, 0, vimline.View{Label: "/"}, "", 90, 12, []string{"Recent"}, 0, "newest", ""))
	var doneLine, liveLine string
	for _, l := range strings.Split(frame, "\n") {
		if strings.Contains(l, "alpha") {
			doneLine = l
		}
		if strings.Contains(l, "beta") {
			liveLine = l
		}
	}
	if !strings.Contains(doneLine, "✓ done") {
		t.Fatalf("done row lacks the check: %q", doneLine)
	}
	if strings.Contains(liveLine, "✓") {
		t.Fatalf("live row shows a check: %q", liveLine)
	}
	if !strings.Contains(liveLine, "working") {
		t.Fatalf("live row lost the agent word: %q", liveLine)
	}
}
