package main

import (
	"fmt"

	"github.com/RooseveltAdvisors/vimline"
	"strings"
	"testing"
	"time"
)

func TestFormShowsEveryChoice(t *testing.T) {
	frame := stripANSI(renderForm(80, 24, []string{"All", "Favorites", "Recent"}, 2, 1, true, vimline.View{Label: "Name", Text: "Recent", Focused: true}, "newest", 20, "", "", ""))
	for _, want := range []string{"Edit tab", "Most recent", "Oldest", "Most used", "Least used", "This workspace"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("form missing %q:\n%s", want, frame)
		}
	}
	for _, gone := range []string{"Descending", "Ascending", "Last used", "Not pinned", "Blocked", "Needs me"} {
		if strings.Contains(frame, gone) {
			t.Fatalf("weak option %q is still offered", gone)
		}
	}
	sort, limit, only, where := stepFormValue(1, 1, "newest", 20, "", "")
	if sort != "oldest" || limit != 20 || only != "" || where != "" {
		t.Fatalf("sort step = %s %d %s %s", sort, limit, only, where)
	}
	if canonicalSort("last", "") != "newest" || canonicalSort("times", "asc") != "least" {
		t.Fatal("old sort values did not map to named sorts")
	}
}

func TestCustomFilters(t *testing.T) {
	here := row{PaneID: "a", WorkspaceID: "w1", Fav: true, Status: "blocked"}
	away := row{PaneID: "b", WorkspaceID: "w2", Status: "idle"}
	if !rowAllowed(here, customTab{Only: "favorites", Where: "here"}, "w1") {
		t.Fatal("matching row was filtered out")
	}
	if rowAllowed(away, customTab{Where: "here"}, "w1") {
		t.Fatal("other workspace was kept")
	}
}

func TestStatusDotSitsLeftOfTheName(t *testing.T) {
	rows := []row{
		{PaneID: "p1", Label: "notes", Status: "working", Last: time.Now().Unix(), Fav: true},
		{PaneID: "p2", Label: "build", Status: "blocked", Last: time.Now().Unix()},
	}
	plain := stripANSI(renderFrame(rows, 0, 0, vimline.View{Label: "/"}, "", 90, 12, []string{"All"}, 0, "last", ""))
	var r1, r2 string
	for _, l := range strings.Split(plain, "\n") {
		if strings.Contains(l, "notes") {
			r1 = l
		}
		if strings.Contains(l, "build") {
			r2 = l
		}
	}
	dot := strings.Index(r1, "●")
	name := strings.Index(r1, "notes")
	word := strings.Index(r1, "working")
	if dot < 0 || name < 0 || word < 0 || !(dot < name && name < word) {
		t.Fatalf("dot, name, word order = %d %d %d\n%s", dot, name, word, r1)
	}
	if strings.Index(r1, "●") != strings.Index(r2, "●") || strings.Index(r1, "working") != strings.Index(r2, "blocked") {
		t.Fatalf("selected row and unselected row columns misaligned:\n%s\n%s", r1, r2)
	}
}

func TestInboxTriageRemindersNotesAndManualBlock(t *testing.T) {
	now := time.Now().Unix()
	st := &store{
		Version:  1,
		TabsInit: true,
		Tabs:     defaultTabs(),
		Panes: map[string]*paneStat{
			"p-work":   {Visits: 3, Last: now - 30, Note: "waiting on db migration"},
			"p-done":   {Visits: 2, Last: now - 60, Done: true, DoneAt: now - 50, DoneState: "working"},
			"p-clear":  {Visits: 2, Last: now - 60, Done: true, DoneAt: now - 50, DoneState: "idle"},
			"p-snooze": {Visits: 4, Last: now - 10, Done: true, DoneAt: now - 10, DoneState: "working", RemindAt: now + 600},
			"p-due":    {Visits: 1, Last: now - 300, Done: true, DoneAt: now - 300, DoneState: "idle", RemindAt: now - 5},
			"p-manual": {Visits: 1, Last: now - 400, Done: true, DoneAt: now - 400, DoneState: "idle", ManualBlocked: true},
		},
	}
	gotOrder := strings.Join(viewNames(st), ",")
	wantOrder := "Favorites,Inbox,Later,Recent,Most used,All"
	if gotOrder != wantOrder {
		t.Fatalf("tab order = %q, want %q", gotOrder, wantOrder)
	}
	snap := &snapshot{
		Tabs: []tabRec{
			{TabID: "t-work", Label: "api-migration"},
			{TabID: "t-done", Label: "auth-fix"},
			{TabID: "t-clear", Label: "docs-clean"},
			{TabID: "t-snooze", Label: "billing-sync"},
			{TabID: "t-due", Label: "qa-followup"},
			{TabID: "t-manual", Label: "vendor-reply"},
		},
		Panes: []paneRec{
			{PaneID: "p-work", TabID: "t-work", AgentStatus: "working"},
			{PaneID: "p-done", TabID: "t-done", AgentStatus: "done"},
			{PaneID: "p-clear", TabID: "t-clear", AgentStatus: "idle"},
			{PaneID: "p-snooze", TabID: "t-snooze", AgentStatus: "working"},
			{PaneID: "p-due", TabID: "t-due", AgentStatus: "idle"},
			{PaneID: "p-manual", TabID: "t-manual", AgentStatus: "idle"},
		},
	}

	inbox := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	gotIDs := make([]string, len(inbox))
	for i, r := range inbox {
		gotIDs[i] = r.PaneID
	}
	// p-clear is Done with unchanged idle state, and p-snooze is snoozed in the future.
	// p-due (due reminder) and p-manual (manual blocked) must sit in bucket 0, then p-done (newly done), then p-work.
	wantIDs := []string{"p-due", "p-manual", "p-done", "p-work"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("inbox rows = %v, want %v", gotIDs, wantIDs)
	}

	// Sticky note is rendered inline and searchable via filterRows; Inbox has no sort pill.
	plain := stripANSI(renderFrame(inbox, 0, 0, vimline.View{Label: "/"}, "", 100, 16, viewNames(st), 1, "both", "", activeSortLabel(st, view{Kind: "inbox"}, sortDefault)))
	if strings.Contains(plain, "[sort:") {
		t.Fatalf("inbox should not show a sort pill:\n%s", plain)
	}
	for _, want := range []string{"qa-followup", "vendor-reply", "waiting on db migration", "e done", "h remind", "n note"} {
		if !strings.Contains(plain, want) {
			t.Fatalf("inbox frame missing %q:\n%s", want, plain)
		}
	}

	// Reviewing the top pane (status changes from done/blocked to idle/working and Last updates)
	// must NOT move the pane off the top position in Inbox.
	snap.Panes[4].AgentStatus = "idle" // p-due was at index 0
	st.Panes["p-due"].RemindAt = 0
	st.Panes["p-work"].Last = now + 100 // another pane gets a newer timestamp
	inboxAfterReview := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	if len(inboxAfterReview) != 4 || inboxAfterReview[0].PaneID != "p-due" {
		t.Fatalf("top pane moved after review: got %v, want p-due at index 0", inboxAfterReview)
	}
	// Pressing e (removeFromInbox) removes the top pane and advances the next one.
	st.Panes["p-due"].Done = true
	st.Panes["p-due"].DoneState = "idle"
	st.removeFromInbox("p-due")
	inboxAfterDone := rowsFor(snap, st, view{Kind: "inbox"}, sortDefault)
	if len(inboxAfterDone) != 3 || inboxAfterDone[0].PaneID != "p-manual" {
		t.Fatalf("inbox after e (done) = %v, want p-manual at index 0", inboxAfterDone)
	}
	byNote := filterRows(inbox, "migration")
	if len(byNote) != 1 || byNote[0].PaneID != "p-work" {
		t.Fatalf("filterRows by sticky note = %+v", byNote)
	}

	// Snoozed pane renders ~ in All tab, and All shows [sort: Most recent].
	all := rowsFor(snap, st, view{Kind: "all"}, sortDefault)
	allPlain := stripANSI(renderFrame(all, 0, 0, vimline.View{Label: "/"}, "", 100, 18, viewNames(st), 5, "both", ""))
	if !strings.Contains(allPlain, "PM") && !strings.Contains(allPlain, "AM") && !strings.Contains(allPlain, "in ") {
		t.Fatalf("expected a 12-hour reminder time in All:\n%s", allPlain)
	}
	if strings.Contains(allPlain, "\u25f7") {
		t.Fatal("cryptic clock mark is still shown")
	}
	if !strings.Contains(allPlain, "[sort: Most recent]") {
		t.Fatalf("expected [sort: Most recent] pill in All:\n%s", allPlain)
	}
}

func TestRowTitleIsTabNameOnly(t *testing.T) {
	snap := &snapshot{
		Panes: []paneRec{{
			PaneID: "p1", TabID: "t1", WorkspaceID: "w1",
			TerminalTitleStrip: "\u03c0 - wiseman", CWD: "/home/jon/git/wiseman",
		}},
		Tabs:       []tabRec{{TabID: "t1", Label: "portal-CI"}},
		Workspaces: []wsRec{{WorkspaceID: "w1", Label: "wiseman"}},
	}
	got := displayLabel(snap, "p1", "\u03c0 - wiseman")
	if got != "portal-CI" {
		t.Fatalf("label = %q", got)
	}
}

func TestCustomTabsDoNotPinFavorites(t *testing.T) {
	now := time.Now().Unix()
	st := &store{Version: 1, Panes: map[string]*paneStat{}, TabsInit: true, Tabs: defaultTabs()}
	st.Panes["notes"] = &paneStat{Visits: 2, Last: now - 5000, Label: "notes"}
	st.Favorites = []string{"notes"}
	var panes []paneRec
	panes = append(panes, paneRec{PaneID: "notes"})
	for i := 0; i < 25; i++ {
		id := string(rune('a' + i))
		st.Panes[id] = &paneStat{Visits: int64(i + 1), Last: now - int64(i)*60, Label: id}
		panes = append(panes, paneRec{PaneID: id})
	}
	snap := &snapshot{Panes: panes}
	recent := rowsFor(snap, st, view{Kind: "custom", Idx: 0}, sortDefault)
	if len(recent) != 26 {
		t.Fatalf("recent len = %d, want 26 (50 limit, 26 panes)", len(recent))
	}
	// The old favorite sits at the bottom by recency, not pinned to the top.
	if len(recent) > 0 && recent[0].PaneID == "notes" {
		t.Fatal("old favorite was pinned to the top of Recent")
	}
	frame := renderFrame(recent, 0, 0, vimline.View{Label: "/"}, "", 90, 16, viewNames(st), 3, "last", "")
	plain := stripANSI(frame)
	if !strings.Contains(strings.Split(plain, "\n")[0], "Recent") {
		t.Fatalf("tab bar missing:\n%s", plain)
	}
	if strings.Contains(plain, "last 20") {
		t.Fatalf("description leaked:\n%s", plain)
	}
	if !strings.Contains(plain, "search panes") || !strings.Contains(plain, "b/w/i/d/a") {
		t.Fatalf("prefix-k search line or footer missing:\n%s", plain)
	}
	all := rowsFor(snap, st, view{Kind: "all"}, sortDefault)
	if len(all) != 26 {
		t.Fatalf("all len = %d", len(all))
	}
	allFrame := renderFrame(all, 20, 0, vimline.View{Label: "/"}, "", 90, 14, viewNames(st), 5, "both", "", activeSortLabel(st, view{Kind: "all"}, sortDefault))
	if strings.Count(allFrame, "\n") >= 14 {
		t.Fatalf("All tab emitted %d newlines for height 14, which scrolls the tab bar off", strings.Count(allFrame, "\n"))
	}
	firstLine := strings.Split(stripANSI(allFrame), "\n")[0]
	for _, tabName := range []string{"Inbox", "All", "Favorites", "Recent", "Most used"} {
		if !strings.Contains(firstLine, tabName) {
			t.Fatalf("All tab top line missing tab %q: %q", tabName, firstLine)
		}
	}
}

func TestDoneReturnsOnlyOnNewAgentActivity(t *testing.T) {
	// done -> idle is the operator viewing the pane, not new work.
	if activityChanged("idle", "done") {
		t.Fatal("viewing a done pane must not return it to Inbox")
	}
	// A fresh completion, a start of work, and a block are all new activity.
	for _, tc := range []struct{ now, base string }{
		{"done", "idle"}, {"working", "done"}, {"blocked", "done"},
		{"working", "idle"}, {"blocked", "working"}, {"idle", "working"},
	} {
		if !activityChanged(tc.now, tc.base) {
			t.Fatalf("expected new activity for %q -> %q", tc.base, tc.now)
		}
	}
	if activityChanged("done", "done") || activityChanged("", "done") {
		t.Fatal("unchanged or unknown state is not new activity")
	}

	// A pane marked done at "done" stays out after being viewed (idle),
	// and returns once the agent starts working again.
	now := time.Now().Unix()
	ps := &paneStat{Done: true, DoneAt: now, DoneState: "done", Last: now - 5}
	r := row{PaneID: "p", Label: "notes", Status: "idle", Done: true, DoneState: "done", Last: ps.Last}
	if inInbox(r, now) {
		t.Fatal("viewed done pane returned to Inbox")
	}
	r.Status = "working"
	if !inInbox(r, now) {
		t.Fatal("agent activity did not return the done pane")
	}
}

func TestSnoozeIfNoReplyBreaksOnActivity(t *testing.T) {
	now := time.Now().Unix()
	snoozed := row{
		PaneID: "p", Label: "build", Status: "working",
		RemindAt: now + 600, SnoozeMode: "", SnoozeState: "idle",
		Done: true, DoneState: "idle", Last: now - 10,
	}
	if !inInbox(snoozed, now) {
		t.Fatal("if no reply snooze did not break on agent activity")
	}
	// Regardless keeps the timer even when the agent moves.
	regardless := snoozed
	regardless.SnoozeMode = "regardless"
	if inInbox(regardless, now) {
		t.Fatal("regardless snooze broke on activity")
	}
	// No activity yet: the snooze still hides the thread.
	quiet := snoozed
	quiet.Status = "idle"
	if inInbox(quiet, now) {
		t.Fatal("quiet snoozed thread should stay hidden")
	}
}

func TestPruneKeepsLivePanes(t *testing.T) {
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	for i := 0; i < 260; i++ {
		id := fmt.Sprintf("p%03d", i)
		st.Panes[id] = &paneStat{Visits: 1, Last: int64(i)}
	}
	live := map[string]bool{"p000": true}
	st.prune(live, 200)
	if _, ok := st.Panes["p000"]; !ok {
		t.Fatal("live pane lost its triage state")
	}
	if len(st.Panes) >= 260 {
		t.Fatalf("closed pane history not pruned: %d rows", len(st.Panes))
	}
}

func TestPruneDoesNotPanicBelowTheCap(t *testing.T) {
	// The common case is far fewer closed panes than the cap. Slicing the
	// overflow without a guard panicked on every prefix+k.
	st := &store{Version: 1, Panes: map[string]*paneStat{}}
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("p%d", i)
		st.Panes[id] = &paneStat{Visits: 1, Last: int64(i)}
	}
	live := map[string]bool{"p0": true}
	st.prune(live, 200)
	if len(st.Panes) != 5 {
		t.Fatalf("rows = %d, want 5 kept", len(st.Panes))
	}
	st.prune(map[string]bool{}, 2)
	if len(st.Panes) != 2 {
		t.Fatalf("rows = %d, want 2 after pruning to cap", len(st.Panes))
	}
}
