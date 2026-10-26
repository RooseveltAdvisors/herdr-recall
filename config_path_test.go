package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestPathProbe prints the resolved store paths for the parent tests, which
// spawn this binary as a child process.
func TestPathProbe(t *testing.T) {
	if os.Getenv("RECALL_PATHS_PROBE") == "" {
		t.Skip("probe helper for the child-process path tests")
	}
	fmt.Println("lock:", lockPath())
	fmt.Println("state:", statePath())
}

func probeEnv(overrides ...string) []string {
	drop := map[string]bool{"HERDR_PLUGIN_CONFIG_DIR": true}
	for _, kv := range overrides {
		if k, _, ok := strings.Cut(kv, "="); ok {
			drop[k] = true
		}
	}
	out := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if drop[k] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, overrides...)
}

func runPathProbe(t *testing.T, env []string) (string, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestPathProbe$")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("probe: %v: %s", err, out)
	}
	lock, state := "", ""
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "lock: "); ok {
			lock = rest
		}
		if rest, ok := strings.CutPrefix(line, "state: "); ok {
			state = rest
		}
	}
	if lock == "" || state == "" {
		t.Fatalf("probe output missing paths: %s", out)
	}
	return lock, state
}

// TestLockAndStatePathShareConfigDir pins the one-directory contract: the
// lock and the state file resolve into the same config directory.
func TestLockAndStatePathShareConfigDir(t *testing.T) {
	useTempConfig(t)
	if filepath.Dir(lockPath()) != filepath.Dir(statePath()) {
		t.Fatalf("lockPath %q and statePath %q must resolve into one directory", lockPath(), statePath())
	}
}

// TestConfigDirStableWhenHerdrFlaps pins the per-process resolution: a herdr
// CLI that answers once and then fails must not split one process across two
// config directories.
func TestConfigDirStableWhenHerdrFlaps(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "herdr")
	marker := filepath.Join(dir, "used")
	real := filepath.Join(dir, "real-config")
	script := "#!/bin/sh\nif [ -e " + marker + " ]; then\n\texit 1\nfi\n: > " + marker + "\necho " + real + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	lock, state := runPathProbe(t, probeEnv(
		"RECALL_PATHS_PROBE=1",
		"PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"),
	))
	if filepath.Dir(lock) != real || filepath.Dir(state) != real {
		t.Fatalf("a flapping herdr CLI split the paths: lock %q state %q want dir %q", lock, state, real)
	}
}

// TestFallbackResolutionIdenticalAcrossProcesses pins the deterministic
// fallback: two separate processes without the herdr CLI resolve the same
// lock and state paths.
func TestFallbackResolutionIdenticalAcrossProcesses(t *testing.T) {
	empty := t.TempDir()
	env := probeEnv("RECALL_PATHS_PROBE=1", "PATH="+empty)
	lock1, state1 := runPathProbe(t, env)
	lock2, state2 := runPathProbe(t, env)
	if lock1 != lock2 || state1 != state2 {
		t.Fatalf("fallback resolution differs across processes: %q %q vs %q %q", lock1, state1, lock2, state2)
	}
}
