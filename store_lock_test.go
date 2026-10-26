package main

import (
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// useTempConfig isolates recall state in a temp dir and resets the process
// stamp, so tests never touch the live plugin config directory.
func useTempConfig(t *testing.T) {
	t.Helper()
	t.Setenv("HERDR_PLUGIN_CONFIG_DIR", t.TempDir())
	lastStoreStamp.Store(0)
}

// writeStoreDirect simulates a write from another process: it replaces the
// state file without updating this process's lastStoreStamp, and bumps the
// modtime so the change is visible even on a coarse filesystem clock.
func writeStoreDirect(t *testing.T, s *store) {
	t.Helper()
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath(), b, 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(statePath(), later, later); err != nil {
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
