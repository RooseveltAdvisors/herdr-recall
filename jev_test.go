package main

import (
	"os"
	"testing"
	"time"
)

func TestJevDisabledWithoutProxy(t *testing.T) {
	t.Setenv("HERDR_RECALL_JEV", "off")
	if jevEnabled() {
		t.Fatal("jev enabled despite off")
	}
	if jevProxyBin() != "" {
		t.Fatal("proxy resolved despite off")
	}
	// A store with no Jev state is not "on", and ordering must not change.
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	if st.jevOn() {
		t.Fatal("jev on with nil state")
	}
}

func TestJevProxyEnvOverride(t *testing.T) {
	t.Setenv("HERDR_RECALL_JEV", "/usr/local/bin/fake-jev")
	if got := jevProxyBin(); got != "/usr/local/bin/fake-jev" {
		t.Fatalf("proxy = %q", got)
	}
	t.Setenv("HERDR_RECALL_JEV", "on")
	t.Setenv("FM_JEV_PROXY_BIN", "/opt/other/jev.sh")
	if got := jevProxyBin(); got != "/opt/other/jev.sh" {
		t.Fatalf("proxy = %q", got)
	}
}

func TestJevLearningReinforcesChosenFeatures(t *testing.T) {
	os.Unsetenv("HERDR_RECALL_JEV")
	t.Setenv("HERDR_RECALL_JEV", "/bin/true") // any executable path enables
	if !jevEnabled() {
		t.Fatal("expected enabled")
	}
	st := &store{Version: 1, Panes: map[string]*paneStat{}, Jev: &jevState{Weights: map[string]float64{}}}
	now := time.Now().Unix()
	blocked := row{PaneID: "a", Label: "portal billing", Status: "blocked", Note: "waiting on stripe", Last: now}
	working := row{PaneID: "b", Label: "docs cleanup", Status: "working", Last: now - 3600}

	// Learning is a no-op while the module is off.
	st.Jev = nil
	st.jevLearn(blocked, []row{working})
	if st.Jev != nil {
		t.Fatal("learning ran without jev state")
	}

	st.Jev = &jevState{Weights: map[string]float64{}}
	st.jevLearn(blocked, []row{working})
	if st.Jev.Examples != 1 {
		t.Fatalf("examples = %d", st.Jev.Examples)
	}
	if w := st.Jev.Weights["status:blocked"]; w <= 0 {
		t.Fatalf("status:blocked weight = %v, want positive", w)
	}
	if w := st.Jev.Weights["status:working"]; w >= 0 {
		t.Fatalf("skipped status:working weight = %v, want negative", w)
	}
	if w := st.Jev.Weights["label:portal"]; w <= 0 {
		t.Fatalf("label:portal weight = %v, want positive", w)
	}

	// Priority follows the learned weights.
	if st.jevPriority(blocked, now) <= st.jevPriority(working, now) {
		t.Fatal("learned preference did not rank the chosen row first")
	}
}

func TestJevOrdersNewArrivalsToFront(t *testing.T) {
	t.Setenv("HERDR_RECALL_JEV", "/bin/true")
	now := time.Now().Unix()
	st := &store{
		Version: 1, TabsInit: true, Tabs: defaultTabs(),
		Panes: map[string]*paneStat{
			"pinned": {Visits: 5, Last: now - 10, Label: "pinned", InInbox: true},
			"hot":    {Visits: 1, Last: now - 5, Label: "hot", Done: true, DoneAt: now - 5, DoneState: "idle"},
			"cold":   {Visits: 1, Last: now - 5, Label: "cold", Done: true, DoneAt: now - 5, DoneState: "idle"},
		},
		Jev: &jevState{
			Scores:   map[string]float64{"hot": 0.9, "cold": 0.1},
			ScoreKey: map[string]string{},
			Weights:  map[string]float64{},
		},
	}
	// Score keys must match the current content or the score is ignored.
	for id := range st.Jev.Scores {
		for _, r := range collectRows(&snapshot{Panes: []paneRec{{PaneID: id}}}, st) {
			st.Jev.ScoreKey[id] = jevRowKey(r)
		}
	}
	snap := &snapshot{Panes: []paneRec{
		{PaneID: "pinned"}, {PaneID: "hot", AgentStatus: "working"}, {PaneID: "cold", AgentStatus: "working"},
	}}
	st.InboxOrder = []string{"pinned"}
	got := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	if len(got) != 3 {
		t.Fatalf("rows = %d, want 3", len(got))
	}
	// hot outranks cold by Jev score, and both new arrivals land above pinned.
	if got[0].PaneID != "hot" || got[1].PaneID != "cold" || got[2].PaneID != "pinned" {
		t.Fatalf("order = %v %v %v, want hot cold pinned", got[0].PaneID, got[1].PaneID, got[2].PaneID)
	}
}

func TestJevAbsentKeepsAppendOrder(t *testing.T) {
	t.Setenv("HERDR_RECALL_JEV", "off")
	now := time.Now().Unix()
	st := &store{
		Version: 1, TabsInit: true, Tabs: defaultTabs(),
		Panes: map[string]*paneStat{
			"pinned": {Visits: 5, Last: now - 10, Label: "pinned", InInbox: true},
			"later":  {Visits: 1, Last: now - 5, Label: "later", Done: true, DoneAt: now - 5, DoneState: "idle"},
		},
		Jev: &jevState{Scores: map[string]float64{"later": 0.9}, ScoreKey: map[string]string{}, Weights: map[string]float64{}},
	}
	snap := &snapshot{Panes: []paneRec{{PaneID: "pinned"}, {PaneID: "later", AgentStatus: "working"}}}
	st.InboxOrder = []string{"pinned"}
	got := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	if len(got) != 2 || got[0].PaneID != "pinned" || got[1].PaneID != "later" {
		t.Fatalf("order = %v, want pinned then later", got)
	}
}

// The captain's exact crash: applyJev seeded Scores but left Weights nil,
// so the first jevLearn on Inbox wrote to a nil map and panicked.
func TestJevLearnWithNilWeightsDoesNotPanic(t *testing.T) {
	t.Setenv("HERDR_RECALL_JEV", "/bin/true")
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	// Simulate the corrupt state: applyJev ran, Weights was never created.
	st.Jev = &jevState{
		Scores:   map[string]float64{"a": 0.9},
		ScoreKey: map[string]string{"a": "x"},
		// Weights deliberately nil.
	}
	r := row{PaneID: "a", Label: "portal", Status: "working", Last: time.Now().Unix()}
	// This must not panic.
	st.jevLearn(r, nil)
	if len(st.Jev.Weights) == 0 {
		t.Fatal("ensureJev did not create the Weights map")
	}
	if st.Jev.Weights["status:working"] <= 0 {
		t.Fatalf("weight = %v, want positive", st.Jev.Weights["status:working"])
	}
}

func TestJevPreferBumpsFeaturesWithoutProxy(t *testing.T) {
	// No HERDR_RECALL_JEV set: preference is local, must still work.
	os.Unsetenv("HERDR_RECALL_JEV")
	os.Unsetenv("FM_JEV_PROXY_BIN")
	t.Setenv("HERDR_RECALL_JEV", "off")
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	blocked := row{PaneID: "a", Label: "portal billing", Status: "blocked", Last: time.Now().Unix()}

	st.jevPrefer(blocked, true)
	if w := st.Jev.Weights["status:blocked"]; w <= 0 {
		t.Fatalf("status:blocked = %v after prefer, want positive", w)
	}
	if w := st.Jev.Weights["label:portal"]; w <= 0 {
		t.Fatalf("label:portal = %v after prefer, want positive", w)
	}

	idle := row{PaneID: "b", Label: "docs cleanup", Status: "idle", Last: time.Now().Unix()}
	st.jevPrefer(idle, false)
	if w := st.Jev.Weights["status:idle"]; w >= 0 {
		t.Fatalf("status:idle = %v after demote, want negative", w)
	}

	// The prefer's ordering influence: a blocked portal row now outranks an idle docs row.
	if st.jevWeight(blocked, time.Now().Unix()) <= st.jevWeight(idle, time.Now().Unix()) {
		t.Fatal("preference did not rank the preferred row higher")
	}
}
