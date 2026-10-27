package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const (
	unlockPTY  = 0x40045431 // Linux TIOCSPTLCK
	ptyNumber  = 0x80045430 // Linux TIOCGPTN
	setWinsize = 0x5414     // Linux TIOCSWINSZ
)

// pickerRun drives the real picker binary on a pty: keys go to its stdin and
// every frame it paints comes back through the pty master.
type pickerRun struct {
	t      *testing.T
	master *os.File
	cmd    *exec.Cmd
	out    strings.Builder
}

// openPTY opens a pty pair sized 120 by 40. The master stays non-blocking,
// so frames can wait on read deadlines: os.File.Fd would switch it to
// blocking mode and turn the deadlines off.
func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	fd, err := syscall.Open("/dev/ptmx", syscall.O_RDWR|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	master := os.NewFile(uintptr(fd), "/dev/ptmx")
	ioctl := func(req uintptr, arg unsafe.Pointer) syscall.Errno {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
		return errno
	}
	var unlock int32
	if errno := ioctl(unlockPTY, unsafe.Pointer(&unlock)); errno != 0 {
		master.Close()
		t.Skipf("unlock pty: %v", errno)
	}
	var num uint32
	if errno := ioctl(ptyNumber, unsafe.Pointer(&num)); errno != 0 {
		master.Close()
		t.Skipf("pty number: %v", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", num), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Skipf("open pty slave: %v", err)
	}
	ws := struct{ Row, Col, X, Y uint16 }{Row: 40, Col: 120}
	if errno := ioctl(setWinsize, unsafe.Pointer(&ws)); errno != 0 {
		slave.Close()
		master.Close()
		t.Skipf("pty size: %v", errno)
	}
	return master, slave
}

const pickerSnapshot = `{"result":{"snapshot":{"focused_pane_id":"w1:pA","focused_tab_id":"tA","focused_workspace_id":"w1","panes":[{"pane_id":"w1:pA","tab_id":"tA","workspace_id":"w1","agent_status":"working","terminal_title_stripped":"alpha","cwd":"/a","focused":true},{"pane_id":"w1:pB","tab_id":"tB","workspace_id":"w1","agent_status":"idle","terminal_title_stripped":"bravo","cwd":"/b","focused":false}],"tabs":[{"tab_id":"tA","workspace_id":"w1","label":"alpha","focused":true},{"tab_id":"tB","workspace_id":"w1","label":"bravo","focused":false}],"workspaces":[{"workspace_id":"w1","label":"Work","active_tab_id":"tA","focused":true}],"layouts":[]}}}`

// herdrShim writes a herdr stand-in that answers every call with snapshot,
// and returns the PATH value that puts it first.
func herdrShim(t *testing.T, snapshot string) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + snapshot + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "herdr"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return dir + string(os.PathListSeparator) + os.Getenv("PATH")
}

func startPicker(t *testing.T, bin, cfgDir string, env ...string) *pickerRun {
	t.Helper()
	master, slave := openPTY(t)
	cmd := exec.Command(bin, "picker")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = probeEnv(append([]string{
		"HERDR_PLUGIN_CONFIG_DIR=" + cfgDir,
		"HERDR_RECALL_JEV=off",
		"PATH=" + herdrShim(t, pickerSnapshot),
	}, env...)...)
	if err := cmd.Start(); err != nil {
		slave.Close()
		master.Close()
		t.Fatal(err)
	}
	slave.Close()
	p := &pickerRun{t: t, master: master, cmd: cmd}
	t.Cleanup(func() {
		_, _ = p.master.WriteString("\x03q")
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = master.Close()
	})
	return p
}

func (p *pickerRun) press(keys string) {
	p.t.Helper()
	if _, err := p.master.WriteString(keys); err != nil {
		p.t.Fatalf("pty write: %v", err)
	}
}

// frames returns the picker output once n full-screen frames are painted and
// the picker has gone quiet. A frame can reach the pty master over several
// reads, so the n-th frame is whole only once no more output follows it.
func (p *pickerRun) frames(n int) string {
	p.t.Helper()
	buf := make([]byte, 4096)
	deadline := time.Now().Add(20 * time.Second)
	for {
		painted := strings.Count(p.out.String(), "\x1b[2J\x1b[H") >= n
		if time.Now().After(deadline) {
			if painted {
				return p.out.String()
			}
			p.t.Fatalf("picker painted %d of %d frames; output so far:\n%s",
				strings.Count(p.out.String(), "\x1b[2J\x1b[H"), n, stripANSI(p.out.String()))
		}
		wait := 500 * time.Millisecond
		if painted {
			wait = 100 * time.Millisecond
		}
		_ = p.master.SetReadDeadline(time.Now().Add(wait))
		nread, err := p.master.Read(buf)
		if nread > 0 {
			p.out.Write(buf[:nread])
			continue
		}
		if err != nil && !os.IsTimeout(err) {
			p.t.Fatalf("pty read: %v", err)
		}
		if painted {
			return p.out.String()
		}
	}
}

// TestVanishedAnchorNeverSubstitutes pins the ruled refusal: when the pane the
// drawn cursor pointed at leaves the list between the frame and the keypress,
// the row action is ignored with a visible note and the neighbor row is
// untouched. Movement keys still clamp; they are not row actions.
func TestVanishedAnchorNeverSubstitutes(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, tc := range []struct {
		name string
		pre  []string
		key  string
	}{
		{"m", nil, "m"},
		{"e", nil, "e"},
		{"block", nil, "!"},
		{"note", nil, "n"},
		{"remind", nil, "h"},
		{"teach", nil, "i"},
		{"enter", nil, "\r"},
		{"search enter", []string{"/", "\x0e"}, "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
			seed := mustLoad(t)
			seed.TabsInit = true
			seed.Panes["w1:pA"] = &paneStat{Visits: 1, Label: "one"}
			seed.Panes["w1:pB"] = &paneStat{Visits: 2, Label: "two"}
			seed.Favorites = []string{"w1:pA", "w1:pB"}
			seed.ActiveTabSet = true
			seed.ActiveTab = 0
			seed.ActivePaneID = "w1:pB"
			if err := saveStore(seed); err != nil {
				t.Fatal(err)
			}

			p := startPicker(t, bin, cfg)
			frame := 1
			p.frames(frame)
			for _, k := range tc.pre {
				p.press(k)
				frame++
				p.frames(frame)
			}

			// Another terminal unfavorites w1:pB: the pointed row leaves the list.
			ext := mustLoad(t)
			ext.Favorites = []string{"w1:pA"}
			writeStoreDirect(t, ext)
			before := mustLoad(t)

			p.press(tc.key)
			frame++
			out := p.frames(frame)
			if !strings.Contains(out, "list changed - review and retry") {
				t.Fatalf("key %q acted without naming the cause:\n%s", tc.key, stripANSI(out))
			}

			after := mustLoad(t)
			if after.Panes["w1:pA"] == nil || *after.Panes["w1:pA"] != *before.Panes["w1:pA"] {
				t.Fatalf("key %q touched the neighbor row: %+v -> %+v", tc.key, before.Panes["w1:pA"], after.Panes["w1:pA"])
			}
			if !slices.Equal(after.Favorites, before.Favorites) {
				t.Fatalf("key %q changed favorites: %v -> %v", tc.key, before.Favorites, after.Favorites)
			}
			if !slices.Equal(after.InboxOrder, before.InboxOrder) {
				t.Fatalf("key %q changed the inbox: %v -> %v", tc.key, before.InboxOrder, after.InboxOrder)
			}
		})
	}
}

// TestAppearedRowNeverTakesKey pins the other direction of the same rule: a
// row another writer added after the frame was drawn was never in front of
// the operator, so no mutation key acts on it. The refusal names the cause
// with the same note the vanish direction shows.
func TestAppearedRowNeverTakesKey(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, tc := range []struct {
		name string
		pre  []string
		key  string
	}{
		{"m", nil, "m"},
		{"e", nil, "e"},
		{"block", nil, "!"},
		{"note", nil, "n"},
		{"remind", nil, "h"},
		{"teach", nil, "i"},
		{"enter", nil, "\r"},
		{"search enter", []string{"/", "\x0e"}, "\r"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
			seed := mustLoad(t)
			seed.TabsInit = true
			seed.Panes["w1:pA"] = &paneStat{Visits: 1, Label: "one"}
			seed.Panes["w1:pB"] = &paneStat{Visits: 2, Label: "two"}
			seed.Favorites = []string{}
			seed.ActiveTabSet = true
			seed.ActiveTab = 0
			if err := saveStore(seed); err != nil {
				t.Fatal(err)
			}

			p := startPicker(t, bin, cfg)
			frame := 1
			p.frames(frame)
			for _, k := range tc.pre {
				p.press(k)
				frame++
				p.frames(frame)
			}

			// Another terminal favorites w1:pB: a row appears after the frame.
			ext := mustLoad(t)
			ext.Favorites = []string{"w1:pB"}
			writeStoreDirect(t, ext)

			p.press(tc.key)
			frame++
			out := p.frames(frame)
			if !strings.Contains(out, "list changed - review and retry") {
				t.Fatalf("key %q acted without naming the cause:\n%s", tc.key, stripANSI(out))
			}

			after := mustLoad(t)
			if !slices.Equal(after.Favorites, []string{"w1:pB"}) {
				t.Fatalf("key %q changed favorites: %v", tc.key, after.Favorites)
			}
			if *after.Panes["w1:pA"] != (paneStat{Visits: 1, Label: "one"}) {
				t.Fatalf("key %q touched the neighbor row: %+v", tc.key, after.Panes["w1:pA"])
			}
			if *after.Panes["w1:pB"] != (paneStat{Visits: 2, Label: "two"}) {
				t.Fatalf("key %q mutated the appeared row: %+v", tc.key, after.Panes["w1:pB"])
			}
		})
	}
}

// TestJumpLeavesHookWritersUnstalled pins the flock-free jump: the picker
// drops recall.lock before it asks herdr to focus the pane, so a focus RPC
// that never answers leaves the focus hooks free to record visits.
func TestJumpLeavesHookWritersUnstalled(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, tc := range []struct {
		name string
		pre  []string
	}{
		{"enter", nil},
		{"search enter", []string{"/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
			seed := mustLoad(t)
			seed.TabsInit = true
			seed.Panes["w1:pA"] = &paneStat{Visits: 1, Label: "one"}
			seed.Panes["w1:pB"] = &paneStat{Visits: 2, Label: "two"}
			seed.Favorites = []string{"w1:pA", "w1:pB"}
			seed.ActiveTabSet = true
			seed.ActiveTab = 0
			seed.ActivePaneID = "w1:pB"
			if err := saveStore(seed); err != nil {
				t.Fatal(err)
			}

			// herdr's socket takes the focus request and never answers.
			sock := filepath.Join(t.TempDir(), "herdr.sock")
			ln, err := net.Listen("unix", sock)
			if err != nil {
				t.Skipf("unix socket: %v", err)
			}
			defer ln.Close()
			rpc := make(chan net.Conn, 1)
			go func() {
				if c, err := ln.Accept(); err == nil {
					rpc <- c
				}
			}()

			p := startPicker(t, bin, cfg, "HERDR_SOCKET_PATH="+sock)
			frame := 1
			p.frames(frame)
			for _, k := range tc.pre {
				p.press(k)
				frame++
				p.frames(frame)
			}
			p.press("\r")
			var conn net.Conn
			select {
			case conn = <-rpc:
			case <-time.After(20 * time.Second):
				t.Fatalf("the picker never sent the focus request:\n%s", stripANSI(p.out.String()))
			}
			defer conn.Close()
			req, err := bufio.NewReader(conn).ReadString('\n')
			if err != nil || !strings.Contains(req, "pane.focus") {
				t.Fatalf("focus request = %q, %v", req, err)
			}

			// A focus hook fires while the jump waits on herdr.
			hook := exec.Command(bin, "record")
			hook.Env = probeEnv("HERDR_PLUGIN_CONFIG_DIR="+cfg, "PATH="+herdrShim(t, pickerSnapshot))
			var out strings.Builder
			hook.Stdout, hook.Stderr = &out, &out
			if err := hook.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- hook.Wait() }()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("the focus hook failed: %v: %s", err, out.String())
				}
			case <-time.After(10 * time.Second):
				_ = hook.Process.Kill()
				<-done
				t.Fatal("the focus hook stalled behind the wedged jump")
			}
			if got := mustLoad(t).Panes["w1:pA"].Visits; got != 2 {
				t.Fatalf("the hook visit did not land: visits=%d", got)
			}
		})
	}
}

// TestPickerPaintsAtPtySize pins the pty harness: openPTY sizes the slave to
// 120 by 40, so the picker paints rules 119 columns wide and caps a long
// list at 39 lines, not the 62 by 12 fallback termSize returns when the pty
// carries no size.
func TestPickerPaintsAtPtySize(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	useTempConfig(t)
	cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	seed := mustLoad(t)
	seed.TabsInit = true
	var panes []string
	for i := range 60 {
		id := fmt.Sprintf("w1:p%d", i)
		seed.Favorites = append(seed.Favorites, id)
		panes = append(panes, fmt.Sprintf(`{"pane_id":%q,"tab_id":"tA","workspace_id":"w1","agent_status":"idle","terminal_title_stripped":"pane %d","cwd":"/p"}`, id, i))
	}
	seed.ActiveTabSet = true
	seed.ActiveTab = 0
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
	snapshot := `{"result":{"snapshot":{"focused_pane_id":"w1:p0","focused_tab_id":"tA","focused_workspace_id":"w1","panes":[` + strings.Join(panes, ",") + `],"tabs":[{"tab_id":"tA","workspace_id":"w1","label":"alpha","focused":true}],"workspaces":[{"workspace_id":"w1","label":"Work","active_tab_id":"tA","focused":true}],"layouts":[]}}}`

	p := startPicker(t, bin, cfg, "PATH="+herdrShim(t, snapshot))
	_, frame, _ := strings.Cut(p.frames(1), "\x1b[2J\x1b[H")
	lines := strings.Split(stripANSI(frame), "\r\n")
	if len(lines) != 39 || !slices.Contains(lines, strings.Repeat("─", 119)) {
		t.Fatalf("the picker must paint at 120 by 40: a 119-column rule and 39 lines, got %d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// TestEnterOnEmptyListStillCloses pins the base behavior the row guard must
// not swallow: with no row in the last frame and none under the cursor now,
// enter still closes the picker.
func TestEnterOnEmptyListStillCloses(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	useTempConfig(t)
	cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.Panes["w1:pA"] = &paneStat{Visits: 1, Label: "one"}
	seed.Favorites = []string{}
	seed.ActiveTabSet = true
	seed.ActiveTab = 0
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	p := startPicker(t, bin, cfg)
	p.frames(1)
	p.press("\r")
	done := make(chan struct{})
	go func() {
		_ = p.cmd.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("enter on an empty list must close the picker")
	}
}

// seedPicker saves panes w1:pA and w1:pB with the given favorites, so the
// picker opens on the Favorites view with the cursor on w1:pA. edit adjusts
// the store before the save.
func seedPicker(t *testing.T, favorites []string, edit func(s *store)) {
	t.Helper()
	seed := mustLoad(t)
	seed.Version = 2
	seed.TabsInit = true
	seed.Panes["w1:pA"] = &paneStat{Visits: 1, Label: "one"}
	seed.Panes["w1:pB"] = &paneStat{Visits: 2, Label: "two"}
	seed.Favorites = favorites
	seed.ActiveTabSet = true
	seed.ActiveTab = 0
	seed.ActivePaneID = "w1:pA"
	if edit != nil {
		edit(seed)
	}
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}
}

// TestNoteCommitLabelsCapturedPane pins matrix row T5 through the real
// picker: the note commit names and writes the pane captured when n was
// pressed, even when a resync before the commit empties the list or puts
// another row under the cursor.
func TestNoteCommitLabelsCapturedPane(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, tc := range []struct {
		name      string
		favorites []string
		after     []string
	}{
		{"list emptied", []string{"w1:pA"}, []string{}},
		{"neighbor under the cursor", []string{"w1:pA", "w1:pB"}, []string{"w1:pB"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
			seedPicker(t, tc.favorites, nil)

			p := startPicker(t, bin, cfg)
			frame := 1
			p.frames(frame)
			p.press("n")
			frame++
			p.frames(frame)

			// Another terminal un-favorites the pane while its note is open.
			ext := readStoreDirect(t)
			ext.Favorites = tc.after
			writeStoreDirect(t, ext)

			p.press("hello")
			frame += len("hello")
			p.frames(frame)
			p.press("\r")
			frame++
			if out := p.frames(frame); !strings.Contains(out, "note saved: alpha") {
				t.Fatalf("the commit must name the pane captured at n:\n%s", stripANSI(out))
			}
			final := mustLoad(t)
			if final.Panes["w1:pA"].Note != "hello" {
				t.Fatalf("the note must land on the captured pane: %+v", final.Panes["w1:pA"])
			}
			if final.Panes["w1:pB"].Note != "" {
				t.Fatalf("the note landed on the row under the cursor: %+v", final.Panes["w1:pB"])
			}
		})
	}
}

// TestRefusalNoteOutlivesOverlayExit pins matrix row R3: a save refused
// while an overlay is up never reaches the overlay's frames, which carry no
// status line, and leaving the overlay must not discard the note, whether a
// hint overwrites it first or the exit clears the status. The next frame
// that carries a status line shows it.
func TestRefusalNoteOutlivesOverlayExit(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	for _, tc := range []struct {
		name  string
		open  string
		churn string
		leave []string
	}{
		{"remind", "h", "k", []string{"q"}},
		{"jev", "J", "j", []string{"i", "q"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTempConfig(t)
			cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
			seedPicker(t, []string{"w1:pA"}, nil)

			p := startPicker(t, bin, cfg)
			frame := 1
			p.frames(frame)
			p.press(tc.open)
			frame++
			p.frames(frame)

			// The save after the next key fails while the overlay is up.
			if err := os.Mkdir(statePath()+".tmp", 0o700); err != nil {
				t.Fatal(err)
			}
			p.press(tc.churn)
			frame++
			if out := p.frames(frame); strings.Contains(out, "not saved: ") {
				t.Fatalf("the overlay frame must carry no status line:\n%s", stripANSI(out))
			}
			if err := os.Remove(statePath() + ".tmp"); err != nil {
				t.Fatal(err)
			}

			for _, k := range tc.leave {
				p.press(k)
				frame++
			}
			if out := p.frames(frame); !strings.Contains(out, "not saved: ") {
				t.Fatalf("leaving the overlay discarded the unseen refusal note:\n%s", stripANSI(out))
			}
		})
	}
}

// TestOverlayActionsActOnDrawnItem pins the last-frame target rule on the
// overlay lists: a Jev delete, a quick pick removal, a tab delete, and a tab
// revise act only on the item the last frame showed. When another writer
// shifts that list under the cursor, the action is ignored, the list stays as
// the writer left it, and the unified note shows on the next frame that
// carries a status line.
func TestOverlayActionsActOnDrawnItem(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	withTabs := func(s *store) {
		s.Tabs = []customTab{{Name: "Work", Sort: "last", Limit: 50}, {Name: "Play", Sort: "last", Limit: 50}}
		s.ActiveTab = 3
	}
	insertTab := func(s *store) {
		s.Tabs = append([]customTab{{Name: "Alpha", Sort: "last", Limit: 50}}, s.Tabs...)
	}
	tabNamed := func(s *store, name string) *customTab {
		for i := range s.Tabs {
			if s.Tabs[i].Name == name {
				return &s.Tabs[i]
			}
		}
		return nil
	}
	for _, tc := range []struct {
		name  string
		seed  func(s *store)
		reach []string
		shift func(s *store)
		act   []string
		acted func(s *store) bool
		list  func(s *store) any
	}{
		{"jev delete",
			func(s *store) {
				s.ensureJev()
				s.Jev.Weights = map[string]float64{"tok:alpha": 0.9, "tok:bravo": 0.5, "tok:charlie": 0.3}
			},
			[]string{"J", "j"},
			func(s *store) { delete(s.Jev.Weights, "tok:alpha") },
			[]string{"\r", "q"},
			func(s *store) bool {
				_, bravo := s.Jev.Weights["tok:bravo"]
				return !bravo && len(s.Jev.Weights) == 2
			},
			func(s *store) any { return s.Jev.Weights }},
		{"quick pick removal",
			nil,
			[]string{"h", "l"},
			func(s *store) { s.SnoozePicks = []string{"1h", "3h", "tomorrow", "clear"} },
			[]string{"x", "q"},
			func(s *store) bool {
				return slices.Equal(s.getSnoozePicks(), []string{"20min", "3h", "tomorrow", "clear"})
			},
			func(s *store) any { return s.getSnoozePicks() }},
		{"tab delete",
			withTabs,
			[]string{"t", "x", "h"},
			insertTab,
			[]string{"\r"},
			func(s *store) bool { return tabNamed(s, "Work") == nil && tabNamed(s, "Play") != nil },
			func(s *store) any { return s.Tabs }},
		{"tab revise",
			withTabs,
			[]string{"t", "r", "l"},
			insertTab,
			[]string{"\r"},
			func(s *store) bool { w := tabNamed(s, "Work"); return w != nil && w.Sort != "last" },
			func(s *store) any { return s.Tabs }},
	} {
		for _, shifted := range []bool{false, true} {
			name := tc.name + "/item unchanged"
			if shifted {
				name = tc.name + "/item shifted away"
			}
			t.Run(name, func(t *testing.T) {
				useTempConfig(t)
				cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
				seedPicker(t, []string{"w1:pA"}, tc.seed)

				p := startPicker(t, bin, cfg)
				frame := 1
				p.frames(frame)
				for _, k := range tc.reach {
					p.press(k)
					frame++
					p.frames(frame)
				}
				var want any
				if shifted {
					ext := readStoreDirect(t)
					tc.shift(ext)
					writeStoreDirect(t, ext)
					want = tc.list(ext)
				}
				for _, k := range tc.act {
					p.press(k)
					frame++
				}
				out := p.frames(frame)
				final := mustLoad(t)
				noted := strings.Contains(out, "list changed - review and retry")
				if !shifted {
					if noted || !tc.acted(final) {
						t.Fatalf("the action must land on the drawn item: noted=%v %+v\n%s", noted, tc.list(final), stripANSI(out))
					}
					return
				}
				if !noted {
					t.Fatalf("the ignored action must show the note:\n%s", stripANSI(out))
				}
				if got := tc.list(final); !reflect.DeepEqual(got, want) {
					t.Fatalf("the action touched the shifted list: got %+v, want %+v", got, want)
				}
			})
		}
	}
}

// TestBrowseRefusalRendersAfterOverlayNote pins one tracking path for the
// unified note: after an overlay guard note has shown and cleared, a browse
// refusal with the same text still renders instead of being cleared as the
// old note.
func TestBrowseRefusalRendersAfterOverlayNote(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	useTempConfig(t)
	cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	seedPicker(t, []string{"w1:pA", "w1:pB"}, func(s *store) {
		s.ensureJev()
		s.Jev.Weights = map[string]float64{"tok:alpha": 0.9, "tok:bravo": 0.5, "tok:charlie": 0.3}
	})
	const changed = "list changed - review and retry"

	p := startPicker(t, bin, cfg)
	frame := 1
	p.frames(frame)
	for _, k := range []string{"J", "j"} {
		p.press(k)
		frame++
		p.frames(frame)
	}

	// The Jev list shifts under the cursor, so enter is ignored with the note,
	// and the browse frame shows it once the panel closes.
	ext := readStoreDirect(t)
	delete(ext.Jev.Weights, "tok:alpha")
	writeStoreDirect(t, ext)
	for _, k := range []string{"\r", "q"} {
		p.press(k)
		frame++
	}
	out := p.frames(frame)
	if !strings.Contains(out, changed) {
		t.Fatalf("the overlay note must show once the panel closes:\n%s", stripANSI(out))
	}
	shown := len(out)

	// The next key's save clears the shown note.
	p.press("j")
	frame++
	out = p.frames(frame)
	if strings.Contains(out[shown:], changed) {
		t.Fatalf("the shown note must clear after the next save:\n%s", stripANSI(out[shown:]))
	}
	cleared := len(out)

	// Another terminal un-favorites the row under the cursor, so m is refused.
	ext = readStoreDirect(t)
	ext.Favorites = []string{"w1:pA"}
	writeStoreDirect(t, ext)
	p.press("m")
	frame++
	out = p.frames(frame)
	if !strings.Contains(out[cleared:], changed) {
		t.Fatalf("the browse refusal must render its note:\n%s", stripANSI(out[cleared:]))
	}
	if final := mustLoad(t); !slices.Equal(final.Favorites, []string{"w1:pA"}) {
		t.Fatalf("m acted on a row the last frame did not show: %v", final.Favorites)
	}
}
