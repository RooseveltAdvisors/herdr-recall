package main

import (
	"encoding/json"
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

// TestWriterFallsBackWhenConfigDirUnresolvable pins the ruled fallback: a
// failing herdr CLI resolution must not drop a capture, the writer resolves
// the deterministic encoded directory and saves there.
func TestWriterFallsBackWhenConfigDirUnresolvable(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	home := t.TempDir()
	enc := strings.ReplaceAll(pluginID, "R", "%52")
	enc = strings.ReplaceAll(enc, "A", "%41")
	state := filepath.Join(home, ".config", "herdr", "plugins", "config", enc, "recall.json")
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "herdr"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "favorite", "w1:p2")
	cmd.Env = probeEnv("HOME="+home, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("favorite must capture despite the failing config-dir CLI: %v: %s", err, out)
	}
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("the fallback state file: %v", err)
	}
	var st store
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if !st.isFavorite("w1:p2") {
		t.Fatal("the capture did not land at the fallback state file")
	}
}
