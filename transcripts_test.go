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

func TestSessionMatchesPanePath(t *testing.T) {
	path := "/home/jon/.pi/agent/sessions/x/2026_01a0d647-8e54-71ee-8c53-b1c71d49083a.jsonl"
	if !sessionMatchesPane("01a0d647-8e54-71ee-8c53-b1c71d49083a", path) {
		t.Fatal("session id should match the pane session path")
	}
}
