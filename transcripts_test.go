package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseCcriderSearch(t *testing.T) {
	text := `Found 1 session(s) with 1 match(es) for: billing

=== Session 1 ===
portal billing
ID: 01a0d647-8e54-71ee-8c53-b1c71d49083a
/home/jon/git/wiseman | 12 msgs | 2h ago | 1 matches

  Match 1:
  waiting on stripe keys
`
	got := parseCcriderSearch(text)
	if len(got) != 1 {
		t.Fatalf("hits = %d", len(got))
	}
	if got[0].SessionID != "01a0d647-8e54-71ee-8c53-b1c71d49083a" {
		t.Fatalf("id = %q", got[0].SessionID)
	}
	if !strings.Contains(got[0].Snippet, "stripe") {
		t.Fatalf("snippet = %q", got[0].Snippet)
	}
	if got[0].CWD != "/home/jon/git/wiseman" {
		t.Fatalf("cwd = %q", got[0].CWD)
	}
}

func TestTranscriptsTabFollowsCcrider(t *testing.T) {
	st := &store{Version: 2, TabsInit: true, Tabs: defaultTabs()}
	t.Setenv("HERDR_RECALL_TRANSCRIPTS", "0")
	for _, name := range viewNames(st) {
		if name == "Transcripts" {
			t.Fatal("Transcripts tab shown without ccrider")
		}
	}
	t.Setenv("HERDR_RECALL_TRANSCRIPTS", "1")
	found := false
	for _, name := range viewNames(st) {
		if name == "Transcripts" {
			found = true
		}
	}
	if !found {
		t.Fatal("Transcripts tab hidden when the dependency is enabled")
	}
}

func TestRelevancePutsRecentStrongHitFirst(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	hits := []txHit{
		{Snippet: "old weak", When: "2026-01-01 00:00:00.000 +0000 UTC", Rank: -1},
		{Snippet: "recent strong", When: "2026-10-28 00:00:00.000 +0000 UTC", Rank: -8},
		{Snippet: "recent weak", When: "2026-10-28 00:00:00.000 +0000 UTC", Rank: -1},
	}
	got := sortTxHits(hits, "relevance", now, nil)
	if got[0].Snippet != "recent strong" {
		t.Fatalf("first = %q", got[0].Snippet)
	}
	newest := sortTxHits(hits, "newest", now, nil)
	if newest[0].When < newest[1].When {
		t.Fatal("newest did not put the later time first")
	}
}

func TestTxTitleShowsTabPaneAndAgent(t *testing.T) {
	hit := txHit{CWD: "/opt/ra/firstmate/projects/portal", Provider: "pi", When: "2026-10-12 04:45:24.873 +0000 UTC", Snippet: "the portal chart"}
	live := txLive{Tab: "billing", Pane: "w1:pSZ", Agent: "pi"}
	title := txTitle(hit, live)
	if !strings.Contains(title, "billing") || !strings.Contains(title, "w1:pSZ") || !strings.Contains(title, "Pi") {
		t.Fatalf("title = %q", title)
	}
	if strings.Contains(title, "+0000") {
		t.Fatal("raw timestamp leaked into the title")
	}
	plain := txTitle(hit, txLive{})
	if !strings.Contains(plain, "portal") || !strings.Contains(plain, "Pi") {
		t.Fatalf("plain = %q", plain)
	}
	marked := highlightWords("fix the portal chart", "portal", false)
	if !strings.Contains(marked, keyAccentFg) || !strings.Contains(marked, "portal") {
		t.Fatalf("highlight = %q", marked)
	}
	win := matchWindow(strings.Repeat("x", 400)+" portal billing", "portal", 40)
	if !strings.Contains(win, "portal") {
		t.Fatalf("window = %q", win)
	}
}

func TestRankUpKeepsAHandoffInTyped(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	handoff := txHit{SessionID: "handoff", Snippet: "FIRSTMATE_OP: v1 watcher portal", Provider: "pi", CWD: "/tmp/portal"}
	typed := txHit{SessionID: "typed", Snippet: "what path is the portal on", Provider: "pi", CWD: "/tmp/portal", Rank: -2, When: "2026-10-28 00:00:00.000 +0000 UTC"}
	st := &store{}
	if txClassScore(st, handoff, now) >= 0 {
		t.Fatal("a handoff starts inside Typed")
	}
	st.rankTranscript(handoff, true)
	if txClassScore(st, handoff, now) < 0 {
		t.Fatal("one rank-up did not bring the handoff into Typed")
	}
	st.rankTranscript(typed, false)
	kept := keepTyped(st, []txHit{handoff, typed}, now)
	if len(kept) != 1 || kept[0].SessionID != "handoff" {
		t.Fatalf("kept = %+v", kept)
	}
	ordered := sortTxHits([]txHit{typed, handoff}, "relevance", now, st)
	if ordered[0].SessionID != "handoff" {
		t.Fatal("rank-up did not move the hit to the top")
	}
}

func TestTranscriptFooterShowsRankKeys(t *testing.T) {
	move, rank := txKeyLines()
	if !strings.Contains(move, "enter") || !strings.Contains(move, "fork") {
		t.Fatalf("move = %q", move)
	}
	for _, key := range []string{"1", "+", "-", "i", "s"} {
		if !strings.Contains(rank, key) {
			t.Fatalf("rank footer missing %s: %q", key, rank)
		}
	}
}

func TestIndexOpenIsNotAFileURI(t *testing.T) {
	if strings.Contains(txWorkerScript, "file:{index}?mode=ro") {
		t.Fatal("a percent in the directory name must not be treated as a URI escape")
	}
	t.Setenv("HERDR_RECALL_INDEX", "/tmp/fixture.db")
	if transcriptIndexPath() != "/tmp/fixture.db" {
		t.Fatal(transcriptIndexPath())
	}
}

func TestIndexedSearchKeepsEveryHit(t *testing.T) {
	hits := make([]txHit, 50)
	for i := range hits {
		hits[i] = txHit{SessionID: "s", Snippet: "portal note"}
	}
	got := finishTxHits(nil, hits, "typed", "relevance", time.Now(), true)
	if len(got) != 50 {
		t.Fatalf("kept %d, want every indexed hit", len(got))
	}
}

func TestFilterHitsNarrowsWithoutAQuery(t *testing.T) {
	hits := []txHit{
		{Snippet: "waiting on stripe keys"},
		{Snippet: "portal docs only"},
	}
	got := filterHits(hits, "stripe keys")
	if len(got) != 1 || !strings.Contains(got[0].Snippet, "stripe") {
		t.Fatalf("filtered = %+v", got)
	}
}

func TestTranscriptRoleSender(t *testing.T) {
	if transcriptSender("typed") != "human" || transcriptSender("any-user") != "human" {
		t.Fatal("typed and any-user search human messages")
	}
	if transcriptSender("replies") != "assistant" {
		t.Fatal("replies search assistant messages")
	}
	if transcriptSender("everything") != "" {
		t.Fatal("everything must not filter the speaker")
	}
	if nextTxRole("typed") != "any-user" || nextTxRole("any-user") != "replies" || nextTxRole("replies") != "everything" || nextTxRole("everything") != "typed" {
		t.Fatal("role cycle")
	}
}

func TestTypedOnlyDropsAgentHandoff(t *testing.T) {
	if _, ok := typedOnly("\u2063FIRSTMATE_OP: v1 watcher: wake"); ok {
		t.Fatal("firstmate handoff counted as typed")
	}
	if _, ok := typedOnly("BEADS NOTIFY (comment). close the bead"); ok {
		t.Fatal("beads notify counted as typed")
	}
	got, ok := typedOnly("PREVIOUS AI RESPONSE (what the user is reacting to):\nagent plan\n\nCURRENT USER MESSAGE:\nyes. go build it")
	if !ok || got != "yes. go build it" {
		t.Fatalf("extracted = %q ok=%v", got, ok)
	}
	got, ok = typedOnly("what's the difference between owner and assignee?")
	if !ok || !strings.Contains(got, "owner") {
		t.Fatal("plain typed message was dropped")
	}
}

func TestSessionMatchesPanePath(t *testing.T) {
	path := "/home/jon/.pi/agent/sessions/x/2026_01a0d647-8e54-71ee-8c53-b1c71d49083a.jsonl"
	if !sessionMatchesPane("01a0d647-8e54-71ee-8c53-b1c71d49083a", path) {
		t.Fatal("session id should match the pane session path")
	}
}
