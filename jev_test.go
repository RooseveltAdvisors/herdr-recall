package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/RooseveltAdvisors/vimline"
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
		for _, r := range collectRows(&snapshot{Panes: []paneRec{{PaneID: id, AgentStatus: "working"}}}, st) {
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

func TestCorrIDStaysOneSignal(t *testing.T) {
	text := "this is obviously a agent message. see this corr=portal-bugs? that's a big tell"
	ids := jevIdentifiers(text)
	if len(ids) != 1 || ids[0] != "id:corr=portal-bugs" {
		t.Fatalf("ids = %v", ids)
	}
	st := &store{}
	selected := withoutIDParts([]string{"tok:bugs", "tok:portal", "tok:corr", "agent:pi"}, ids)
	st.jevPreferSelected(append(selected, ids...), false)
	if st.Jev.Weights["tok:bugs"] != 0 || st.Jev.Weights["tok:portal"] != 0 || st.Jev.Weights["tok:corr"] != 0 {
		t.Fatalf("split words were demoted: %+v", st.Jev.Weights)
	}
	if st.Jev.Weights["id:corr=portal-bugs"] >= 0 {
		t.Fatal("the identifier was not demoted")
	}
	now := time.Now()
	idHit := txHit{SessionID: "id", Snippet: "see corr=portal-bugs in the handoff"}
	plain := txHit{SessionID: "plain", Snippet: "the bugs list is ready"}
	if txClassScore(st, idHit, now) >= txClassScore(st, plain, now) {
		t.Fatal("the identifier hit should rank below a plain bugs hit")
	}
}

func TestJevCallIsStoredForTraining(t *testing.T) {
	dir := t.TempDir()
	jevCallLogOverride = filepath.Join(dir, "jev-calls.jsonl")
	t.Cleanup(func() { jevCallLogOverride = "" })
	recordJevCall(row{PaneID: "w1:p1", Label: "portal"}, "jev-latest", map[string]any{"model": "jev-latest"}, `{"answers":{"triage":{"noul":0.42}}}`, 0.42, true)
	got := examplesFromJevLog()
	if len(got) != 1 || got[0].Y != 0.42 {
		t.Fatalf("examples = %+v", got)
	}
	if got[0].Feats["label:portal"] != 1 && got[0].Feats["tok:portal"] != 1 {
		t.Fatalf("features = %+v", got[0].Feats)
	}
}

func TestLocalTriagerLearnsJevAndDemote(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Jev: &jevState{
		Scores:   map[string]float64{},
		ScoreKey: map[string]string{},
		Weights:  map[string]float64{},
	}}
	var rows []row
	for i := 0; i < 10; i++ {
		r := row{PaneID: fmt.Sprintf("hot-%d", i), Label: "portal billing", Status: "blocked", Agent: "pi", CWD: "/tmp/portal"}
		st.Jev.Scores[r.PaneID] = 0.8
		st.Jev.ScoreKey[r.PaneID] = jevRowKey(r)
		rows = append(rows, r)
	}
	for i := 0; i < 10; i++ {
		r := row{PaneID: fmt.Sprintf("cold-%d", i), Label: "notes", Status: "idle", Agent: "claude", CWD: "/tmp/notes"}
		st.Jev.Scores[r.PaneID] = 0.1
		st.Jev.ScoreKey[r.PaneID] = jevRowKey(r)
		rows = append(rows, r)
	}
	if st.fitLocalTriager(rows, now) < 8 {
		t.Fatal("fit did not use the Jev labels")
	}
	hot := st.localTeacherScore(rows[0], now)
	cold := st.localTeacherScore(rows[10], now)
	if hot <= cold {
		t.Fatalf("hot %v should outrank cold %v", hot, cold)
	}
	st.Jev.Teachings = []jevTeaching{{
		Intent:   "demote",
		Pane:     jevPaneContext{PaneID: rows[0].PaneID},
		Selected: []string{"tok:billing", "tok:portal"},
	}}
	before := teachShift(st, rows[0], now)
	if before >= 0 {
		t.Fatal("a demote teach did not lower the target")
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

func TestJevContextualTeaching(t *testing.T) {
	useTempConfig(t)

	now := time.Now()
	st := &store{Version: 1, Panes: map[string]*paneStat{}}

	teaching := jevTeaching{
		ID:          "teach-test-01",
		Timestamp:   now.Unix(),
		TimeRFC3339: now.Format(time.RFC3339),
		Feedback:    "prioritize billing because client is waiting",
		Intent:      "prefer",
		Pane: jevPaneContext{
			PaneID:   "w1:p1",
			Label:    "billing-engine",
			Status:   "blocked",
			Note:     "waiting on stripe keys",
			Visits:   5,
			Last:     now.Unix(),
			JevScore: 0.85,
		},
		InboxRank:  0,
		InboxTotal: 3,
		Queue: []jevQueueContext{
			{PaneID: "w1:p1", Label: "billing-engine", Status: "blocked", Priority: 0.85},
			{PaneID: "w1:p2", Label: "docs", Status: "idle", Priority: 0.2},
		},
		Features: []string{"status:blocked", "label:billing-engine", "note:yes"},
		Session: jevSessionContext{
			ActiveTab: "inbox",
		},
	}

	recordJevTeaching(st, teaching)
	if err := logJevTeaching(teaching); err != nil {
		t.Fatalf("logJevTeaching failed: %v", err)
	}

	if len(st.Jev.Teachings) != 1 {
		t.Fatalf("expected 1 teaching in store, got %d", len(st.Jev.Teachings))
	}
	saved := st.Jev.Teachings[0]
	if saved.Feedback != teaching.Feedback || saved.Pane.Label != "billing-engine" {
		t.Fatalf("saved teaching mismatch: %+v", saved)
	}

	logPath := filepath.Join(configDir(), "jev-teachings.jsonl")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read jev-teachings.jsonl failed: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line in jsonl, got %d", len(lines))
	}
	var readBack jevTeaching
	if err := json.Unmarshal([]byte(lines[0]), &readBack); err != nil {
		t.Fatalf("json unmarshal failed: %v", err)
	}
	if readBack.ID != "teach-test-01" || readBack.Pane.Note != "waiting on stripe keys" {
		t.Fatalf("readBack mismatch: %+v", readBack)
	}
	if len(readBack.Queue) != 2 || readBack.Features[0] != "status:blocked" {
		t.Fatalf("context mismatch: queue=%+v, features=%+v", readBack.Queue, readBack.Features)
	}
}

func TestRenderTeachVisualPreferences(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	r := row{
		PaneID: "w1:p1",
		Label:  "portal-billing",
		Status: "blocked",
		Note:   "waiting on stripe keys",
	}

	fv := vimline.View{Label: "reason", Text: "high priority"}
	feats := teachFeatureChoices(r, st, now)

	// When pref is 0 (Prefer (+)), "Prefer (+)" chip should be highlighted.
	// Signature: field, pref, priChoice, feats, featSel.
	framePrefer := renderTeach(100, 34, []string{"Inbox"}, 0, r, 0, 0, 0, feats, 0, fv, st, now, nil, 0)
	plainPrefer := stripANSI(framePrefer)

	if !strings.Contains(plainPrefer, "Teach Jev: portal-billing (w1:p1)") {
		t.Fatalf("teach header missing in frame:\n%s", plainPrefer)
	}
	if !strings.Contains(plainPrefer, "waiting on stripe keys") {
		t.Fatalf("sticky note missing in frame:\n%s", plainPrefer)
	}
	if !strings.Contains(plainPrefer, "Prefer (+)") || !strings.Contains(plainPrefer, "Great job") || !strings.Contains(plainPrefer, "Neutral") || !strings.Contains(plainPrefer, "Demote (-)") {
		t.Fatalf("preference choices missing in frame:\n%s", plainPrefer)
	}
	if !strings.Contains(plainPrefer, "Priority (P0 highest)") || !strings.Contains(plainPrefer, "P0") {
		t.Fatalf("priority chooser missing in frame:\n%s", plainPrefer)
	}
	if !strings.Contains(plainPrefer, "h/l choose") || !strings.Contains(plainPrefer, "enter save") {
		t.Fatalf("footer missing in frame:\n%s", plainPrefer)
	}
	if !strings.Contains(plainPrefer, "[x] blocked") || !strings.Contains(plainPrefer, "[x] portal-billing") {
		t.Fatalf("feature chips missing in frame:\n%s", plainPrefer)
	}
	if !strings.Contains(plainPrefer, "Features to guide") {
		t.Fatalf("feature field missing in frame:\n%s", plainPrefer)
	}

	// Verify that the Prefer (+) chip is highlighted with keySelectBg in framePrefer
	if !strings.Contains(framePrefer, keySelectBg) {
		t.Fatalf("expected selected chip to have keySelectBg styling")
	}
}

func TestJevFeaturesCarryAgentRepoAndDomain(t *testing.T) {
	now := time.Now().Unix()
	r := row{
		PaneID: "w1:p1",
		Label:  "portal billing",
		Status: "blocked",
		Agent:  "pi",
		CWD:    "/home/jon/git/portal",
		Note:   "waiting on stripe",
		Last:   now,
	}
	got := map[string]bool{}
	for _, f := range jevFeatures(r, now) {
		got[f] = true
	}
	for _, want := range []string{"status:blocked", "agent:pi", "repo:portal", "label:portal", "note:yes"} {
		if !got[want] {
			t.Fatalf("missing %s in %v", want, got)
		}
	}
	for f := range got {
		if strings.HasPrefix(f, "tok:") {
			t.Fatalf("bag-of-words tok leaked: %s", f)
		}
		if strings.HasPrefix(f, "domain:") {
			t.Fatalf("static domain feature leaked: %s", f)
		}
	}
	rank := map[string]bool{}
	for _, f := range jevRankFeatures(jevFeatures(r, now)) {
		rank[f] = true
	}
	if rank["agent:pi"] {
		t.Fatal("common agent:pi must not enter the ranker diet")
	}
	if !rank["status:blocked"] || !rank["repo:portal"] {
		t.Fatalf("structured rank features missing: %v", rank)
	}
}

func TestFilterRareLabels(t *testing.T) {
	got := filterRareLabels([][]string{
		{"status:idle", "label:rare1"},
		{"status:idle", "label:common"},
		{"status:done", "label:common"},
		{"status:done", "label:common"},
	}, 3)
	join := func(fs []string) string { return strings.Join(fs, ",") }
	if strings.Contains(join(got[0]), "label:rare1") {
		t.Fatal("rare label kept")
	}
	if !strings.Contains(join(got[1]), "label:common") {
		t.Fatal("common label dropped")
	}
}

func TestLegacyTokWeightsDoNotMovePriority(t *testing.T) {
	now := time.Now().Unix()
	r := row{PaneID: "w1:p1", Label: "portal billing", Status: "idle", Agent: "pi", CWD: "/tmp/portal", Note: "x", Last: now}
	st := &store{Jev: &jevState{Weights: map[string]float64{
		"tok:portal":  0.8,
		"agent:pi":    0.5,
		"status:idle": 0.1,
	}}}
	w := st.jevWeight(r, now)
	if w > 0.15 || w < 0.05 {
		t.Fatalf("jevWeight = %v, want only status:idle (~0.1)", w)
	}
}

func TestSentenceLabelIsNotAFeature(t *testing.T) {
	if jevLabelFeature("this is just agent routine 30 minutes check") != "" {
		t.Fatal("sentence label became a feature")
	}
	if jevLabelFeature("portal billing") != "label:portal" {
		t.Fatal("short tab labels must still become label features")
	}
}

func TestUnknownWordStillParsesInTeachText(t *testing.T) {
	adj, rules := parseJevFeedback("prefer covenant over rollout", nil)
	if len(rules) != 0 {
		t.Fatalf("rules = %v", rules)
	}
	if adj["tok:covenant"] <= 0 || adj["tok:rollout"] >= 0 {
		t.Fatalf("adjust = %v", adj)
	}
}

func TestAgentAndTokStartUnchecked(t *testing.T) {
	now := time.Now().Unix()
	r := row{PaneID: "a", Label: "alpha", Status: "idle", Agent: "pi", CWD: "/work/alpha", Last: now}
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	choices := teachFeatureChoices(r, st, now)
	on := map[string]bool{}
	for _, c := range choices {
		on[c.key] = c.on
	}
	if on["agent:pi"] {
		t.Fatal("agent:pi started checked")
	}
	if !on["status:idle"] {
		t.Fatal("status should start checked")
	}
}

func TestParseBillingOverDocs(t *testing.T) {
	adj, rules := parseJevFeedback("prioritize billing over docs", nil)
	if len(rules) != 0 {
		t.Fatalf("rules = %v, want none", rules)
	}
	if adj["tok:billing"] <= 0 {
		t.Fatalf("billing = %v, want positive", adj["tok:billing"])
	}
	if adj["tok:docs"] >= 0 {
		t.Fatalf("docs = %v, want negative", adj["tok:docs"])
	}
	if _, ok := adj["label:prioritize"]; ok {
		t.Fatal("polarity word became a feature")
	}
}

func TestParseDemoteIdleUntilAfternoon(t *testing.T) {
	_, rules := parseJevFeedback("demote idle until afternoon", nil)
	if len(rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(rules))
	}
	rule := rules[0]
	if rule.Sign >= 0 {
		t.Fatalf("sign = %v, want negative", rule.Sign)
	}
	if len(rule.When) != 1 || rule.When[0] != "cadence:morning" {
		t.Fatalf("when = %v, want morning only", rule.When)
	}
	if len(rule.Target) != 1 || rule.Target[0] != "status:idle" {
		t.Fatalf("target = %v, want status:idle", rule.Target)
	}
}

func TestTeachCreditAssignmentIgnoresUnchecked(t *testing.T) {
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	st.jevPreferSelected([]string{"status:blocked", "tok:billing"}, true)
	if st.Jev.Weights["status:blocked"] <= 0 || st.Jev.Weights["tok:billing"] <= 0 {
		t.Fatalf("selected weights = %v", st.Jev.Weights)
	}
	if st.Jev.Weights["agent:pi"] != 0 || st.Jev.Weights["cadence:morning"] != 0 {
		t.Fatalf("unchecked features were trained: %v", st.Jev.Weights)
	}
}

func TestRuleBonusOnlyWhenCadenceMatches(t *testing.T) {
	morning := time.Date(2026, 10, 22, 9, 0, 0, 0, time.Local).Unix()
	afternoon := time.Date(2026, 10, 22, 15, 0, 0, 0, time.Local).Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}, Jev: &jevState{Weights: map[string]float64{}}}
	_, rules := parseJevFeedback("demote idle until afternoon", nil)
	st.applyJevDirective(nil, rules)
	idle := row{PaneID: "a", Label: "notes", Status: "idle", Last: morning}
	if st.jevRuleBonus(idle, morning) >= 0 {
		t.Fatalf("morning bonus = %v, want negative", st.jevRuleBonus(idle, morning))
	}
	if st.jevRuleBonus(idle, afternoon) != 0 {
		t.Fatalf("afternoon bonus = %v, want 0", st.jevRuleBonus(idle, afternoon))
	}
	if st.Jev.Weights["status:idle"] != 0 {
		t.Fatal("until-rule also wrote a global weight")
	}
}

func TestArchiveDoesNotReorderInbox(t *testing.T) {
	t.Setenv("HERDR_RECALL_JEV", "/bin/true")
	now := time.Now().Unix()
	st := &store{
		Version: 1, TabsInit: true, Tabs: defaultTabs(),
		Panes: map[string]*paneStat{
			"a": {Visits: 1, Last: now - 30, Label: "docs", InInbox: true},
			"b": {Visits: 1, Last: now - 20, Label: "billing", InInbox: true},
			"c": {Visits: 1, Last: now - 10, Label: "portal", InInbox: true},
		},
		InboxOrder: []string{"a", "b", "c"},
		Jev:        &jevState{Weights: map[string]float64{"status:blocked": 0.9, "label:portal": 0.8}},
	}
	snap := &snapshot{Panes: []paneRec{
		{PaneID: "a", AgentStatus: "idle"},
		{PaneID: "b", AgentStatus: "working"},
		{PaneID: "c", AgentStatus: "blocked"},
	}}
	st.Panes["b"].Done = true
	st.Panes["b"].DoneState = "working"
	st.removeFromInbox("b")
	got := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	if strings.Join(paneIDs(got), ",") != "a,c" {
		t.Fatalf("order = %v, want a,c", paneIDs(got))
	}
}

func TestHumanPriorityPinOrdersNewInboxArrivalWithoutJev(t *testing.T) {
	now := time.Now().Unix()
	st := &store{
		Version: 1, TabsInit: true, Tabs: defaultTabs(),
		Panes: map[string]*paneStat{
			"low":  {Visits: 1, Last: now - 5, Label: "noise", InInbox: true},
			"high": {Visits: 1, Last: now - 1, Label: "portal-mediorbis", InInbox: true, PrioritySet: true, Priority: 0},
		},
		// Empty InboxOrder so both count as new arrivals.
	}
	snap := &snapshot{Panes: []paneRec{
		{PaneID: "low", AgentStatus: "idle"},
		{PaneID: "high", AgentStatus: "blocked"},
	}}
	got := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	if len(got) < 2 || got[0].PaneID != "high" {
		t.Fatalf("P0 pin must lead new arrivals without Jev: %v", paneIDs(got))
	}
}

func TestHumanPriorityBoostRanksP0AboveUnset(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	cold := row{PaneID: "w1:p1", Label: "cold", Status: "idle", Last: now}
	hot := row{PaneID: "w1:p2", Label: "hot", Status: "idle", Last: now, PrioritySet: true, Priority: 0}
	if st.jevPriority(hot, now) <= st.jevPriority(cold, now) {
		t.Fatalf("P0 pin must outrank unset: hot=%+.3f cold=%+.3f", st.jevPriority(hot, now), st.jevPriority(cold, now))
	}
	feats := map[string]bool{}
	for _, f := range jevFeatures(hot, now) {
		feats[f] = true
	}
	if !feats["priority:P0"] {
		t.Fatalf("priority feature missing: %v", feats)
	}
}

func TestReviewIntentShiftsStrongerThanPrefer(t *testing.T) {
	now := time.Now().Unix()
	r := row{PaneID: "w1:p9", Label: "portal-mediorbis", Status: "blocked", Last: now}
	stPrefer := &store{Version: 1, Panes: map[string]*paneStat{}, Jev: &jevState{Teachings: []jevTeaching{{
		Intent: "prefer", Pane: jevPaneContext{PaneID: r.PaneID}, Selected: jevFeatures(r, now),
	}}}}
	stReview := &store{Version: 1, Panes: map[string]*paneStat{}, Jev: &jevState{Teachings: []jevTeaching{{
		Intent: "review", Pane: jevPaneContext{PaneID: r.PaneID}, Selected: jevFeatures(r, now),
	}}}}
	prefer := teachShift(stPrefer, r, now)
	review := teachShift(stReview, r, now)
	if review <= prefer {
		t.Fatalf("review shift must beat prefer: review=%+.3f prefer=%+.3f", review, prefer)
	}
}

func TestJevReviewSelectedBumpsHarderThanPrefer(t *testing.T) {
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	feats := []string{"label:portal-mediorbis", "status:blocked"}
	st.jevPreferSelected(feats, true)
	preferW := st.Jev.Weights["label:portal-mediorbis"]
	st.jevReviewSelected(feats)
	reviewW := st.Jev.Weights["label:portal-mediorbis"]
	if reviewW-preferW < 0.15 {
		t.Fatalf("review bump too weak: prefer=%+.3f after-review=%+.3f", preferW, reviewW)
	}
}

func TestPriorityBadgeRendersOnRow(t *testing.T) {
	rows := []row{{
		PaneID: "w1:p1", Label: "portal-mediorbis", Status: "blocked",
		PrioritySet: true, Priority: 0, Last: time.Now().Unix(),
	}}
	frame := stripANSI(renderFrame(rows, 0, 0, vimline.View{Label: "/"}, "", 100, 14, []string{"Inbox"}, 0, "both", ""))
	if !strings.Contains(frame, "P0") {
		t.Fatalf("P0 badge missing:\n%s", frame)
	}
	if !strings.Contains(frame, "portal-mediorbis") {
		t.Fatalf("label missing:\n%s", frame)
	}
}

func TestRenderPriorityChooser(t *testing.T) {
	r := row{PaneID: "w1:p1", Label: "portal-mediorbis", PrioritySet: true, Priority: 1}
	frame := stripANSI(renderPriority(80, 16, []string{"Inbox"}, 0, r, 2))
	if !strings.Contains(frame, "Priority: portal-mediorbis") {
		t.Fatalf("header missing:\n%s", frame)
	}
	for _, want := range []string{"P0", "P1", "P2", "P3", "P4", "current:"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q in:\n%s", want, frame)
		}
	}
}

func TestRenderTeachGreatJobReviewLabel(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	r := row{PaneID: "w1:p1", Label: "portal-mediorbis", Status: "blocked"}
	fv := vimline.View{Label: "review", Text: "monitoring caught outliers I had forgotten"}
	feats := teachFeatureChoices(r, st, now)
	// pref=1 Great job, field=3 review comment focused
	frame := stripANSI(renderTeach(100, 34, []string{"Inbox"}, 0, r, 3, 1, 0, feats, 0, fv, st, now, nil, 0))
	if !strings.Contains(frame, "Review comment") {
		t.Fatalf("review comment field missing:\n%s", frame)
	}
	if !strings.Contains(frame, "Great job") {
		t.Fatalf("great job pref missing:\n%s", frame)
	}
}

func TestPriorityBadgeMatchesBeadsTuiGrammar(t *testing.T) {
	// beads-tui formatPriority is coloured "P0".."P4", P0 bold red.
	r0 := row{PrioritySet: true, Priority: 0}
	badge := formatPriorityBadge(r0)
	if stripANSI(badge) != "P0" {
		t.Fatalf("badge plain = %q, want P0", stripANSI(badge))
	}
	if !strings.Contains(badge, keyPri0) || !strings.Contains(badge, keyBold) {
		t.Fatalf("P0 must be bold red: %q", badge)
	}
	r4 := row{PrioritySet: true, Priority: 4}
	b4 := formatPriorityBadge(r4)
	if stripANSI(b4) != "P4" || !strings.Contains(b4, keyPri4) {
		t.Fatalf("P4 must be blue: plain=%q raw=%q", stripANSI(b4), b4)
	}
	if nameColor(r0) != keyPri0 || nameColor(row{}) != keyText {
		t.Fatalf("nameColor P0=%q unset=%q", nameColor(r0), nameColor(row{}))
	}
	labels := priorityChoiceLabels()
	if labels[1] != "P0" || labels[5] != "P4" {
		t.Fatalf("choice labels = %v", labels)
	}
}

func TestPriorityPillSitsNextToStatusDot(t *testing.T) {
	// beads-tui order: status glyph, then P0, then the title.
	rows := []row{{
		PaneID: "w1:p1", Label: "portal-mediorbis", Status: "blocked",
		PrioritySet: true, Priority: 0, Last: time.Now().Unix(),
	}}
	frame := renderFrame(rows, -1, 0, vimline.View{Label: "/"}, "", 100, 14, []string{"Inbox"}, 0, "both", "")
	plain := stripANSI(frame)
	// Find the data row (skip tab bar).
	idx := strings.Index(plain, "portal-mediorbis")
	if idx < 0 {
		t.Fatalf("label missing:\n%s", plain)
	}
	// Walk back on the same line for "● P0 portal" or "●P0 portal".
	lineStart := strings.LastIndex(plain[:idx], "\n") + 1
	line := plain[lineStart : idx+len("portal-mediorbis")]
	if !strings.Contains(line, "P0") {
		t.Fatalf("P0 missing on row line %q", line)
	}
	p0 := strings.Index(line, "P0")
	name := strings.Index(line, "portal-mediorbis")
	// Status glyph is before P0; name is after P0.
	if p0 < 0 || name < p0 {
		t.Fatalf("want status…P0…name order, got %q", line)
	}
	// Colored frame must paint the name with P0 red.
	if !strings.Contains(frame, keyPri0) {
		t.Fatalf("P0 red missing from colored frame")
	}
}

func TestLoadPaneHistoryMergesTeachAndScore(t *testing.T) {
	useTempConfig(t)
	now := time.Now()
	st := &store{Version: 1, Panes: map[string]*paneStat{}, Jev: &jevState{
		Teachings: []jevTeaching{{
			ID: "t1", Timestamp: now.Unix(), TimeRFC3339: now.Format(time.RFC3339),
			Intent: "review", Feedback: "monitoring caught outliers",
			Pane: jevPaneContext{PaneID: "w1:pM", Label: "portal-mediorbis", JevPriority: 0.9},
			Selected: []string{"label:portal-mediorbis"}, InboxRank: 0, InboxTotal: 4,
		}},
	}}
	// Disk teaching for same pane
	_ = logJevTeaching(jevTeaching{
		ID: "t2", Timestamp: now.Add(-time.Hour).Unix(), TimeRFC3339: now.Add(-time.Hour).Format(time.RFC3339),
		Intent: "prefer", Feedback: "care about this",
		Pane: jevPaneContext{PaneID: "w1:pM", Label: "portal-mediorbis"},
		Selected: []string{"status:blocked"},
	})
	// ML score trace
	jevCallLogOverride = filepath.Join(configDir(), "jev-calls.jsonl")
	t.Cleanup(func() { jevCallLogOverride = "" })
	recordJevCall(row{PaneID: "w1:pM", Label: "portal-mediorbis", Status: "blocked"}, "test-model", map[string]any{"q": 1}, `{"ok":true}`, 0.77, true)

	got := loadPaneHistory(st, "w1:pM", 20)
	if len(got) < 3 {
		t.Fatalf("want >=3 history items, got %d: %+v", len(got), got)
	}
	kinds := map[string]bool{}
	for _, it := range got {
		kinds[it.Kind] = true
	}
	for _, want := range []string{"review", "prefer", "score"} {
		if !kinds[want] {
			t.Fatalf("missing kind %s in %+v", want, got)
		}
	}
	// Newest first: review (now) before prefer (hour ago)
	if got[0].Kind != "review" && got[0].Kind != "score" {
		t.Fatalf("expected recent first, got %s", got[0].Kind)
	}
}

func TestRenderTeachShowsHistoryFeed(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	r := row{PaneID: "w1:pM", Label: "portal-mediorbis", Status: "blocked"}
	hist := []paneHistoryItem{
		{At: time.Unix(now, 0), Kind: "review", Title: "great job ★", Body: "monitoring caught outliers", Rank: 0, Total: 5, Score: 0.9},
		{At: time.Unix(now-60, 0), Kind: "score", Title: "score jev +0.77 (ok)", Body: "features: status:blocked", Score: 0.77},
		{At: time.Unix(now-120, 0), Kind: "priority", Title: "set P0", Body: ""},
	}
	fv := vimline.View{Label: "reason"}
	frame := stripANSI(renderTeach(110, 40, []string{"Inbox"}, 0, r, 4, 0, 0, nil, 0, fv, st, now, hist, 0))
	if !strings.Contains(frame, "History (3)") {
		t.Fatalf("history header missing:\n%s", frame)
	}
	for _, want := range []string{"great job", "score jev", "set P0", "monitoring caught outliers"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("missing %q in:\n%s", want, frame)
		}
	}
	// Unset priority shows a plain dash in teach meta, not a diamond.
	if !strings.Contains(frame, "-") {
		t.Fatalf("unset priority marker missing:\n%s", frame)
	}
}

func TestApplyPriorityChoice(t *testing.T) {
	ps := &paneStat{}
	applyPriorityChoice(ps, 1) // P0
	if !ps.PrioritySet || ps.Priority != 0 {
		t.Fatalf("P0 set failed: %+v", ps)
	}
	applyPriorityChoice(ps, 0) // clear
	if ps.PrioritySet || ps.Priority != 0 {
		t.Fatalf("clear failed: %+v", ps)
	}
	applyPriorityChoice(ps, 5) // P4
	if !ps.PrioritySet || ps.Priority != 4 {
		t.Fatalf("P4 set failed: %+v", ps)
	}
}

func paneIDs(rows []row) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.PaneID
	}
	return out
}
