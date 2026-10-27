package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
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
// fingerprint, so tests never touch the live plugin config directory.
func useTempConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	configDirOnce = sync.Once{}
	lastStoreFP.Store("")
	loadFailed.Store(false)
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

// TestPendingChangeRetriesOrDiscards pins the pending contract: a refused
// change stays in memory for the automatic retry and drops only when the
// operator discards it, without writing.
func TestPendingChangeRetriesOrDiscards(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	w := &storeWriter{lk: lk}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"

	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.persist(st); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	if !w.pending() {
		t.Fatal("the refused change must be pending")
	}

	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	if err := w.persist(st); err != nil {
		t.Fatalf("automatic retry: %v", err)
	}
	if w.pending() {
		t.Fatal("pending must clear once the change is saved")
	}
	if got := mustLoad(t).Panes["w1:p1"].Note; got != "typed" {
		t.Fatalf("the pending change must persist on retry: note=%q", got)
	}

	lk.release()
	st2 := mustLoad(t)
	st2.Panes["w1:p1"].Note = "second"
	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.persist(st2); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	dropped, err := w.discard(st2)
	if err != nil {
		t.Fatalf("discard: %v", err)
	}
	if dropped.Panes["w1:p1"].Note != "typed" {
		t.Fatalf("the discard must drop the pending change: note=%q", dropped.Panes["w1:p1"].Note)
	}
	if got := mustLoad(t).Panes["w1:p1"].Note; got != "typed" {
		t.Fatalf("the discard must not write: note=%q", got)
	}
}

// TestAdoptionRefusesSameUnitWrite pins the conflict rule: an external write
// on a unit the operator changed refuses the adoption, the pending change
// wins, and the pending note stays.
func TestAdoptionRefusesSameUnitWrite(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Note: "old"}
	seed.Panes["w1:p2"] = &paneStat{Visits: 1, Label: "two"}
	seed.Favorites = []string{"w1:p2"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	w := &storeWriter{lk: lk}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := w.persist(st); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}

	hook := mustLoad(t)
	hook.Panes["w1:p1"].Visits++
	hook.Panes["w1:p2"].Visits += 5
	writeStoreDirect(t, hook)

	base := syncedFields()
	kept := w.adopt(st, refreshStore(st), base)
	if kept.Panes["w1:p1"].Note != "typed" {
		t.Fatalf("the pending change was lost: %q", kept.Panes["w1:p1"].Note)
	}
	if kept.Panes["w1:p2"].Visits != 1 {
		t.Fatalf("the refused adoption still took the sync: %d", kept.Panes["w1:p2"].Visits)
	}
	if w.note() == "" {
		t.Fatal("the refused adoption must keep the pending note")
	}

	st2 := mustLoad(t)
	st2.Favorites = []string{"w1:p2", "w1:p1"}
	if err := w.persist(st2); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	hook2 := mustLoad(t)
	hook2.Favorites = []string{"w1:p2"}
	writeStoreDirect(t, hook2)
	base = syncedFields()
	kept = w.adopt(st2, refreshStore(st2), base)
	if len(kept.Favorites) != 2 {
		t.Fatalf("the pending favorites change was lost: %v", kept.Favorites)
	}
}

// TestAdoptionCarriesPendingAcrossDisjointUnits pins the reapply rule: an
// external write on other units is adopted and the pending change rides
// along wholesale.
func TestAdoptionCarriesPendingAcrossDisjointUnits(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Note: "old"}
	seed.Panes["w1:p2"] = &paneStat{Visits: 1, Label: "two"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	w := &storeWriter{lk: lk}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := w.persist(st); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}

	hook := mustLoad(t)
	hook.Panes["w1:p2"].Visits++
	writeStoreDirect(t, hook)

	base := syncedFields()
	st = w.adopt(st, refreshStore(st), base)
	if st.Panes["w1:p1"].Note != "typed" {
		t.Fatalf("the adoption dropped the pending change: %q", st.Panes["w1:p1"].Note)
	}
	if st.Panes["w1:p2"].Visits != 2 {
		t.Fatalf("the adoption dropped the external write: %d", st.Panes["w1:p2"].Visits)
	}
	if !w.pending() {
		t.Fatal("the adopted change must stay pending until it is saved")
	}
}

// TestUnitMergeKeepsOddKeys pins that the unit merge cannot corrupt the
// store: a pending Jev weight keyed with a slash survives an adoption and a
// save intact.
func TestUnitMergeKeepsOddKeys(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	seed.Panes["w1:p2"] = &paneStat{Visits: 1, Label: "two"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	w := &storeWriter{lk: lk}
	st := mustLoad(t)
	st.ensureJev()
	if st.Jev.Weights == nil {
		st.Jev.Weights = map[string]float64{}
	}
	st.Jev.Weights["label:fix/store-write-race"] = 0.5
	if err := w.persist(st); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}

	hook := mustLoad(t)
	hook.Panes["w1:p2"].Visits++
	writeStoreDirect(t, hook)

	base := syncedFields()
	st = w.adopt(st, refreshStore(st), base)
	if got := st.Jev.Weights["label:fix/store-write-race"]; got != 0.5 {
		t.Fatalf("the pending weight was corrupted: %v (%+v)", got, st.Jev.Weights)
	}
	if st.Panes["w1:p2"].Visits != 2 {
		t.Fatalf("the external write was dropped: %d", st.Panes["w1:p2"].Visits)
	}

	if err := lk.acquire(); err != nil {
		t.Fatal(err)
	}
	defer lk.release()
	if err := w.persist(st); err != nil {
		t.Fatalf("save after the adoption: %v", err)
	}
	final := mustLoad(t)
	if got := final.Jev.Weights["label:fix/store-write-race"]; got != 0.5 {
		t.Fatalf("the weight did not round-trip to disk: %v (%+v)", got, final.Jev.Weights)
	}
}

// TestPendingChangeSurvivesExit pins the exit contract: the picker does not
// exit while a pending change cannot be saved, and the exit persists it even
// when the lock is not held.
func TestPendingChangeSurvivesExit(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	w := &storeWriter{lk: lk}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := w.persist(st); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}

	if err := os.Mkdir(lockPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if w.close(st) {
		t.Fatal("the exit must be refused while the change cannot be saved")
	}
	if !w.pending() {
		t.Fatal("the refused exit must keep the change pending")
	}

	if err := os.Remove(lockPath()); err != nil {
		t.Fatal(err)
	}
	if !w.close(st) {
		t.Fatal("the exit must persist the pending change even without a held lock")
	}
	if got := mustLoad(t).Panes["w1:p1"].Note; got != "typed" {
		t.Fatalf("the pending change was lost at exit: %q", got)
	}
}

// TestDiscardFailureKeepsPendingChange pins the discard contract: a discard
// that cannot reload drops nothing and keeps the pending note visible.
func TestDiscardFailureKeepsPendingChange(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one"}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	lk := &storeLock{}
	w := &storeWriter{lk: lk}
	st := mustLoad(t)
	st.Panes["w1:p1"].Note = "typed"
	if err := w.persist(st); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}

	if err := os.Chmod(statePath(), 0o000); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(statePath(), 0o600)
	if _, err := os.ReadFile(statePath()); err == nil {
		t.Skip("cannot make the state file unreadable")
	}
	if _, err := w.discard(st); err == nil {
		t.Fatal("the discard must fail while the state file is unreadable")
	}
	if !w.pending() {
		t.Fatal("a failed discard must keep the change pending")
	}
	if st.Panes["w1:p1"].Note != "typed" {
		t.Fatalf("a failed discard must keep the change: %q", st.Panes["w1:p1"].Note)
	}
	if w.note() == "" {
		t.Fatal("a failed discard must keep the pending note")
	}
}

// TestPendingNoteNeverSaysSaved is the cross-check: while a change is
// pending, no rendered note claims a save happened.
func TestPendingNoteNeverSaysSaved(t *testing.T) {
	useTempConfig(t)
	lk := &storeLock{}
	w := &storeWriter{lk: lk}
	if err := w.persist(mustLoad(t)); err == nil {
		t.Fatal("the save must be refused while the lock is unavailable")
	}
	note := w.note()
	if !strings.Contains(note, "pending") {
		t.Fatalf("the note must say the change is pending: %q", note)
	}
	frame := renderFrame([]row{{PaneID: "w1:p1", Label: "one"}}, 0, 0, vimline.View{}, note, 80, 24, []string{"All"}, 0, "both", "")
	if strings.Contains(frame, "saved") {
		t.Fatalf("the rendered note claims saved while a change is pending:\n%s", frame)
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
