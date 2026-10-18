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
		out := renderHelp(w, h, fv, sel)
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
		out := renderJev(w, h, names, 0, items, sel, 10, nil)
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
