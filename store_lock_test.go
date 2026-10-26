package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"sync"
	"testing"
	"time"
)

// useTempConfig isolates recall state in a temp dir and resets the process
// fingerprint, so tests never touch the live plugin config directory.
func useTempConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	lastStoreFP.Store("")
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

func TestRefreshStoreKeepsStoreWhenNoExternalWrite(t *testing.T) {
	useTempConfig(t)
	st := loadStore()
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := loadStore()
	if fresh := refreshStore(held); fresh != held {
		t.Fatal("refresh with no external write must keep the in-memory store")
	}
}

func TestRefreshStoreAdoptsExternalWrites(t *testing.T) {
	useTempConfig(t)
	st := loadStore()
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := loadStore() // the picker's long-lived in-memory store

	ext := loadStore()
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
	seed := loadStore()
	for i := 0; i < 250; i++ {
		seed.Panes["dead:"+string(rune(i))] = &paneStat{Label: "dead"}
	}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
	pickerStore := loadStore() // picker opens

	// A hook fires while the picker is open: records a visit and prunes.
	live := map[string]bool{"w1:p23": true}
	hook := loadStore()
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

	final := loadStore()
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
	st := loadStore()
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := loadStore()
	fi, err := os.Stat(statePath())
	if err != nil {
		t.Fatal(err)
	}
	same := fi.ModTime()

	ext := loadStore()
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
	st := loadStore()
	st.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	st.Favorites = []string{"w1:p1"}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	held := loadStore()

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
	if err := saveStore(fresh); err != nil {
		t.Fatal(err)
	}
	final := loadStore()
	if final.Panes["w1:p1"] == nil || len(final.Favorites) != 1 {
		t.Fatalf("an empty store was saved over the real one: %+v", final)
	}
}

// TestResyncKeepsSelectionOnSamePane is the wrong-pane regression: rows
// [A,B,C] with the cursor on B, an external write re-sorts to [C,A,B], and
// the applied keypress must still target B's pane ID.
func TestResyncKeepsSelectionOnSamePane(t *testing.T) {
	useTempConfig(t)
	st := loadStore()
	st.Panes["w1:pA"] = &paneStat{Label: "A", Last: 300}
	st.Panes["w1:pB"] = &paneStat{Label: "B", Last: 200}
	st.Panes["w1:pC"] = &paneStat{Label: "C", Last: 100}
	if err := saveStore(st); err != nil {
		t.Fatal(err)
	}
	frozen := &snapshot{Panes: []paneRec{
		{PaneID: "w1:pA"}, {PaneID: "w1:pB"}, {PaneID: "w1:pC"},
	}}
	held := loadStore()
	rows := rowsFor(frozen, held, view{Kind: "all"}, sortDefault)
	if len(rows) != 3 || rows[0].PaneID != "w1:pA" || rows[1].PaneID != "w1:pB" || rows[2].PaneID != "w1:pC" {
		t.Fatalf("setup order = %v, want [A B C]", rows)
	}
	sel := 1 // the cursor sits on B

	ext := loadStore()
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
	st := loadStore()
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

	held := loadStore()
	rows := rowsFor(frozen, held, view{Kind: "all"}, sortDefault)
	sel := pinSel(rows, 0, held.ActivePaneID)
	if rows[sel].PaneID != "w1:pB" {
		t.Fatalf("restore pinned %q, want %q", rows[sel].PaneID, "w1:pB")
	}

	ext := loadStore()
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

// TestCLIWriterRefusesWriteWithoutLock is the fail-closed regression: when
// recall.lock cannot be taken, jev reset and favorite must error out and
// leave recall.json byte-identical instead of writing unlocked.
func TestCLIWriterRefusesWriteWithoutLock(t *testing.T) {
	useTempConfig(t)
	seed := loadStore()
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

// TestJevResetClearsLearnings covers the ordinary locked path of the command
// that now fails closed.
func TestJevResetClearsLearnings(t *testing.T) {
	useTempConfig(t)
	seed := loadStore()
	seed.TabsInit = true
	seed.ensureJev()
	seed.Jev.Weights = map[string]float64{"label:docs": 0.5}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("go", "run", ".", "jev", "reset").CombinedOutput(); err != nil {
		t.Fatalf("jev reset: %v: %s", err, out)
	}
	if got := loadStore(); got.Jev == nil || len(got.Jev.Weights) != 0 {
		t.Fatalf("learnings not cleared: %+v", got.Jev)
	}
}

// TestStoreLockSerializesConcurrentWriters guards the critical section: many
// read-modify-write cycles under the lock must not lose an update.
func TestStoreLockSerializesConcurrentWriters(t *testing.T) {
	useTempConfig(t)
	if err := saveStore(loadStore()); err != nil {
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
			s := loadStore()
			s.ensurePane("w1:p1").Visits++
			if err := saveStore(s); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := loadStore().Panes["w1:p1"].Visits; got != writers {
		t.Fatalf("lost updates under concurrency: visits=%d, want %d", got, writers)
	}
}
