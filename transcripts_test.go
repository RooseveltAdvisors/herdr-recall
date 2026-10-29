package main

import (
	"strings"
	"testing"
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
