package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

var rowOfRe = regexp.MustCompile(`row (\d+) of (\d+)`)

func lastPaintedFrame(all string) string {
	const mark = "\x1b[2J\x1b[H"
	idx := strings.LastIndex(all, mark)
	if idx < 0 {
		return all
	}
	return all[idx:]
}

func parseRowOf(frame string) (row, total int, ok bool) {
	plain := stripANSI(lastPaintedFrame(frame))
	m := rowOfRe.FindStringSubmatch(plain)
	if m == nil {
		return 0, 0, false
	}
	row, _ = strconv.Atoi(m[1])
	total, _ = strconv.Atoi(m[2])
	return row, total, true
}

func waitRowStatus(t *testing.T, p *pickerRun, frame *int, wantMinTotal int) (row, total int) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		*frame++
		out := p.frames(*frame)
		r, tot, ok := parseRowOf(out)
		if ok && tot >= wantMinTotal {
			return r, tot
		}
		// allow async search worker to finish
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("never got row status with total>=%d; last:\n%s", wantMinTotal, stripANSI(lastPaintedFrame(p.out.String())))
	return 0, 0
}

// TestTranscriptNavigationAndSelectionE2E drives the real picker on a pty
// against the live speaker index and checks selection moves.
func TestTranscriptNavigationAndSelectionE2E(t *testing.T) {
	indexCandidates, _ := filepath.Glob(filepath.Join(os.Getenv("HOME"), ".config/herdr/plugins/config/*recall*/transcripts.db"))
	if len(indexCandidates) == 0 {
		t.Skip("no transcripts.db index")
	}
	index := indexCandidates[0]
	if st, err := os.Stat(index); err != nil || st.Size() < 1000 {
		t.Skip("transcripts index not ready")
	}

	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	useTempConfig(t)
	cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.ActiveTabSet = true
	seed.ActiveTab = 1 // Inbox; jump to Transcripts with g t
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	p := startPicker(t, bin, cfg,
		"HERDR_RECALL_TRANSCRIPTS=1",
		"HERDR_RECALL_INDEX="+index,
		"HERDR_RECALL_JEV=off",
	)
	frame := 1
	p.frames(frame)
	// g then t jumps to Transcripts (chord dispatcher)
	p.press("g")
	frame++
	p.frames(frame)
	p.press("t")
	frame++
	out := p.frames(frame)
	plain := stripANSI(out)
	if !strings.Contains(plain, "Transcripts") || !strings.Contains(plain, "Typed") {
		t.Fatalf("Transcripts tab not open after g t:\n%s", plain)
	}

	// open search and type a common word with many hits
	p.press("/")
	frame++
	p.frames(frame)
	p.press("portal")
	frame++
	p.frames(frame)
	// wait for async search
	row, total := waitRowStatus(t, p, &frame, 10)
	if row != 1 {
		t.Fatalf("after search want row 1, got %d of %d", row, total)
	}

	// one Esc focuses list
	p.press("\x1b")
	frame++
	p.frames(frame)
	row, total = waitRowStatus(t, p, &frame, 10)
	if row != 1 {
		t.Fatalf("after esc want row 1, got %d of %d", row, total)
	}

	waitRowChange := func(afterKey string, pred func(r int) bool) (int, int) {
		t.Helper()
		p.press(afterKey)
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			frame++
			out := p.frames(frame)
			r, tot, ok := parseRowOf(out)
			if ok && tot >= 10 && pred(r) {
				return r, tot
			}
			time.Sleep(50 * time.Millisecond)
		}
		r, tot, _ := parseRowOf(p.out.String())
		t.Fatalf("after key %q row did not match; got %d of %d\n%s", afterKey, r, tot, stripANSI(lastPaintedFrame(p.out.String())))
		return 0, 0
	}

	// j moves down one
	_, total2 := waitRowChange("j", func(r int) bool { return r == 2 })
	if total2 != total {
		t.Fatalf("total changed on j: %d -> %d", total, total2)
	}

	// k moves up one
	waitRowChange("k", func(r int) bool { return r == 1 })

	// down arrow
	row4, _ := waitRowChange("\x1b[B", func(r int) bool { return r == 2 })

	// ctrl-d half page should move selection by more than 1
	before := row4
	row5, total5 := waitRowChange("\x04", func(r int) bool { return r > before })
	if row5-before < 2 && total5 > 10 {
		t.Fatalf("ctrl-d moved only %d rows (want half-page >=2 when many hits)", row5-before)
	}

	// ctrl-u half page up
	waitRowChange("\x15", func(r int) bool { return r < row5 })

	// while typing, ctrl-n should move selection
	p.press("/")
	frame++
	p.frames(frame)
	before = 0
	if r, _, ok := parseRowOf(p.out.String()); ok {
		before = r
	}
	row7, _ := waitRowChange("\x0e", func(r int) bool { return r != before || r >= 1 })
	if row7 < 1 {
		t.Fatalf("ctrl-n while typing failed, row=%d", row7)
	}
}
