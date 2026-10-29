package main

import "testing"

func TestGBDTScoresTheMatchingLeaf(t *testing.T) {
	hot := 0.8
	cold := 0.1
	m := &gbdtModel{
		Features: []string{"status:blocked"},
		Init:     0.1,
		Trees: []gbNode{{
			Feat:  0,
			Thr:   0.5,
			Left:  &gbNode{Leaf: &cold},
			Right: &gbNode{Leaf: &hot},
		}},
	}
	if got := m.score(map[string]float64{"status:blocked": 1}); got < 0.8 {
		t.Fatalf("blocked score = %v", got)
	}
	if got := m.score(map[string]float64{}); got > 0.3 {
		t.Fatalf("missing feature score = %v", got)
	}
}
