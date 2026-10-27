package main

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

const (
	unlockPTY = 0x40045431 // Linux TIOCSPTLCK
	ptyNumber = 0x80045430 // Linux TIOCGPTN
)

// pickerRun drives the real picker binary on a pty: keys go to its stdin and
// every frame it paints comes back through the pty master.
type pickerRun struct {
	t      *testing.T
	master *os.File
	cmd    *exec.Cmd
	out    strings.Builder
}

func openPTY(t *testing.T) (*os.File, *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("no /dev/ptmx: %v", err)
	}
	var unlock int32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), unlockPTY, uintptr(unsafe.Pointer(&unlock))); errno != 0 {
		master.Close()
		t.Skipf("unlock pty: %v", errno)
	}
	var num uint32
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), ptyNumber, uintptr(unsafe.Pointer(&num))); errno != 0 {
		master.Close()
		t.Skipf("pty number: %v", errno)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", num), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		master.Close()
		t.Skipf("open pty slave: %v", err)
	}
	ws := struct{ Row, Col, X, Y uint16 }{Row: 40, Col: 120}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), uintptr(tiocgwinsz), uintptr(unsafe.Pointer(&ws))); errno != 0 {
		slave.Close()
		master.Close()
		t.Skipf("pty size: %v", errno)
	}
	return master, slave
}

const pickerSnapshot = `{"result":{"snapshot":{"focused_pane_id":"w1:pA","focused_tab_id":"tA","focused_workspace_id":"w1","panes":[{"pane_id":"w1:pA","tab_id":"tA","workspace_id":"w1","agent_status":"working","terminal_title_stripped":"alpha","cwd":"/a","focused":true},{"pane_id":"w1:pB","tab_id":"tB","workspace_id":"w1","agent_status":"idle","terminal_title_stripped":"bravo","cwd":"/b","focused":false}],"tabs":[{"tab_id":"tA","workspace_id":"w1","label":"alpha","focused":true},{"tab_id":"tB","workspace_id":"w1","label":"bravo","focused":false}],"workspaces":[{"workspace_id":"w1","label":"Work","active_tab_id":"tA","focused":true}],"layouts":[]}}}`

// herdrShim writes a herdr stand-in that answers every call with the fixed
// snapshot, and returns the PATH value that puts it first.
func herdrShim(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\necho '" + pickerSnapshot + "'\n"
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
		"PATH=" + herdrShim(t),
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

// frames returns the picker output once n full-screen frames are painted.
func (p *pickerRun) frames(n int) string {
	p.t.Helper()
	buf := make([]byte, 4096)
	deadline := time.Now().Add(20 * time.Second)
	for strings.Count(p.out.String(), "\x1b[2J\x1b[H") < n {
		if time.Now().After(deadline) {
			p.t.Fatalf("picker painted %d of %d frames; output so far:\n%s",
				strings.Count(p.out.String(), "\x1b[2J\x1b[H"), n, stripANSI(p.out.String()))
		}
		_ = p.master.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		nread, err := p.master.Read(buf)
		if nread > 0 {
			p.out.Write(buf[:nread])
		}
		if err != nil && !os.IsTimeout(err) {
			p.t.Fatalf("pty read: %v", err)
		}
	}
	return p.out.String()
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
			hook.Env = probeEnv("HERDR_PLUGIN_CONFIG_DIR="+cfg, "PATH="+herdrShim(t))
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
