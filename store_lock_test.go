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
	saveTaint.Store(false)
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
// refused save the hook's visit bumps, un-Done writes, and prune deletions
// are never persisted over.
func TestRefusedSaveKeepsConcurrentWrites(t *testing.T) {
	useTempConfig(t)
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:p1"] = &paneStat{Visits: 1, Label: "one", Note: "old", Done: true}
	seed.Panes["w1:p2"] = &paneStat{Visits: 1, Label: "two"}
	seed.Panes["w1:dead"] = &paneStat{Label: "dead"}
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
	delete(hook.Panes, "w1:dead")
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

// TestNoteCommitLabelsCapturedPane pins the note commit: the status names the
// label captured when the note opened, and a resync that empties or reorders
// the list cannot panic or relabel it.
func TestNoteCommitLabelsCapturedPane(t *testing.T) {
	useTempConfig(t)
	st := mustLoad(t)

	rows := []row{{PaneID: "w1:p1", Label: "one"}}
	sel := 0
	notePaneID := rows[sel].PaneID
	noteLabel := rows[sel].Label

	rows = nil
	status := commitNote(st, notePaneID, noteLabel, "hello")
	if status != "note saved: one" {
		t.Fatalf("status must name the captured pane: %q", status)
	}
	if st.Panes["w1:p1"].Note != "hello" {
		t.Fatalf("the note must land on the captured pane: %q", st.Panes["w1:p1"].Note)
	}

	rows = []row{{PaneID: "w1:p2", Label: "two"}, {PaneID: "w1:p1", Label: "one"}}
	sel = 0
	status = commitNote(st, notePaneID, noteLabel, "")
	if status != "note cleared: one" {
		t.Fatalf("status must keep naming the captured pane: %q", status)
	}
	if st.Panes["w1:p1"].Note != "" {
		t.Fatalf("the clear must land on the captured pane: %q", st.Panes["w1:p1"].Note)
	}
}

// TestTaintedRollbackNeverPersistsRefusedMutation pins the taint contract:
// a refused change never reaches disk, the note reports the live failure
// while the reload keeps failing, and a clean reload clears the taint so the
// next persist saves only proven state.
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

	if err := os.Remove(statePath()); err != nil {
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
	if !os.IsNotExist(err) {
		t.Fatalf("the note must report the live failure: %v", err)
	}
	note := &saveNote{}
	if got := note.refuse(err); strings.Contains(got, "recall.lock unavailable") {
		t.Fatalf("the note must not replay the stale reason: %q", got)
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
