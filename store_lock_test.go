package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/RooseveltAdvisors/vimline"
)

func mustLoad(t *testing.T) *store {
	t.Helper()
	s, err := loadStore()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// useTempConfig isolates recall state in a temp dir and resets the process
// state, so tests never touch the live plugin config directory. The reset
// runs again at cleanup, so no later test inherits this test's memoized
// config directory after its temp dir is removed.
func useTempConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	resetStoreState()
	t.Cleanup(resetStoreState)
}

func resetStoreState() {
	configDirOnce = sync.Once{}
	proven.Store([]byte(nil))
	loadFailed.Store(false)
	saveTaint.Store(false)
	stateVanished.Store(false)
}

// writeStoreDirect simulates a write from another process: it replaces the
// state file without updating this process's stored fingerprint.
func writeStoreDirect(t *testing.T, s *store) {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// readStoreDirect simulates a read by another process: it parses the state
// file without touching this process's proven bytes.
func readStoreDirect(t *testing.T) *store {
	t.Helper()
	b, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatal(err)
	}
	s, err := parseStore(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestRefreshStoreKeepsStoreWhenNoExternalWrite(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := mustLoad(t)
	if fresh := refreshStore(held); fresh != held {
		t.Fatal("refresh with no external write must keep the in-memory store")
	}
}

func TestRefreshStoreAdoptsExternalWrites(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := mustLoad(t) // the picker's long-lived in-memory store

	ext := mustLoad(t)
	ext.Panes["w1:p1"].Visits++
	ext.Panes["w2:p9"] = &paneStat{Visits: 3, Last: 123, Label: "hook"}
	writeStoreDirect(t, ext)

	fresh := refreshStore(held)
	if fresh == held {
		t.Fatal("refresh must adopt the external write")
	}
	if got := fresh.Panes["w1:p1"].Visits; got != 2 {
		t.Fatalf("external visit increment lost: visits=%d", got)
	}
	if got := fresh.Panes["w2:p9"].Visits; got != 3 {
		t.Fatalf("pane added by external writer missing: visits=%d", got)
	}
}

// TestStalePickerSaveCannotClobberHookWrite is the regression test for the
// lost-update race: a picker session holding a store loaded before a hook ran
// must resync before mutating, so the hook's visits and prune survive the
// picker's save.
func TestStalePickerSaveCannotClobberHookWrite(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	for i := 0; i < 250; i++ {
		seed.Panes["dead:"+string(rune(i))] = &paneStat{Label: "dead"}
	}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
	pickerStore := mustLoad(t) // picker opens

	// A hook fires while the picker is open: records a visit and prunes.
	live := map[string]bool{"w1:p23": true}
	hook := mustLoad(t)
	hp := hook.ensurePane("w1:p23")
	hp.Visits++
	hp.Last = time.Now().Unix()
	hook.prune(live, 200)
	writeStoreDirect(t, hook)

	// The picker mutates through the resync path, like a keypress does.
	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	fresh := refreshStore(pickerStore)
	if fresh == pickerStore {
		t.Fatal("resync must adopt the hook write before mutating")
	}
	fresh.ensurePane("w1:p7S").Done = true
	if err := saveStore(fresh); err != nil {
		t.Fatal(err)
	}
	lk.release()

	final := mustLoad(t)
	if !final.Panes["w1:p7S"].Done {
		t.Fatal("picker mutation lost")
	}
	if got := final.Panes["w1:p23"].Visits; got != 1 {
		t.Fatalf("hook visit clobbered: visits=%d", got)
	}
	dead := 0
	for id := range final.Panes {
		if id[:5] == "dead:" {
			dead++
		}
	}
	if dead > 200 {
		t.Fatalf("prune defeated: %d dead panes resurrected", dead)
	}
}

// TestRefreshStoreAdoptsWriteWithSameMtime pins content-based adoption: an
// external write that reuses the previous modtime must still be picked up.
func TestRefreshStoreAdoptsWriteWithSameMtime(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := mustLoad(t)
	fi, err := os.Stat(statePath())
	if err != nil {
		t.Fatal(err)
	}
	same := fi.ModTime()

	ext := mustLoad(t)
	ext.Panes["w1:p1"].Visits++
	writeStoreDirect(t, ext)
	if err := os.Chtimes(statePath(), same, same); err != nil {
		t.Fatal(err)
	}

	fresh := refreshStore(held)
	if fresh == held {
		t.Fatal("refresh must adopt an external write even when the modtime is unchanged")
	}
	if got := fresh.Panes["w1:p1"].Visits; got != 2 {
		t.Fatalf("external visit increment lost: visits=%d", got)
	}
}

// TestRefreshStoreKeepsStoreWhenStateUnreadable pins the empty-on-error
// regression: when the state file cannot be read mid-session, the in-memory
// store survives and no empty store is saved over the real one.
func TestRefreshStoreKeepsStoreWhenStateUnreadable(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	st.Favorites = []string{"w1:p1"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := mustLoad(t)

	if err := os.Remove(statePath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath(), 0o700); err != nil {
		t.Fatal(err)
	}
	fresh := refreshStore(held)
	if fresh != held {
		t.Fatal("refresh must keep the in-memory store when the state file cannot be read")
	}
	if fresh.Panes["w1:p1"] == nil || fresh.Panes["w1:p1"].Visits != 1 {
		t.Fatalf("in-memory store lost: %+v", fresh.Panes)
	}

	if err := os.Remove(statePath()); err != nil {
		t.Fatal(err)
	}
	if got := refreshStore(fresh); got != fresh {
		t.Fatal("refresh must keep the in-memory store when the state file is gone")
	}
	if err := saveStore(fresh); err != nil {
		t.Fatal(err)
	}
	final := mustLoad(t)
	if final.Panes["w1:p1"] == nil || len(final.Favorites) != 1 {
		t.Fatalf("an empty store was saved over the real one: %+v", final)
	}
}

// TestPickerDoesNotPersistAfterFailedRefresh is the fail-closed regression on
// the picker writer path: a mid-session refresh that hits a real read error
// must stop the picker from persisting a stale store over a concurrent hook
// write.
func TestPickerDoesNotPersistAfterFailedRefresh(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	picker := mustLoad(t)

	hook := mustLoad(t)
	hook.Panes["w1:p1"].Visits++
	hook.Panes["w1:p9"] = &paneStat{Visits: 3, Last: 123, Label: "hook"}
	writeStoreDirect(t, hook)

	if err := os.Chmod(statePath(), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(statePath(), 0o600)
	if _, err := os.ReadFile(statePath()); err == nil {
		t.Skip("cannot make the state file unreadable")
	}
	if got := refreshStore(picker); got != picker {
		t.Fatal("refresh must keep the in-memory store on a read error")
	}

	picker.ActiveQuery = "typed"
	if err := saveStore(picker); err == nil {
		t.Fatal("the picker persisted a stale store after a failed refresh")
	}

	if err := os.Chmod(statePath(), 0o600); err != nil {
		t.Fatal(err)
	}
	final := mustLoad(t)
	if final.Panes["w1:p1"] == nil || final.Panes["w1:p1"].Visits != 2 {
		t.Fatalf("hook visit erased: %+v", final.Panes["w1:p1"])
	}
	if final.Panes["w1:p9"] == nil {
		t.Fatal("hook pane erased")
	}
}

// TestResyncKeepsSelectionOnSamePane is the wrong-pane regression: rows
// [A,B,C] with the cursor on B, an external write re-sorts to [C,A,B], and
// the applied keypress must still target B's pane ID.
func TestResyncKeepsSelectionOnSamePane(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)
	st.Panes["w1:pA"] = &paneStat{Label: "A", Last: 300}
	st.Panes["w1:pB"] = &paneStat{Label: "B", Last: 200}
	st.Panes["w1:pC"] = &paneStat{Label: "C", Last: 100}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	frozen := &snapshot{Panes: []paneRec{
		{PaneID: "w1:pA"}, {PaneID: "w1:pB"}, {PaneID: "w1:pC"},
	}}
	held := mustLoad(t)
	rows := rowsFor(frozen, held, view{Kind: "all"}, sortDefault)
	if len(rows) != 3 || rows[0].PaneID != "w1:pA" || rows[1].PaneID != "w1:pB" || rows[2].PaneID != "w1:pC" {
		t.Fatalf("setup order = %v, want [A B C]", rows)
	}
	sel := 1 // the cursor sits on B

	ext := mustLoad(t)
	ext.Panes["w1:pC"].Last = 400
	writeStoreDirect(t, ext)

	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	fresh := refreshStore(held)
	if fresh == held {
		t.Fatal("resync must adopt the external write")
	}
	next := rowsFor(frozen, fresh, view{Kind: "all"}, sortDefault)
	if len(next) != 3 || next[0].PaneID != "w1:pC" || next[1].PaneID != "w1:pA" || next[2].PaneID != "w1:pB" {
		t.Fatalf("external resort = %v, want [C A B]", next)
	}
	sel = anchorSel(rows, next, sel, held.ActivePaneID)
	target := next[sel].PaneID
	if target != rows[1].PaneID || target != "w1:pB" {
		t.Fatalf("key press targeted %q, want %q", target, "w1:pB")
	}
}

// TestFirstResyncKeepsRestoreAnchor models the startup window: the picker
// opens on ActivePaneID=B in [A,B,C] and a hook write re-sorts to [C,A,B]
// before the first frame's resync. The selection must still name B.
func TestFirstResyncKeepsRestoreAnchor(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)
	st.Panes["w1:pA"] = &paneStat{Label: "A", Last: 300}
	st.Panes["w1:pB"] = &paneStat{Label: "B", Last: 200}
	st.Panes["w1:pC"] = &paneStat{Label: "C", Last: 100}
	st.ActivePaneID = "w1:pB"
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	frozen := &snapshot{Panes: []paneRec{
		{PaneID: "w1:pA"}, {PaneID: "w1:pB"}, {PaneID: "w1:pC"},
	}}

	held := mustLoad(t)
	rows := rowsFor(frozen, held, view{Kind: "all"}, sortDefault)
	sel := pinSel(rows, 0, held.ActivePaneID)
	if rows[sel].PaneID != "w1:pB" {
		t.Fatalf("restore pinned %q, want %q", rows[sel].PaneID, "w1:pB")
	}

	ext := mustLoad(t)
	ext.Panes["w1:pC"].Last = 400
	writeStoreDirect(t, ext)

	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	fresh := refreshStore(held)
	if fresh == held {
		t.Fatal("resync must adopt the external write")
	}
	next := rowsFor(frozen, fresh, view{Kind: "all"}, sortDefault)
	if len(next) != 3 || next[0].PaneID != "w1:pC" || next[1].PaneID != "w1:pA" || next[2].PaneID != "w1:pB" {
		t.Fatalf("external resort = %v, want [C A B]", next)
	}
	if got := anchorSel(nil, next, sel, fresh.ActivePaneID); next[got].PaneID != "w1:pB" {
		t.Fatalf("first resync moved the selection to %q, want %q", next[got].PaneID, "w1:pB")
	}
}

// TestUnsavedHeldPanesAreNoRecreationEvidence pins the reference of the
// recreation check: it judges a changed file against the proven bytes, so a
// pane stat this process created and never saved is no evidence of a
// rebuild. Opening the Inbox creates such stats for live rows. A hook write
// in the startup window is adopted, and a refused save rolls back cleanly
// instead of tainting the session with a false rebuild.
func TestUnsavedHeldPanesAreNoRecreationEvidence(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Last: 100}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	// The picker opens on the Inbox: its rows create a stat for the live
	// pane that was never focused, and that stat is not on disk.
	st := mustLoad(t)
	frozen := &snapshot{Panes: []paneRec{{PaneID: "w1:p1"}, {PaneID: "w1:p2"}}}
	rowsFor(frozen, st, view{Kind: "inbox"}, sortDefault)
	if st.Panes["w1:p2"] == nil {
		t.Fatal("setup: the Inbox rows must create the unsaved stat")
	}

	// A focus hook saves a visit before the picker's first resync.
	hook := readStoreDirect(t)
	hook.Panes["w1:p1"].Visits++
	hook.Panes["w1:p1"].Last = time.Now().Unix()
	writeStoreDirect(t, hook)

	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	fresh := refreshStore(st)
	if fresh == st {
		t.Fatal("the hook write must be adopted: an unsaved stat is no rebuild evidence")
	}
	if fresh.Panes["w1:p1"].Visits != 2 {
		t.Fatalf("the hook visit was lost: %+v", fresh.Panes["w1:p1"])
	}

	// e archives the row whose stat is still unsaved, and the save fails.
	rowsFor(frozen, fresh, view{Kind: "inbox"}, sortDefault)
	fresh.Panes["w1:p2"].Done = true
	if err := os.Mkdir(statePath()+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	kept, err := persistStore(lk, fresh)
	if err == nil {
		t.Fatal("the save must fail while recall.json.tmp blocks it")
	}
	if saveTaint.Load() {
		t.Fatal("the rollback read the unchanged file as a rebuild and tainted the store")
	}
	if ps := kept.Panes["w1:p2"]; ps != nil && ps.Done {
		t.Fatal("the refused mutation lingered in memory")
	}
	if err := os.Remove(statePath() + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("the next save must land: %v", err)
	}
	if final := mustLoad(t); final.Panes["w1:p1"].Visits != 2 {
		t.Fatalf("the hook visit did not survive the picker's save: %+v", final.Panes["w1:p1"])
	}
}

// TestCLIWriterRefusesWriteWithoutLock is the fail-closed regression: when
// recall.lock cannot be taken, jev reset and favorite must error out and
// leave recall.json byte-identical instead of writing unlocked.
func TestCLIWriterRefusesWriteWithoutLock(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.ensureJev()
	seed.Jev.Weights = map[string]float64{"label:docs": 0.5}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"jev", "reset"}, {"favorite", "w1:pA"}} {
		argv := append([]string{"run", "."}, args...)
		out, err := exec.Command("go", argv...).CombinedOutput()
		if err == nil {
			t.Fatalf("%v wrote without the lock: %s", args, out)
		}
		after, err := os.ReadFile(statePath())
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("%v rewrote recall.json without the lock", args)
		}
	}
}

// TestPickerKeepsAndRematerializesStateWhenFileGone pins the picker contract
// behind the no-silent-loss intent: a vanished recall.json never empties the
// in-memory dataset, and the next successful save writes it back.
func TestPickerKeepsAndRematerializesStateWhenFileGone(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	seed.Favorites = []string{"w1:p1"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := os.Remove(statePath()); err != nil {
		t.Fatal(err)
	}

	if got := refreshStore(st); got != st {
		t.Fatal("refresh must keep the in-memory store when the file vanishes")
	}
	if err := saveStore(st); err != nil {
		t.Fatalf("the next successful save must re-materialize the file: %v", err)
	}
	final := mustLoad(t)
	if final.Panes["w1:p1"] == nil || final.Panes["w1:p1"].Visits != 1 || final.Panes["w1:p1"].Note != "typed" {
		t.Fatalf("the dataset must survive the gap: %+v", final.Panes["w1:p1"])
	}
	if len(final.Favorites) != 1 {
		t.Fatalf("favorites must survive the gap: %v", final.Favorites)
	}
}

// TestIdlePickerKeepsDatasetAndSaveWritesIt pins the idle-picker case: the
// file vanishes and a hook rebuilds it from one pane while the picker sits
// idle, with no refresh between the two events. The rebuild drops panes no
// prune can drop, so it is a recreation and not a writer's continuation: the
// picker keeps its dataset, the vanish episode opens, and a later rebuild
// that re-records every pane but none of the held content is refused too.
// The next save writes the dataset back in full.
func TestIdlePickerKeepsDatasetAndSaveWritesIt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		watch bool
	}{
		{"rebuild between events", false},
		{"rebuild after a watched vanish", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			seed := mustLoad(t)
			seed.TabsInit = true
			seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Note: "call vendor"}
			seed.Panes["w1:p2"] = &paneStat{Visits: 2, Label: "two"}
			seed.Panes["w1:p3"] = &paneStat{Visits: 3, Label: "three"}
			if err := saveStore(seed); err != nil {
				t.Fatal(err)
			}

			st := mustLoad(t)
			st.Panes["w1:p3"].Visits = 4
			if err := os.Remove(statePath()); err != nil {
				t.Fatal(err)
			}
			if tc.watch {
				if got := refreshStore(st); got != st {
					t.Fatal("refresh must keep the in-memory store when the file vanishes")
				}
			}
			// A hook's honest fresh-store write rebuilds the file from one
			// pane; the picker is idle between the vanish and the rebuild.
			writeStoreDirect(t, &store{Version: 1, Panes: map[string]*paneStat{
				"w1:p9": {Visits: 1, Label: "hook"},
			}})

			if got := refreshStore(st); got != st {
				t.Fatal("the rebuilt file must not replace the picker's dataset")
			}

			// More focus hooks re-record every held pane before any save.
			stripped := &store{Version: 1, Panes: map[string]*paneStat{}}
			for id, ps := range st.Panes {
				stripped.Panes[id] = &paneStat{Visits: ps.Visits + 1, Last: time.Now().Unix(), Label: ps.Label}
			}
			writeStoreDirect(t, stripped)
			if got := refreshStore(st); got != st {
				t.Fatal("a rebuild that drops the held note must not replace the picker's dataset")
			}

			if err := saveStore(st); err != nil {
				t.Fatalf("the picker's save must write the dataset: %v", err)
			}
			final := mustLoad(t)
			for _, id := range []string{"w1:p1", "w1:p2", "w1:p3"} {
				if final.Panes[id] == nil {
					t.Fatalf("dataset pane %s was dropped: %+v", id, final.Panes)
				}
			}
			if final.Panes["w1:p3"].Visits != 4 {
				t.Fatalf("dataset change lost: %+v", final.Panes["w1:p3"])
			}
			if final.Panes["w1:p1"].Note != "call vendor" {
				t.Fatalf("the held note was dropped: %+v", final.Panes["w1:p1"])
			}

			// The episode is over once the dataset lands: a writer's write is
			// adopted again.
			hook := mustLoad(t)
			hook.Panes["w1:p1"].Visits++
			hook.Panes["w1:p9"] = &paneStat{Visits: 2, Label: "hook"}
			writeStoreDirect(t, hook)
			if got := refreshStore(final); got == final {
				t.Fatal("a writer's write must be adopted again after the dataset lands")
			}
		})
	}
}

// TestRefusedSaveKeepsDatasetAfterRebuild pins the save-time half of the same
// rule and the taint scope inside the episode. A refused save while the file
// is rebuilt or still gone must not roll back into that file, so the store is
// tainted with the refused mutation in memory. That holds for a rebuild that
// re-records every held pane without its content too. The taint stops only
// the refused mutation: the next save writes the held dataset back without
// it, the note names the last action as not applied, and repeating the
// action lands.
func TestRefusedSaveKeepsDatasetAfterRebuild(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rebuild map[string]*paneStat
	}{
		{"rebuilt from one pane", map[string]*paneStat{
			"w1:p9": {Visits: 1, Label: "hook"},
		}},
		{"rebuilt from every pane, stripped", map[string]*paneStat{
			"w1:p1": {Visits: 1, Label: "one"},
			"w1:p2": {Visits: 1, Label: "two"},
			"w1:p3": {Visits: 1, Label: "three"},
		}},
		{"still gone", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			seed := mustLoad(t)
			seed.TabsInit = true
			seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Note: "call vendor"}
			seed.Panes["w1:p2"] = &paneStat{Visits: 2, Label: "two"}
			seed.Panes["w1:p3"] = &paneStat{Visits: 3, Label: "three"}
			seed.Favorites = []string{"w1:p2", "w1:p3"}
			if err := saveStore(seed); err != nil {
				t.Fatal(err)
			}

			lk := &storeLock{}
			st := mustLoad(t)
			if err := os.Remove(statePath()); err != nil {
				t.Fatal(err)
			}
			if tc.rebuild != nil {
				writeStoreDirect(t, &store{Version: 1, Panes: tc.rebuild})
			}
			before, _ := os.ReadFile(statePath())

			// e archives w1:p3, and its save is refused.
			st.Panes["w1:p3"].Done = true
			if err := os.Mkdir(lockPath(), 0o700); err != nil {
				t.Fatal(err)
			}
			kept, err := persistStore(lk, st)
			if err == nil {
				t.Fatal("the save must be refused while the lock is unavailable")
			}
			if kept.Panes["w1:p1"] == nil || kept.Panes["w1:p1"].Note != "call vendor" || kept.Panes["w1:p2"] == nil || kept.Panes["w1:p3"] == nil || !kept.isFavorite("w1:p2") || !kept.isFavorite("w1:p3") {
				t.Fatalf("the rollback replaced the dataset with the rebuild: %+v %v", kept.Panes, kept.Favorites)
			}
			if after, _ := os.ReadFile(statePath()); !bytes.Equal(after, before) {
				t.Fatal("the refused save changed recall.json")
			}

			// The lock comes back and the next save runs.
			if err := os.Remove(lockPath()); err != nil {
				t.Fatal(err)
			}
			if err := lk.acquire(); err != nil {
				t.Fatal(err)
			}
			defer lk.release()
			kept, err = persistStore(lk, kept)
			if err == nil {
				t.Fatal("the save that drops the refused mutation must report it, never a plain save")
			}
			note := &saveNote{}
			if got := note.refuse(err); got != "not saved: last action not applied - repeat it" {
				t.Fatalf("the note must name the last action as not applied: %q", got)
			}
			final := mustLoad(t)
			for _, id := range []string{"w1:p1", "w1:p2", "w1:p3"} {
				if final.Panes[id] == nil {
					t.Fatalf("the held dataset was not written back: %+v", final.Panes)
				}
			}
			if final.Panes["w1:p1"].Note != "call vendor" || !final.isFavorite("w1:p2") || !final.isFavorite("w1:p3") {
				t.Fatalf("the held content was not written back: %+v %v", final.Panes["w1:p1"], final.Favorites)
			}
			if final.Panes["w1:p3"].Done || kept.Panes["w1:p3"].Done {
				t.Fatal("the refused mutation persisted")
			}

			// The refused action stays repeatable and lands now.
			kept.Panes["w1:p3"].Done = true
			if _, err := persistStore(lk, kept); err != nil {
				t.Fatalf("the repeated action must land: %v", err)
			}
			if !mustLoad(t).Panes["w1:p3"].Done {
				t.Fatal("the repeated action did not reach disk")
			}
		})
	}
}

// TestLegitimateWriterWinsOverHeldPin pins legitimate-writer-wins: the CLI
// un-favorite and a hook's prune are the writer's update, and a picker-held
// pin never reverts them. A pruned old pane a later writer recreates is not a
// recreation: only its fresh record is adopted, and a rebuild after it leaves
// the un-favorite in effect with nothing resurrected.
func TestLegitimateWriterWinsOverHeldPin(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:old"] = &paneStat{Visits: 9, Label: "old", Last: 1}
	seed.Favorites = []string{"w1:old"}
	for i := 0; i < pruneKeep; i++ {
		seed.Panes["fill:"+strconv.Itoa(i)] = &paneStat{Label: "fill", Last: int64(2 + i)}
	}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	st := mustLoad(t) // the picker opens holding the favorite

	if out, err := exec.Command("go", "run", ".", "favorite", "w1:old").CombinedOutput(); err != nil {
		t.Fatalf("favorite: %v: %s", err, out)
	}

	// A hook records a visit and prunes the unpinned old pane the keep
	// window has passed.
	hook := mustLoad(t)
	hp := hook.ensurePane("w1:live")
	hp.Visits++
	hp.Last = time.Now().Unix()
	hook.prune(map[string]bool{"w1:live": true}, pruneKeep)
	writeStoreDirect(t, hook)

	fresh := refreshStore(st)
	if fresh == st {
		t.Fatal("the writer's update must be adopted, not reverted by the held pin")
	}
	if fresh.isFavorite("w1:old") || fresh.Panes["w1:old"] != nil {
		t.Fatalf("the un-favorite or prune was reverted: fav=%v pane=%+v", fresh.isFavorite("w1:old"), fresh.Panes["w1:old"])
	}

	// The old pane comes back in a writer's write. Only the fresh record is
	// adopted; nothing of the held copy returns with it.
	hook2 := mustLoad(t)
	hook2.Panes["w1:old"] = &paneStat{Visits: 1, Label: "old", Last: time.Now().Unix()}
	writeStoreDirect(t, hook2)
	if fresh = refreshStore(fresh); fresh.Panes["w1:old"] == nil || fresh.Panes["w1:old"].Visits != 1 {
		t.Fatalf("the recreated pane record was not adopted: %+v", fresh.Panes["w1:old"])
	}
	if fresh.isFavorite("w1:old") {
		t.Fatal("the held favorite resurrected on the recreated pane")
	}

	// A rebuild after it is a recreation: the dataset wins and the save
	// writes it, leaving the un-favorite in effect and nothing resurrected.
	if err := os.Remove(statePath()); err != nil {
		t.Fatal(err)
	}
	writeStoreDirect(t, &store{Version: 1, Panes: map[string]*paneStat{
		"w1:p9": {Visits: 1, Label: "hook"},
	}})
	if got := refreshStore(fresh); got != fresh {
		t.Fatal("the rebuild must not replace the picker's dataset")
	}
	if err := saveStore(fresh); err != nil {
		t.Fatalf("the save must write the dataset: %v", err)
	}
	final := mustLoad(t)
	if final.isFavorite("w1:old") {
		t.Fatal("the un-favorite did not survive the rebuild")
	}
	if final.Panes["w1:old"] == nil || final.Panes["w1:old"].Visits != 1 {
		t.Fatalf("the old pane record resurrected: %+v", final.Panes["w1:old"])
	}
	if final.Panes["fill:0"] == nil {
		t.Fatal("the held dataset was dropped")
	}
}

// TestCleanReloadAdoptEndsVanishWatch pins the episode contract at the save
// boundary: a rollback that adopts bytes proving continuation ends the vanish
// episode, the same as the refresh adoption and the landed save, so a
// keep-window prune landing in a later idle gap is judged on its own merits.
// A stale episode reads that write as a recreation, and the next save then
// restores the un-favorite the writer removed.
func TestCleanReloadAdoptEndsVanishWatch(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:old"] = &paneStat{Visits: 9, Label: "old", Last: 1}
	seed.Favorites = []string{"w1:old"}
	for i := 0; i < pruneKeep; i++ {
		seed.Panes["fill:"+strconv.Itoa(i)] = &paneStat{Label: "fill", Last: int64(2 + i)}
	}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	st := mustLoad(t) // the picker opens holding the favorite

	// The file vanishes on the picker's watch.
	if err := os.Remove(statePath()); err != nil {
		t.Fatal(err)
	}
	if got := refreshStore(st); got != st {
		t.Fatal("refresh must keep the in-memory store when the file vanishes")
	}

	// A rebuild that carries every held pane and content unit lands while
	// the picker is idle: it proves continuation.
	rebuilt := &store{Version: 1, Panes: map[string]*paneStat{}, Favorites: append([]string(nil), st.Favorites...)}
	for id, ps := range st.Panes {
		rebuilt.Panes[id] = &paneStat{Visits: ps.Visits, Last: ps.Last, Label: ps.Label}
	}
	writeStoreDirect(t, rebuilt)

	// A save fails, and its rollback adopts those bytes under the lock.
	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(statePath()+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	kept, err := persistStore(lk, st)
	lk.release()
	if err == nil {
		t.Fatal("the save must fail while recall.json.tmp blocks the rename")
	}
	if kept == st || kept.Panes["w1:old"] == nil || !kept.isFavorite("w1:old") {
		t.Fatalf("the rollback must adopt the proven continuation: %+v", kept)
	}
	if err := os.Remove(statePath() + ".tmp"); err != nil {
		t.Fatal(err)
	}

	// One idle gap: the CLI un-favorites the old pane and a hook's
	// keep-window prune drops it.
	if out, err := exec.Command("go", "run", ".", "favorite", "w1:old").CombinedOutput(); err != nil {
		t.Fatalf("favorite: %v: %s", err, out)
	}
	hook := readStoreDirect(t)
	hp := hook.ensurePane("w1:live")
	hp.Visits++
	hp.Last = time.Now().Unix()
	hook.prune(map[string]bool{"w1:live": true}, pruneKeep)
	writeStoreDirect(t, hook)

	// The picker's next keypress resyncs, and its save lands the update.
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	next := refreshStore(kept)
	if next == kept {
		t.Fatal("the un-favorite and prune write must be adopted")
	}
	if _, err := persistStore(lk, next); err != nil {
		t.Fatalf("the picker's save must land: %v", err)
	}
	final := mustLoad(t)
	if final.isFavorite("w1:old") {
		t.Fatal("the picker's save restored a removed favorite")
	}
	if final.Panes["w1:old"] != nil {
		t.Fatalf("the picker's save undid the prune: %+v", final.Panes["w1:old"])
	}
}

// seedHeldUnits saves a store that carries one of each content unit the
// vanish episode guards: a note, a manual block, a reminder, a favorite, a
// custom tab, and a Jev weight.
func seedHeldUnits(t *testing.T) {
	t.Helper()
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Note: "call vendor"}
	seed.Panes["w1:p2"] = &paneStat{Visits: 2, Label: "two", ManualBlocked: true}
	seed.Panes["w1:p3"] = &paneStat{Visits: 3, Label: "three", RemindAt: time.Now().Add(time.Hour).Unix()}
	seed.Favorites = []string{"w1:p2"}
	seed.Tabs = append(defaultTabs(), customTab{Name: "Work", Sort: "last", Limit: 50})
	seed.ensureJev()
	seed.Jev.Weights = map[string]float64{"label:docs": 0.5}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
}

// TestPickerAdoptsSupersetAfterVanish pins the other half of the rule: a
// recreated file that holds every pane and content unit the picker holds,
// and more, is another saver writing more, not less, so the picker adopts it
// normally.
func TestPickerAdoptsSupersetAfterVanish(t *testing.T) {
	useTempConfig(t)
	seedHeldUnits(t)

	st := mustLoad(t)
	superset := readStoreDirect(t)
	if err := os.Remove(statePath()); err != nil {
		t.Fatal(err)
	}
	if got := refreshStore(st); got != st {
		t.Fatal("refresh must keep the in-memory store when the file vanishes")
	}
	superset.Panes["w1:p9"] = &paneStat{Visits: 5, Label: "hook"}
	writeStoreDirect(t, superset)

	fresh := refreshStore(st)
	if fresh == st {
		t.Fatal("a superset recreation must be adopted normally")
	}
	if fresh.Panes["w1:p9"] == nil || fresh.Panes["w1:p9"].Visits != 5 {
		t.Fatalf("the adopted superset lost the other saver's pane: %+v", fresh.Panes)
	}
}

// TestEpisodeRefusesRebuildMissingContent pins the continuation proof inside
// a vanish episode: a file that comes back with every held pane but without
// a held content unit was built again, not continued. The picker keeps its
// dataset and the next save writes every unit back.
func TestEpisodeRefusesRebuildMissingContent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		strip func(s *store)
	}{
		{"note", func(s *store) { s.Panes["w1:p1"].Note = "" }},
		{"manual block", func(s *store) { s.Panes["w1:p2"].ManualBlocked = false }},
		{"reminder", func(s *store) { s.Panes["w1:p3"].RemindAt = 0 }},
		{"favorite", func(s *store) { s.Favorites = nil }},
		{"tab", func(s *store) { s.Tabs = defaultTabs() }},
		{"jev weight", func(s *store) { s.Jev.Weights = nil }},
		{"every unit", func(s *store) {
			panes := map[string]*paneStat{}
			for id, ps := range s.Panes {
				panes[id] = &paneStat{Visits: ps.Visits + 1, Last: time.Now().Unix(), Label: ps.Label}
			}
			*s = store{Version: 1, Panes: panes}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			seedHeldUnits(t)

			st := mustLoad(t)
			rebuilt := readStoreDirect(t)
			if err := os.Remove(statePath()); err != nil {
				t.Fatal(err)
			}
			if got := refreshStore(st); got != st {
				t.Fatal("refresh must keep the in-memory store when the file vanishes")
			}
			tc.strip(rebuilt)
			writeStoreDirect(t, rebuilt)

			if got := refreshStore(st); got != st {
				t.Fatalf("a rebuild without the held %s must not replace the picker's dataset", tc.name)
			}
			if err := saveStore(st); err != nil {
				t.Fatalf("the picker's save must write the dataset: %v", err)
			}
			assertHeldUnits(t, mustLoad(t))
		})
	}
}

// assertHeldUnits fails unless s carries every content unit seedHeldUnits
// saved.
func assertHeldUnits(t *testing.T, s *store) {
	t.Helper()
	if p := s.Panes["w1:p1"]; p == nil || p.Note != "call vendor" {
		t.Fatalf("the note was not written back: %+v", p)
	}
	if p := s.Panes["w1:p2"]; p == nil || !p.ManualBlocked || !s.isFavorite("w1:p2") {
		t.Fatalf("the manual block or favorite was not written back: %+v %v", p, s.Favorites)
	}
	if p := s.Panes["w1:p3"]; p == nil || p.RemindAt == 0 {
		t.Fatalf("the reminder was not written back: %+v", p)
	}
	if len(s.Tabs) != 3 || s.Tabs[2].Name != "Work" {
		t.Fatalf("the custom tab was not written back: %+v", s.Tabs)
	}
	if s.Jev == nil || s.Jev.Weights["label:docs"] != 0.5 {
		t.Fatalf("the Jev weight was not written back: %+v", s.Jev)
	}
}

// rerecordStripped deletes recall.json and plays focus hooks for ids while
// the picker holding held waits for a key: each hook loads what the previous
// one wrote, or starts fresh, and records only its pane's visit, the way
// recordFocus does. The rebuilt file carries the panes and none of the held
// content.
func rerecordStripped(t *testing.T, held *store, ids ...string) {
	t.Helper()
	if err := os.Remove(statePath()); err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		hook := &store{Version: 1, Panes: map[string]*paneStat{}}
		if _, err := os.Stat(statePath()); err == nil {
			hook = readStoreDirect(t)
		}
		ps := hook.ensurePane(id)
		ps.Visits++
		ps.Last = time.Now().Unix()
		ps.Label = held.Panes[id].Label
		writeStoreDirect(t, hook)
	}
}

// TestIdlePickerRefusesStrippedFullRebuild pins the continuation proof at
// every adoption, not only inside an observed episode. recall.json vanishes
// while the picker waits for a key, and focus hooks re-record every held
// pane before any refresh sees the gap, so the rebuilt file carries every
// held pane ID and none of the held content. The picker's next refresh
// refuses it and keeps the dataset, and the next save writes the full store
// back. A writer that changes a unit and keeps it, removes the only
// favorite, or resets the Jev weights is adopted.
func TestIdlePickerRefusesStrippedFullRebuild(t *testing.T) {
	useTempConfig(t)
	seedHeldUnits(t)

	st := mustLoad(t)
	rerecordStripped(t, st, "w1:p1", "w1:p2", "w1:p3")

	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	if got := refreshStore(st); got != st {
		t.Fatal("the stripped rebuild must not replace the picker's dataset")
	}
	if _, err := persistStore(lk, st); err != nil {
		t.Fatalf("the next save must write the dataset back: %v", err)
	}
	final := mustLoad(t)
	assertHeldUnits(t, final)

	edit := readStoreDirect(t)
	edit.Panes["w1:p1"].Note = "call vendor at 3"
	edit.Favorites = []string{}
	edit.Jev.Weights = nil
	writeStoreDirect(t, edit)
	fresh := refreshStore(final)
	if fresh == final {
		t.Fatal("a writer's change that keeps every picker-only unit must be adopted")
	}
	if fresh.Panes["w1:p1"].Note != "call vendor at 3" || fresh.isFavorite("w1:p2") || fresh.Jev != nil && len(fresh.Jev.Weights) > 0 {
		t.Fatalf("the writer's change was not adopted: %+v %v %+v", fresh.Panes["w1:p1"], fresh.Favorites, fresh.Jev)
	}
}

// seedFavoritesOnly saves three panes whose only content units are the
// given favorites and one Jev weight.
func seedFavoritesOnly(t *testing.T, favorites ...string) {
	t.Helper()
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 4, Label: "one"}
	seed.Panes["w1:p2"] = &paneStat{Visits: 5, Label: "two"}
	seed.Panes["w1:p3"] = &paneStat{Visits: 6, Label: "three"}
	seed.Favorites = favorites
	seed.ensureJev()
	seed.Jev.Weights = map[string]float64{"label:docs": 0.5}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
}

// TestUnfavoritesAndJevResetAdopt pins the favorite and Jev weight side of
// the every-adoption check with the real commands. Un-favorites, one or
// several in one idle gap, and a jev reset outside an episode are
// legitimate writes: the picker adopts them, and its next save keeps them.
func TestUnfavoritesAndJevResetAdopt(t *testing.T) {
	for _, tc := range []struct {
		name      string
		favorites []string
		cmds      [][]string
		check     func(s *store) bool
	}{
		{"un-favorite the only favorite", []string{"w1:p1"}, [][]string{{"favorite", "w1:p1"}},
			func(s *store) bool { return !s.isFavorite("w1:p1") }},
		{"un-favorite one of two", []string{"w1:p1", "w1:p2"}, [][]string{{"favorite", "w1:p1"}},
			func(s *store) bool { return !s.isFavorite("w1:p1") && s.isFavorite("w1:p2") }},
		{"un-favorite both of two in one gap", []string{"w1:p1", "w1:p2"}, [][]string{{"favorite", "w1:p1"}, {"favorite", "w1:p2"}},
			func(s *store) bool { return !s.isFavorite("w1:p1") && !s.isFavorite("w1:p2") }},
		{"jev reset", []string{"w1:p1", "w1:p2"}, [][]string{{"jev", "reset"}},
			func(s *store) bool { return s.Jev == nil || len(s.Jev.Weights) == 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			seedFavoritesOnly(t, tc.favorites...)

			st := mustLoad(t)
			for _, args := range tc.cmds {
				if out, err := exec.Command("go", append([]string{"run", "."}, args...)...).CombinedOutput(); err != nil {
					t.Fatalf("%v: %v: %s", args, err, out)
				}
			}

			lk := &storeLock{}
			if err := lk.acquire(); err != nil {
				t.Fatal(err)
			}
			defer lk.release()
			fresh := refreshStore(st)
			if fresh == st || !tc.check(fresh) {
				t.Fatalf("%v must be adopted: %v %+v", tc.cmds, fresh.Favorites, fresh.Jev)
			}
			if _, err := persistStore(lk, fresh); err != nil {
				t.Fatalf("the picker's save must land: %v", err)
			}
			if final := mustLoad(t); !tc.check(final) {
				t.Fatalf("the picker's save reverted %v: %v %+v", tc.cmds, final.Favorites, final.Jev)
			}
		})
	}
}

// TestWriterCreatesFreshStateWhenFileGone pins that a missing recall.json is
// honest fresh state: a stale recall.lock beside it proves nothing, because
// acquire creates the lock before the first successful save.
func TestWriterCreatesFreshStateWhenFileGone(t *testing.T) {
	useTempConfig(t)
	if err := os.WriteFile(lockPath(), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	argv := append([]string{"run", "."}, "favorite", "w1:p2")
	if out, err := exec.Command("go", argv...).CombinedOutput(); err != nil {
		t.Fatalf("a missing recall.json must be fresh state: %v: %s", err, out)
	}
	if final := mustLoad(t); !final.isFavorite("w1:p2") {
		t.Fatal("the writer's change must land in the fresh store")
	}
}

// TestJevResetClearsLearnings covers the ordinary locked path of the command
// that now fails closed.
func TestJevResetClearsLearnings(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.ensureJev()
	seed.Jev.Weights = map[string]float64{"label:docs": 0.5}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "run", ".", "jev", "reset").CombinedOutput(); err != nil {
		t.Fatalf("jev reset: %v: %s", err, out)
	}
	if got := mustLoad(t); got.Jev == nil || len(got.Jev.Weights) != 0 {
		t.Fatalf("learnings not cleared: %+v", got.Jev)
	}
}

// TestWritersRefuseToPersistOnReadError is the empty-on-read-error
// regression: a state file that is present but unreadable must never be
// erased by a writer that would adopt an empty store.
func TestWritersRefuseToPersistOnReadError(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	seed.Favorites = []string{"w1:p1"}
	seed.ensureJev()
	seed.Jev.Weights = map[string]float64{"label:docs": 0.5}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(statePath(), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(statePath(), 0o600)
	if _, err := os.ReadFile(statePath()); err == nil {
		t.Skip("cannot make the state file unreadable")
	}
	if st, err := loadStore(); err == nil {
		t.Fatalf("loadStore adopted a store on a read error: %+v", st)
	}
	for _, args := range [][]string{{"favorite", "w1:p2"}, {"jev", "reset"}} {
		argv := append([]string{"run", "."}, args...)
		if out, err := exec.Command("go", argv...).CombinedOutput(); err == nil {
			t.Fatalf("%v wrote despite the unreadable state file: %s", args, out)
		}
	}
	if err := os.Chmod(statePath(), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(statePath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("a writer erased recall.json after a read error")
	}
}

// TestRefusedSaveRollsBackAndNotes pins the refusal contract: a refused save
// reports a refusal, never a save, and rolls the uncommitted mutation back
// under the lock so nothing unsaved lingers and the keypress stays
// repeatable.
func TestRefusedSaveRollsBackAndNotes(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	kept, err := persistStore(lk, st)
	if err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	if kept.Panes["w1:p1"].Note != "" {
		t.Fatalf("the refused mutation lingered in memory: %q", kept.Panes["w1:p1"].Note)
	}
	note := "not saved: " + err.Error()
	if !strings.HasPrefix(note, "not saved:") {
		t.Fatalf("the note must state a refusal: %q", note)
	}
	frame := renderFrame([]row{{PaneID: "w1:p1", Label: "one"}}, 0, 0, vimline.View{}, note, 80, 24, []string{"All"}, 0, "both", "")
	if !strings.Contains(frame, "not saved: ") {
		t.Fatalf("the refusal note must render:\n%s", frame)
	}

	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("a later save must succeed: %v", err)
	}
	if got := mustLoad(t).Panes["w1:p1"].Note; got != "" {
		t.Fatalf("the rolled-back mutation must never persist: %q", got)
	}
}

// TestRefusedSaveKeepsConcurrentWrites pins the traced failure: after a
// refused save the hook's visit bumps, un-Done writes, and keep-window prune
// deletions are never persisted over.
func TestRefusedSaveKeepsConcurrentWrites(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Note: "old", Done: true, Last: 400}
	seed.Panes["w1:p2"] = &paneStat{Visits: 1, Label: "two", Last: 300}
	seed.Panes["w1:dead"] = &paneStat{Label: "dead", Last: 1}
	for i := 0; i < pruneKeep; i++ {
		seed.Panes["fill:"+strconv.Itoa(i)] = &paneStat{Label: "fill", Last: int64(2 + i)}
	}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	kept, err := persistStore(lk, st)
	if err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}

	hook := mustLoad(t)
	hook.Panes["w1:p1"].Visits++
	hook.Panes["w1:p1"].Done = false
	hook.Panes["w1:p2"].Visits += 5
	hook.prune(map[string]bool{"w1:p1": true, "w1:p2": true}, pruneKeep)
	writeStoreDirect(t, hook)

	fresh := refreshStore(kept)
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	if _, err := persistStore(lk, fresh); err != nil {
		t.Fatalf("save after the hook write: %v", err)
	}
	final := mustLoad(t)
	if final.Panes["w1:p1"].Note != "old" {
		t.Fatalf("the rolled-back mutation must never persist: %q", final.Panes["w1:p1"].Note)
	}
	if final.Panes["w1:p1"].Visits != 2 || final.Panes["w1:p1"].Done {
		t.Fatalf("the hook visit or un-done was erased: %+v", final.Panes["w1:p1"])
	}
	if final.Panes["w1:p2"].Visits != 6 {
		t.Fatalf("the disjoint hook write was erased: %d", final.Panes["w1:p2"].Visits)
	}
	if final.Panes["w1:dead"] != nil {
		t.Fatal("the hook prune was erased")
	}
}

// TestUnitMergeKeepsOddKeys pins that no merge machinery can corrupt the
// store or resurrect a refused change: a Jev weight keyed with a slash
// round-trips intact, and a refused change leaves no trace in memory or on
// disk.
func TestUnitMergeKeepsOddKeys(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	st := mustLoad(t)
	st.ensureJev()
	if st.Jev.Weights == nil {
		st.Jev.Weights = map[string]float64{}
	}
	st.Jev.Weights["label:fix/store-write-race"] = 0.5
	if _, err := persistStore(lk, st); err != nil {
		t.Fatalf("save: %v", err)
	}
	final := mustLoad(t)
	if got := final.Jev.Weights["label:fix/store-write-race"]; got != 0.5 {
		t.Fatalf("the weight did not round-trip intact: %v (%+v)", got, final.Jev.Weights)
	}

	refused := mustLoad(t)
	refused.Jev.Weights["label:fix/store-write-race"] = -1
	lk.release()
	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	kept, err := persistStore(lk, refused)
	if err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	if got := kept.Jev.Weights["label:fix/store-write-race"]; got != 0.5 {
		t.Fatalf("the refused change lingered in memory: %v", got)
	}
	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("a later save must succeed: %v", err)
	}
	if got := mustLoad(t).Jev.Weights["label:fix/store-write-race"]; got != 0.5 {
		t.Fatalf("the refused change resurfaced on disk: %v", got)
	}
}

// TestRefusedSaveKeepsStoreWhenStateGoneOrCorrupt pins that a fabricated
// empty store never replaces proven live state: when recall.json is missing
// or unparseable at rollback time the in-memory dataset stays intact, the
// note stays a truthful refusal, and refresh adopts nothing. A missing
// recall.json stays honest fresh state: no lock-presence proof can say
// otherwise, because acquire creates recall.lock before the first successful
// save and so proves nothing about prior state.
func TestRefusedSaveKeepsStoreWhenStateGoneOrCorrupt(t *testing.T) {
	for _, tc := range []struct {
		name string
		lose func(t *testing.T)
	}{
		{"deleted", func(t *testing.T) {
			if err := os.Remove(statePath()); err != nil {
				t.Fatal(err)
			}
		}},
		{"corrupt", func(t *testing.T) {
			if err := os.WriteFile(statePath(), []byte("{"), 0o600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			seed := mustLoad(t)
			seed.TabsInit = true
			seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
			seed.Favorites = []string{"w1:p1"}
			if err := saveStore(seed); err != nil {
				t.Fatal(err)
			}

			lk := &storeLock{}
			st := mustLoad(t)
			st.Panes["w1:p1"].Note = "typed"
			tc.lose(t)

			if got := refreshStore(st); got != st {
				t.Fatal("refresh must keep the in-memory store when the state file is gone or corrupt")
			}
			if err := os.Mkdir(lockPath(), 0o700); err != nil {
				t.Fatal(err)
			}
			kept, err := persistStore(lk, st)
			if err == nil {
				t.Fatal("the save must be refused while the lock is unavailable")
			}
			if kept.Panes["w1:p1"] == nil || kept.Panes["w1:p1"].Visits != 1 {
				t.Fatalf("the rollback replaced the live dataset: %+v", kept.Panes)
			}
			if len(kept.Favorites) != 1 {
				t.Fatalf("the rollback dropped favorites: %v", kept.Favorites)
			}
			note := "not saved: " + err.Error()
			frame := renderFrame([]row{{PaneID: "w1:p1", Label: "one"}}, 0, 0, vimline.View{}, note, 80, 24, []string{"All"}, 0, "both", "")
			if !strings.Contains(frame, "not saved: ") {
				t.Fatalf("the refusal note must render:\n%s", frame)
			}
		})
	}
}

// TestRefusalNoteRendersBeforeAnySaveClearsIt pins the note contract: a
// refusal note reaches a rendered frame before any save can clear it, and it
// is gone only after a successful save.
func TestRefusalNoteRendersBeforeAnySaveClearsIt(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"

	kept, err := persistStore(lk, st)
	if err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	note := &saveNote{}
	status := note.refuse(err)

	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("the auto-save must succeed: %v", err)
	}
	status = note.clear(status)
	if !strings.Contains(status, "not saved: ") {
		t.Fatalf("the refusal note was cleared before it rendered: %q", status)
	}

	frame := renderFrame([]row{{PaneID: "w1:p1", Label: "one"}}, 0, 0, vimline.View{}, status, 80, 24, []string{"All"}, 0, "both", "")
	if !strings.Contains(stripANSI(frame), "not saved: ") {
		t.Fatalf("the refusal note must render:\n%s", frame)
	}

	overlay := renderNote(80, 24, []string{"All"}, 0, vimline.View{Label: "note", Text: "x", Focused: true})
	if strings.Contains(overlay, note.text) {
		t.Fatalf("the overlay frame carries no status line:\n%s", overlay)
	}
	note.rendered(overlay)
	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("the later save must succeed: %v", err)
	}
	status = note.clear(status)
	if !strings.Contains(status, "not saved: ") {
		t.Fatalf("an overlay frame must not mark the note seen: %q", status)
	}

	note.rendered(frame)
	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("the later save must succeed: %v", err)
	}
	status = note.clear(status)
	if status != "" {
		t.Fatalf("the note must clear only after a successful save: %q", status)
	}
}

// TestRefusalNoteSeenWhenStatusClips pins that the seen flag is decided from
// the emitted status text: a narrow pane clips the note down to its visible
// head, that head is what the operator sees, and a later landed save may then
// clear the note instead of freezing a stale not-saved status.
func TestRefusalNoteSeenWhenStatusClips(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := os.Mkdir(statePath()+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	kept, err := persistStore(lk, st)
	if err == nil {
		t.Fatal("the save must be refused while recall.json.tmp blocks it")
	}
	note := &saveNote{}
	status := note.refuse(err)
	if len(status) <= 24 {
		t.Fatalf("the note must outlast the 24-column line it clips against: %q", status)
	}

	frame := renderFrame([]row{{PaneID: "w1:p1", Label: "one"}}, 0, 0, vimline.View{}, status, 24, 24, []string{"All"}, 0, "both", "")
	shown := stripANSI(frame)
	if strings.Contains(shown, note.text) {
		t.Fatalf("the frame must clip the note at width 24:\n%s", frame)
	}
	if !strings.Contains(shown, note.text[:24]) {
		t.Fatalf("the frame must show the note's visible head:\n%s", frame)
	}
	note.rendered(frame)

	if err := os.Remove(statePath() + ".tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("the later save must succeed: %v", err)
	}
	if status = note.clear(status); status != "" {
		t.Fatalf("a clipped note the frame showed must clear after a landed save: %q", status)
	}
}

// TestUndoPointerSurvivesRefusedSave pins the undo pointer rule: e publishes
// it only on a landed save, and z spends it only on a landed undo. A refused
// undo rolls the marker back and keeps the pointer armed, so recovery undoes
// the same marker again.
func TestUndoPointerSurvivesRefusedSave(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	st := mustLoad(t)
	save := func() bool {
		next, err := persistStore(lk, st)
		if next != st {
			st = next
		}
		return err == nil
	}

	// e archives the row; the pointer publishes only because its save landed.
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	ps := st.Panes["w1:p1"]
	ps.Done = true
	ps.DoneState = "done"
	st.removeFromInbox("w1:p1")
	if !save() {
		t.Fatal("the done marker must land")
	}
	lk.release()
	mark := "w1:p1"

	// z under a refused save: the marker rolls back, the pointer stays armed.
	mark = undoDone(st, mark, save)
	if mark != "w1:p1" {
		t.Fatalf("the refused undo spent the undo pointer: %q", mark)
	}
	if st.Panes["w1:p1"] == nil || !st.Panes["w1:p1"].Done {
		t.Fatal("the refused undo must roll the marker back")
	}

	// Recovery: the armed pointer undoes the same marker once the save lands.
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	mark = undoDone(st, mark, save)
	if mark != "" {
		t.Fatalf("the landed undo must spend the undo pointer: %q", mark)
	}
	if ps := mustLoad(t).Panes["w1:p1"]; ps == nil || ps.Done {
		t.Fatal("the retried undo must land the marker change on disk")
	}
}

// TestRefusedTeachLeavesNoLogRecord pins the learning-log rule: a teaching
// reaches jev-teachings.jsonl only after its save lands. A refused teach
// rolls back and leaves no record, and its retry appends exactly once.
func TestRefusedTeachLeavesNoLogRecord(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	st := mustLoad(t)
	save := func() bool {
		next, err := persistStore(lk, st)
		if next != st {
			st = next
		}
		return err == nil
	}
	teach := jevTeaching{ID: "teach-1", Feedback: "prefer billing", Intent: "prefer", Pane: jevPaneContext{PaneID: "w1:p1", Label: "one"}}
	logPath := filepath.Join(configDir(), "jev-teachings.jsonl")

	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if commitTeaching(st, teach, save) {
		t.Fatal("the teach must be refused while the lock is unavailable")
	}
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatalf("the refused teach left a learning-log record: %v", err)
	}
	if st.Jev != nil && len(st.Jev.Teachings) != 0 {
		t.Fatal("the refused teach lingered in memory")
	}

	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	if !commitTeaching(st, teach, save) {
		t.Fatal("the retried teach must land")
	}
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Split(strings.TrimSpace(string(data)), "\n"); len(lines) != 1 || !strings.Contains(lines[0], `"teach-1"`) {
		t.Fatalf("the retry must append exactly one record: %q", data)
	}
	if got := mustLoad(t); got.Jev == nil || len(got.Jev.Teachings) != 1 {
		t.Fatalf("the landed teach must reach recall.json: %+v", got.Jev)
	}
}

// TestTaintedRollbackNeverPersistsRefusedMutation pins the taint contract
// outside a vanish episode: a refused change never reaches disk, the note
// reports the live failure while the reload keeps failing, and a clean
// reload clears the taint so the next persist saves only proven state.
func TestTaintedRollbackNeverPersistsRefusedMutation(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"

	if err := os.WriteFile(statePath(), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	kept, err := persistStore(lk, st)
	if err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	if kept.Panes["w1:p1"] == nil || kept.Panes["w1:p1"].Note != "typed" {
		t.Fatal("the in-memory view must keep showing the refused change")
	}

	kept, err = persistStore(lk, kept)
	if err == nil {
		t.Fatal("a tainted save must stay refused while the reload fails")
	}
	var syntax *json.SyntaxError
	if !errors.As(err, &syntax) {
		t.Fatalf("the note must report the live failure: %v", err)
	}
	note := &saveNote{}
	if got := note.refuse(err); strings.Contains(got, "recall.lock unavailable") {
		t.Fatalf("the note must not replay the stale reason: %q", got)
	}
	if b, _ := os.ReadFile(statePath()); string(b) != "{" {
		t.Fatalf("a tainted save wrote recall.json: %q", b)
	}

	writeStoreDirect(t, seed)
	kept, err = persistStore(lk, kept)
	if err == nil {
		t.Fatal("the reload that drops the caller's change must report a refusal, never a save")
	}
	if kept.Panes["w1:p1"] == nil || kept.Panes["w1:p1"].Note != "" {
		t.Fatalf("the clean reload must drop the refused change: %+v", kept.Panes["w1:p1"])
	}

	if _, err := persistStore(lk, kept); err != nil {
		t.Fatalf("persistence must resume after the reload: %v", err)
	}
	final := mustLoad(t)
	if final.Panes["w1:p1"].Note != "" {
		t.Fatalf("the refused mutation reached disk: %q", final.Panes["w1:p1"].Note)
	}
}

// TestStoreLockSerializesConcurrentWriters guards the critical section: many
// read-modify-write cycles under the lock must not lose an update.
func TestStoreLockSerializesConcurrentWriters(t *testing.T) {
	useTempConfig(t)
	if err := saveStore(mustLoad(t)); err != nil {
		t.Fatal(err)
	}
	const writers = 32
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lk := &storeLock{}
			if err := lk.acquire(); err != nil {
				t.Error(err)
				return
			}
			defer lk.release()
			s, err := loadStore()
			if err != nil {
				t.Error(err)
				return
			}
			s.ensurePane("w1:p1").Visits++
			if err := saveStore(s); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := mustLoad(t).Panes["w1:p1"].Visits; got != writers {
		t.Fatalf("lost updates under concurrency: visits=%d, want %d", got, writers)
	}
}
