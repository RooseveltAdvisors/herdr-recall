// herdr-recall: a pane recall picker for Herdr bound to prefix+shift+l.
//
// It remembers which panes you actually used (recency and visit counts) and
// lets you mark favorites, then jumps to a pane from one overlay list.
// The look and key model copy Herdr's built-in goto overlay (prefix+k) rather
// than inventing a new visual language.
package main

import (
	"bytes"
	"encoding/json"
	"errors"

	"fmt"
	"github.com/RooseveltAdvisors/vimline"
	"github.com/RooseveltAdvisors/vimline/duration"
	"github.com/RooseveltAdvisors/vimline/keys"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const pluginID = "RooseveltAdvisors.herdr-recall"

// recentMax is the most rows the Recently used tab lists under its title.
const recentMax = 20

const (
	tabRecent    = 0
	tabMostUsed  = 1
	tabFavorites = 2
	tabAll       = 3
	tabCount     = 4
)

const (
	sortDefault = 0
	sortNewest  = 1
	sortOldest  = 2
	sortMost    = 3
	sortLeast   = 4
	sortCount   = 5
)

const (
	keyReset    = "\x1b[0m"
	keyDim      = "\x1b[2m"
	keyBold     = "\x1b[1m"
	keyAccentFg = "\x1b[38;5;81m"  // accent, used for selected-row bar, key names
	keySub      = "\x1b[38;5;246m" // subtext, kept for compatibility
	keyOverlay  = "\x1b[38;5;244m" // overlay0: hints and placeholders only
	keySelectBg = "\x1b[48;5;81m"  // selected row background
	keySelectFg = "\x1b[38;5;235m" // contrast text on accent
	keyText     = "\x1b[38;5;252m" // readable body text on the panel
	keySurface  = "\x1b[38;5;238m" // mid surface colour for separators
	keyStar     = "\x1b[38;5;221m" // warm yellow favourite star
	keySecFav   = "\x1b[38;5;221m" // FAVORITES header, warm
	keySecRec   = "\x1b[38;5;81m"  // RECENT header, accent
	keySecUsed  = "\x1b[38;5;44m"  // MOST USED header, teal
	keyStBad    = "\x1b[38;5;203m" // blocked: red
	keyStBusy   = "\x1b[38;5;221m" // working: yellow
	keyStGood   = "\x1b[38;5;44m"  // done: teal
	keyStFree   = "\x1b[38;5;114m" // idle: green
)

// ---------------------------------------------------------------------------
// herdr CLI plumbing. Every call is the same CLI a human types.
// ---------------------------------------------------------------------------

func herdr(args ...string) ([]byte, error) {
	cmd := exec.Command("herdr", args...)
	out, err := cmd.Output()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && len(ee.Stderr) > 0 {
			return out, fmt.Errorf("herdr %s: %s", strings.Join(args, " "), strings.TrimSpace(string(ee.Stderr)))
		}
		return out, fmt.Errorf("herdr %s: %w", strings.Join(args, " "), err)
	}
	return out, nil
}

type rect struct {
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

type paneRec struct {
	PaneID             string `json:"pane_id"`
	TabID              string `json:"tab_id"`
	WorkspaceID        string `json:"workspace_id"`
	AgentStatus        string `json:"agent_status"`
	Agent              string `json:"agent,omitempty"`
	TerminalTitle      string `json:"terminal_title"`
	TerminalTitleStrip string `json:"terminal_title_stripped"`
	CWD                string `json:"cwd"`
	ForegroundCWD      string `json:"foreground_cwd,omitempty"`
	Focused            bool   `json:"focused"`
}

type tabRec struct {
	TabID        string `json:"tab_id"`
	WorkspaceID  string `json:"workspace_id"`
	Label        string `json:"label"`
	Focused      bool   `json:"focused"`
	ActivePaneID string `json:"-"`
}

type wsRec struct {
	WorkspaceID string `json:"workspace_id"`
	Label       string `json:"label"`
	ActiveTabID string `json:"active_tab_id"`
	Focused     bool   `json:"focused"`
}

type layoutPane struct {
	PaneID string `json:"pane_id"`
	Rect   rect   `json:"rect"`
	Focus  bool   `json:"focused"`
}

type layoutRec struct {
	TabID         string       `json:"tab_id"`
	WorkspaceID   string       `json:"workspace_id"`
	FocusedPaneID string       `json:"focused_pane_id"`
	Panes         []layoutPane `json:"panes"`
}

type snapshot struct {
	FocusedPaneID    string      `json:"focused_pane_id"`
	FocusedTabID     string      `json:"focused_tab_id"`
	FocusedWorkspace string      `json:"focused_workspace_id"`
	Panes            []paneRec   `json:"panes"`
	Tabs             []tabRec    `json:"tabs"`
	Workspaces       []wsRec     `json:"workspaces"`
	Layouts          []layoutRec `json:"layouts"`
}

func loadSnapshot() (*snapshot, error) {
	out, err := herdr("api", "snapshot")
	if err != nil {
		return nil, err
	}
	var wrap struct {
		Result struct {
			Snapshot snapshot `json:"snapshot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(out, &wrap); err != nil {
		return nil, fmt.Errorf("parse snapshot: %w", err)
	}
	s := &wrap.Result.Snapshot
	byTab := map[string]*tabRec{}
	for i := range s.Tabs {
		byTab[s.Tabs[i].TabID] = &s.Tabs[i]
	}
	for _, l := range s.Layouts {
		if t, ok := byTab[l.TabID]; ok && l.FocusedPaneID != "" {
			t.ActivePaneID = l.FocusedPaneID
		}
	}
	return s, nil
}

// ---------------------------------------------------------------------------
// state: visit counts, last-seen, favorites
// ---------------------------------------------------------------------------

type paneStat struct {
	Visits        int64  `json:"visits"`
	Last          int64  `json:"last"`
	Label         string `json:"label,omitempty"`
	Note          string `json:"note,omitempty"`
	ManualBlocked bool   `json:"manual_blocked,omitempty"`
	RemindAt      int64  `json:"remind_at,omitempty"`
	Done          bool   `json:"done,omitempty"`
	DoneAt        int64  `json:"done_at,omitempty"`
	DoneState     string `json:"done_state,omitempty"`
	SnoozeMode    string `json:"snooze_mode,omitempty"`
	SnoozeState   string `json:"snooze_state,omitempty"`
	InInbox       bool   `json:"in_inbox,omitempty"`
}

type customTab struct {
	Name  string `json:"name"`
	Sort  string `json:"sort"`
	Dir   string `json:"dir,omitempty"`
	Limit int    `json:"limit"`
	Only  string `json:"only,omitempty"`
	Where string `json:"where,omitempty"`
	Fixed bool   `json:"fixed,omitempty"`
}

type store struct {
	Version      int                  `json:"version"`
	Panes        map[string]*paneStat `json:"panes"`
	Favorites    []string             `json:"favorites"`
	TabsInit     bool                 `json:"tabs_init"`
	Tabs         []customTab          `json:"tabs,omitempty"`
	InboxOrder   []string             `json:"inbox_order,omitempty"`
	ActiveTab    int                  `json:"active_tab,omitempty"`
	ActiveTabSet bool                 `json:"active_tab_set,omitempty"`
	ActiveSort   int                  `json:"active_sort,omitempty"`
	ActiveState  string               `json:"active_state,omitempty"`
	ActiveQuery  string               `json:"active_query,omitempty"`
	ActivePaneID string               `json:"active_pane_id,omitempty"`
	SnoozePicks  []string             `json:"snooze_picks,omitempty"`
	Jev          *jevState            `json:"jev,omitempty"`
}

func defaultSnoozePicks() []string {
	return []string{"20min", "1h", "3h", "tomorrow", "clear"}
}

func (s *store) getSnoozePicks() []string {
	if s != nil && len(s.SnoozePicks) > 0 {
		return s.SnoozePicks
	}
	return defaultSnoozePicks()
}

func (s *store) pinSnoozePick(pick string) bool {
	if s == nil {
		return false
	}
	pick = strings.TrimSpace(pick)
	if pick == "" {
		return false
	}
	for _, p := range s.getSnoozePicks() {
		if strings.EqualFold(p, pick) {
			return false
		}
	}
	picks := append([]string(nil), s.getSnoozePicks()...)
	if len(picks) > 0 && strings.EqualFold(picks[len(picks)-1], "clear") {
		picks = append(picks[:len(picks)-1], pick, "clear")
	} else {
		picks = append(picks, pick)
	}
	s.SnoozePicks = picks
	return true
}

func (s *store) removeSnoozePick(idx int) bool {
	if s == nil {
		return false
	}
	picks := s.getSnoozePicks()
	if idx < 0 || idx >= len(picks) || len(picks) <= 1 {
		return false
	}
	s.SnoozePicks = append(picks[:idx], picks[idx+1:]...)
	return true
}

type view struct {
	Kind string
	Idx  int
}

var (
	configDirOnce sync.Once
	configDirVal  string
)

func configDir() string {
	configDirOnce.Do(func() { configDirVal = resolveConfigDir() })
	return configDirVal
}

// resolveConfigDir answers with the herdr CLI directory and falls back to
// the deterministic encoded layout herdr uses. Cross-process serialization
// holds while every process resolves one directory: the memo plus this
// byte-identical fallback give that on this platform, and a herdr layout
// whose real config dir differs from the encoded fallback would still split
// writers.
func resolveConfigDir() string {
	if v := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); v != "" {
		return v
	}
	out, err := herdr("plugin", "config-dir", pluginID)
	if err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return d
		}
	}
	enc := strings.ReplaceAll(pluginID, "R", "%52")
	enc = strings.ReplaceAll(enc, "A", "%41")
	return filepath.Join(os.Getenv("HOME"), ".config", "herdr", "plugins", "config", enc)
}

func statePath() string { return filepath.Join(configDir(), "recall.json") }

// proven holds the recall.json bytes this process last loaded, adopted, or
// saved: the content fingerprint a long-lived picker compares before
// touching shared state, so a concurrent hook or CLI write is never
// overwritten by a stale copy. The bytes decide, so an external write that
// reuses a coarse modtime is still adopted. They are also the held dataset
// without this process's unsaved changes, so every recreation check and
// every re-materialization after a refused save reads them, never the live
// store.
var proven atomic.Value // []byte

// loadFailed records that the last read of recall.json hit a real error, so
// the in-memory store is unproven against disk and must not be written back
// over whatever is there.
var loadFailed atomic.Bool

// saveTaint marks the store tainted while a refused save could not be
// rolled back: the in-memory store may carry unsaved state that must never
// reach disk until a clean reload proves the state again.
var saveTaint atomic.Bool

// stateVanished marks a vanish episode open: this process saw recall.json
// gone, or saw it come back missing a pane no writer could have dropped. The
// episode stays open until the held dataset is proven on disk again, by a
// landed save or by adopting bytes that prove continuation.
var stateVanished atomic.Bool

func stampStore(b []byte) {
	proven.Store(b)
}

func provenBytes() []byte {
	b, _ := proven.Load().([]byte)
	return b
}

// provenState parses the proven bytes into a store of its own. With nothing
// proven yet it is the fresh state loadStore starts from.
func provenState() *store {
	if s, err := parseStore(provenBytes()); err == nil {
		return s
	}
	return &store{Version: 1, Panes: map[string]*paneStat{}}
}

// pinned reports whether the pane carries state no writer may drop: a note,
// a manual block, a reminder, or a favorite. prune keeps these.
func (s *store) pinned(id string) bool {
	ps := s.Panes[id]
	return s.isFavorite(id) || ps != nil && (ps.Note != "" || ps.ManualBlocked || ps.RemindAt > 0)
}

// pruneKeep is the history window prune keeps for closed panes.
const pruneKeep = 200

// recreatesState reports whether the store on disk is a recreation of the
// proven state instead of a writer's continuation of it. Inside a vanish
// episode only a file that continues the proven state may replace it.
// Outside one, a file that strips content units no single legitimate action
// removes was built again, and so was one that misses a proven pane prune
// could not have dropped. A missing proven pane is a prune's work only when
// the file carries a full keep window of panes ranked at or above it: prune
// drops just the tail of a closed set it has a window to drop. A held pin
// proves nothing for a missing pane: a writer may un-pin and prune it for
// real. Either finding opens the episode. The proven state is the reference,
// so this process's own unsaved changes are no evidence either way.
func recreatesState(disk *store) bool {
	held := provenState()
	if stateVanished.Load() {
		return !continues(held, disk)
	}
	if strips(held, disk) {
		stateVanished.Store(true)
		return true
	}
	for id, hp := range held.Panes {
		if disk.Panes[id] != nil {
			continue
		}
		ahead := 0
		for _, dp := range disk.Panes {
			if dp.Last >= hp.Last {
				ahead++
			}
		}
		if ahead < pruneKeep {
			stateVanished.Store(true)
			return true
		}
	}
	return false
}

// continues reports whether disk carries every pane of held and every
// content unit on it: each note, manual block, reminder, and favorite, each
// tab, and each Jev weight. A file that reappears during a vanish episode
// must prove that before it replaces the held dataset.
func continues(held, disk *store) bool {
	for id := range held.Panes {
		if disk.Panes[id] == nil {
			return false
		}
	}
	for _, id := range held.Favorites {
		if !disk.isFavorite(id) {
			return false
		}
	}
	if held.Jev != nil {
		for k := range held.Jev.Weights {
			if disk.Jev == nil {
				return false
			}
			if _, ok := disk.Jev.Weights[k]; !ok {
				return false
			}
		}
	}
	return !strips(held, disk)
}

// strips reports whether disk dropped content units of held that no single
// legitimate action removes: the note, manual block, or reminder of a pane
// disk still carries, a tab, or every held favorite when held had more than
// one. No hook or command clears the pane units or tabs, and the favorite
// command and m remove one favorite at a time, so a changed file that drops
// these does not prove it continues the held state. Any drop of favorites
// short of a multi-favorite wipe, and any drop of Jev weights, is left to
// the episode check: the favorite and jev reset commands make such drops for
// real.
func strips(held, disk *store) bool {
	for id, hp := range held.Panes {
		dp := disk.Panes[id]
		if dp != nil && (hp.Note != "" && dp.Note == "" || hp.ManualBlocked && !dp.ManualBlocked || hp.RemindAt > 0 && dp.RemindAt == 0) {
			return true
		}
	}
	for _, ht := range held.Tabs {
		found := false
		for _, dt := range disk.Tabs {
			if dt.Name == ht.Name {
				found = true
				break
			}
		}
		if !found {
			return true
		}
	}
	return len(held.Favorites) > 1 && !slices.ContainsFunc(held.Favorites, disk.isFavorite)
}

// refreshStore adopts on-disk writes made by hook or CLI processes since this
// process last loaded or saved recall.json. It reloads only when the file
// content changed, so rows stay frozen while no other writer ran.
func refreshStore(st *store) *store {
	b, err := os.ReadFile(statePath())
	if err != nil {
		loadFailed.Store(!os.IsNotExist(err))
		if os.IsNotExist(err) {
			stateVanished.Store(true)
		}
		return st
	}
	if p := provenBytes(); p != nil && bytes.Equal(b, p) {
		loadFailed.Store(false)
		return st
	}
	fresh, perr := parseStore(b)
	if perr != nil {
		loadFailed.Store(true)
		return st
	}
	loadFailed.Store(false)
	if recreatesState(fresh) {
		return st
	}
	stateVanished.Store(false)
	stampStore(b)
	return fresh
}

// storeLock serializes read-modify-write cycles on recall.json across the
// long-lived picker and the short-lived hook and CLI processes, while they
// resolve one config directory (see resolveConfigDir). Without it the last
// full-file write wins and erases concurrent visits, un-done threads, and
// prune results.
type storeLock struct{ f *os.File }

func lockPath() string { return filepath.Join(configDir(), "recall.lock") }

func (l *storeLock) acquire() error {
	if l.f != nil {
		return nil
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(lockPath(), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return err
	}
	l.f = f
	return nil
}

func (l *storeLock) release() {
	if l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}

func (l *storeLock) held() bool { return l.f != nil }

// cleanReload replaces the in-memory store with the proven bytes on disk and
// clears the taint: only a reload can show the state again after a failed
// rollback. It judges the file the way the refresh does: a file gone on its
// watch opens the vanish episode, and bytes that are a recreation of the
// proven state prove nothing and never replace the held dataset.
func cleanReload(lk *storeLock) (*store, error) {
	lk.acquire()
	b, err := os.ReadFile(statePath())
	if err != nil {
		if os.IsNotExist(err) {
			stateVanished.Store(true)
		}
		return nil, err
	}
	fresh, err := parseStore(b)
	if err != nil {
		return nil, err
	}
	if recreatesState(fresh) {
		return nil, errors.New("recall.json rebuilt, state unproven")
	}
	loadFailed.Store(false)
	saveTaint.Store(false)
	stateVanished.Store(false)
	stampStore(b)
	return fresh, nil
}

// persistStore writes st under the held lock. A refused write rolls the
// uncommitted mutation back by reloading the store from disk under the lock,
// so nothing unsaved lingers, and reports the refusal. When that reload
// fails the store is tainted, and nothing it holds may persist until a clean
// reload proves the state again. The taint never blocks re-materialization:
// inside an open vanish episode the proven state, which is the held dataset
// without the refused mutation, is written back instead.
func persistStore(lk *storeLock, st *store) (*store, error) {
	write := func(s *store) error {
		if !lk.held() {
			return errors.New("recall.lock unavailable")
		}
		return saveStore(s)
	}
	if saveTaint.Load() {
		fresh, err := cleanReload(lk)
		if err == nil {
			return fresh, errors.New("recall state reloaded from disk")
		}
		if !stateVanished.Load() {
			return st, err
		}
		held := provenState()
		if err := write(held); err != nil {
			return st, err
		}
		saveTaint.Store(false)
		return held, errors.New("last action not applied - repeat it")
	}
	err := write(st)
	if err == nil {
		return st, nil
	}
	if fresh, rerr := cleanReload(lk); rerr == nil {
		return fresh, err
	}
	saveTaint.Store(true)
	return st, err
}

// saveNote keeps a refusal note on screen until the operator has seen it, so
// no save can clear a note that was never rendered. The status line clips at
// the pane width, so the note can reach the screen as a head of itself; what
// the emitted frame displays is what marks it seen.
type saveNote struct {
	text  string
	shown bool
}

func (n *saveNote) refuse(err error) string {
	n.text = "not saved: " + err.Error()
	n.shown = false
	return n.text
}

func (n *saveNote) clear(status string) string {
	if n.shown && status == n.text {
		return ""
	}
	return status
}

func (n *saveNote) rendered(frame string) {
	if n.text == "" {
		return
	}
	if strings.Contains(frame, n.text) {
		n.shown = true
		return
	}
	// Clipping cuts the note's tail, never its head, so an emitted status
	// line is exactly a head of the note. That visible head marks it seen.
	for _, line := range strings.Split(strings.ReplaceAll(frame, "\r\n", "\n"), "\n") {
		if line := stripANSI(line); line != "" && strings.HasPrefix(n.text, line) {
			n.shown = true
			return
		}
	}
}

// commitNote stores text on the pane captured when the note opened and labels
// the status from the label captured with it.
func commitNote(st *store, paneID, label, text string) string {
	ps := st.ensurePane(paneID)
	ps.Note = strings.TrimSpace(text)
	if ps.Note != "" {
		ps.Done = false
		return "note saved: " + label
	}
	return "note cleared: " + label
}

// commitTeaching records t in the store and saves it. Only a landed save
// appends t to the learning log, so a refused teach leaves no record behind
// and its retry appends exactly once.
func commitTeaching(st *store, t jevTeaching, save func() bool) bool {
	recordJevTeaching(st, t)
	if !save() {
		return false
	}
	_ = logJevTeaching(t)
	return true
}

// undoDone reverts the done marker behind mark and returns the mark that
// stays armed. Only a landed save spends it: a refused save rolls the marker
// back and keeps the mark, so the next z retries the same undo. The e handler
// arms the mark the same way, only on a landed save.
func undoDone(st *store, mark string, save func() bool) string {
	ps := st.Panes[mark]
	if ps == nil {
		return mark
	}
	ps.Done = false
	ps.DoneState = ""
	st.addToInbox(mark)
	if save() {
		return ""
	}
	return mark
}

func loadStore() (*store, error) {
	b, err := os.ReadFile(statePath())
	if err != nil {
		loadFailed.Store(!os.IsNotExist(err))
		if os.IsNotExist(err) {
			return &store{Version: 1, Panes: map[string]*paneStat{}}, nil
		}
		return nil, err
	}
	s, perr := parseStore(b)
	if perr != nil {
		loadFailed.Store(true)
		return nil, perr
	}
	loadFailed.Store(false)
	stampStore(b)
	return s, nil
}

func parseStore(b []byte) (*store, error) {
	s := &store{Version: 1, Panes: map[string]*paneStat{}}
	if err := json.Unmarshal(b, s); err != nil {
		return nil, err
	}
	if s.Panes == nil {
		return nil, errors.New("recall.json has no panes")
	}
	if !s.TabsInit {
		s.Tabs = defaultTabs()
		s.TabsInit = true
	}
	// Later was inserted at index 2. Shift a saved index so Recent stays Recent.
	if s.Version < 2 {
		if s.ActiveTab >= 2 {
			s.ActiveTab++
		}
		s.Version = 2
	}
	// Bump persisted 20 limits to 50 so Most used fills the popup.
	for i := range s.Tabs {
		if s.Tabs[i].Limit == 20 {
			s.Tabs[i].Limit = 50
		}
	}
	return s, nil
}

func defaultTabs() []customTab {
	return []customTab{
		{Name: "Recent", Sort: "last", Limit: 50, Fixed: true},
		{Name: "Most used", Sort: "times", Limit: 50, Fixed: true},
	}
}

func views(st *store) []view {
	out := []view{{Kind: "favorites"}, {Kind: "inbox"}, {Kind: "later"}}
	for i := range st.Tabs {
		out = append(out, view{Kind: "custom", Idx: i})
	}
	out = append(out, view{Kind: "all"})
	return out
}

func viewName(st *store, v view) string {
	switch v.Kind {
	case "inbox":
		return "Inbox"
	case "later":
		return "Later"
	case "all":
		return "All"
	case "favorites":
		return "Favorites"
	default:
		if v.Idx >= 0 && v.Idx < len(st.Tabs) {
			return st.Tabs[v.Idx].Name
		}
		return "Tab"
	}
}

func viewNames(st *store) []string {
	vs := views(st)
	names := make([]string, len(vs))
	for i, v := range vs {
		names[i] = viewName(st, v)
	}
	return names
}

// openTab restores the tab the captain left. A fresh store still opens
// Inbox. Index 0 is both "unset" and Favorites, so Favorites sticks only
// after a session has saved active_tab_set.
// tabJumpLetters maps a browse letter to a tab index. Fixed tabs keep
// i, r, f, m, and a. A custom tab takes the first free letter of its name.
func tabJumpLetters(st *store) map[string]int {
	vs := views(st)
	out := map[string]int{}
	claim := func(letter string, idx int) {
		if letter == "" {
			return
		}
		if _, taken := out[letter]; taken {
			return
		}
		out[letter] = idx
	}
	for i, v := range vs {
		switch v.Kind {
		case "inbox":
			claim("i", i)
		case "later":
			claim("l", i)
		case "favorites":
			claim("f", i)
		case "all":
			claim("a", i)
		default:
			name := strings.ToLower(viewName(st, v))
			switch name {
			case "recent":
				claim("r", i)
			case "most used":
				claim("m", i)
			default:
				for _, r := range name {
					if r < 'a' || r > 'z' {
						continue
					}
					s := string(r)
					if _, taken := out[s]; taken {
						continue
					}
					claim(s, i)
					break
				}
			}
		}
	}
	return out
}

func registerGoChords(d *keys.Dispatcher, st *store) {
	chords := map[string]string{"g": "goto-top"}
	jumps := tabJumpLetters(st)
	var hint []string
	seen := map[string]bool{}
	add := func(letter string) {
		idx, ok := jumps[letter]
		if !ok || seen[letter] {
			return
		}
		seen[letter] = true
		chords[letter] = fmt.Sprintf("goto:%d", idx)
		hint = append(hint, letter+" "+viewName(st, views(st)[idx]))
	}
	for _, letter := range []string{"i", "l", "r", "f", "m", "a"} {
		add(letter)
	}
	var extra []string
	for letter := range jumps {
		if !seen[letter] {
			extra = append(extra, letter)
		}
	}
	sort.Strings(extra)
	for _, letter := range extra {
		add(letter)
	}
	hint = append(hint, "g top")
	d.Register("g", chords, strings.Join(hint, "  "))
}

func openTab(st *store) int {
	vs := views(st)
	inbox := 0
	for i, v := range vs {
		if v.Kind == "inbox" {
			inbox = i
			break
		}
	}
	if (st.ActiveTabSet || st.ActiveTab > 0) && st.ActiveTab >= 0 && st.ActiveTab < len(vs) {
		return st.ActiveTab
	}
	return inbox
}

func saveStore(s *store) error {
	if loadFailed.Load() {
		return errors.New("recall.json unreadable")
	}
	if err := os.MkdirAll(configDir(), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := statePath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, statePath()); err != nil {
		return err
	}
	stateVanished.Store(false)
	stampStore(b)
	return nil
}

// prune drops history for panes that are closed. A live pane keeps its triage
// state no matter how long it sits, so review history never evaporates.
func (s *store) prune(live map[string]bool, keep int) {
	type kv struct {
		id string
		ts int64
	}
	var all []kv
	for id, st := range s.Panes {
		if live[id] {
			continue
		}
		all = append(all, kv{id, st.Last})
	}
	if len(all) <= keep {
		return
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ts > all[j].ts })
	for _, e := range all[keep:] {
		if !s.pinned(e.id) {
			delete(s.Panes, e.id)
		}
	}
}

func (s *store) ensurePane(id string) *paneStat {
	if s.Panes == nil {
		s.Panes = map[string]*paneStat{}
	}
	ps := s.Panes[id]
	if ps == nil {
		ps = &paneStat{}
		s.Panes[id] = ps
	}
	return ps
}

func (s *store) isFavorite(id string) bool {
	for _, f := range s.Favorites {
		if f == id {
			return true
		}
	}
	return false
}

func (s *store) toggleFavorite(id string) bool {
	if s.isFavorite(id) {
		out := s.Favorites[:0]
		for _, f := range s.Favorites {
			if f != id {
				out = append(out, f)
			}
		}
		s.Favorites = out
		return false
	}
	s.Favorites = append(s.Favorites, id)
	return true
}

// recordFocus bumps the currently focused pane.
func recordFocus() (string, error) {
	snap, err := loadSnapshot()
	if err != nil {
		return "", err
	}
	id := snap.FocusedPaneID
	if id == "" {
		return "", fmt.Errorf("no focused pane in snapshot")
	}
	label := paneLabel(snap, id)
	lk := &storeLock{}
	if err := lk.acquire(); err != nil {
		return "", err
	}
	defer lk.release()
	st, err := loadStore()
	if err != nil {
		return "", err
	}
	now := time.Now().Unix()
	ps := st.ensurePane(id)
	ps.Visits++
	ps.Last = now
	if label != "" {
		ps.Label = label
	}
	// Only real work un-Dones a thread. Viewing a pane flips done to idle in
	// Herdr, and that is the operator reading, not the agent moving.
	if ps.Done {
		if w := liveStatusWord(snap, id); w == "working" || w == "blocked" {
			ps.Done = false
		}
	}
	st.prune(liveSet(snap), pruneKeep)
	return id, saveStore(st)
}

func liveSet(s *snapshot) map[string]bool {
	live := make(map[string]bool, len(s.Panes))
	for _, p := range s.Panes {
		live[p.PaneID] = true
	}
	return live
}

// baselineState stores the status word recorded when a thread left Inbox.
// An empty status becomes "none" so the first real status counts as new work.
func baselineState(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

// activityChanged reports whether the agent did something new since baseline.
// done -> idle only flips Herdr's seen marker when the operator views a pane,
// so that pair is the operator reading, not new work. An empty baseline is a
// legacy row with no record, and it honours whatever the row already says.
func activityChanged(now, baseline string) bool {
	if now == "" || baseline == "" || now == baseline {
		return false
	}
	if now == "idle" && baseline == "done" {
		return false
	}
	return true
}

func liveStatusWord(s *snapshot, id string) string {
	if s == nil {
		return ""
	}
	for _, p := range s.Panes {
		if p.PaneID == id {
			return statusWord(p.AgentStatus)
		}
	}
	return ""
}

func paneLabel(s *snapshot, id string) string {
	return displayLabel(s, id, "")
}

// ---------------------------------------------------------------------------
// jump: workspace, tab, then geometry hops until the pane is focused
// ---------------------------------------------------------------------------

func jumpTo(paneID string) error {
	snap, err := loadSnapshot()
	if err != nil {
		return err
	}
	live := false
	for _, p := range snap.Panes {
		if p.PaneID == paneID {
			live = true
			break
		}
	}
	if !live {
		return fmt.Errorf("that pane is closed")
	}
	return focusPane(paneID)
}

// focusPane uses herdr's pane.focus API. Direction hops miss panes that are
// not a neighbor, so Enter looked like it did nothing.
func focusPane(paneID string) error {
	sock := os.Getenv("HERDR_SOCKET_PATH")
	if sock == "" {
		return fmt.Errorf("HERDR_SOCKET_PATH unset")
	}
	conn, err := net.Dial("unix", sock)
	if err != nil {
		return err
	}
	defer conn.Close()
	req, err := json.Marshal(map[string]any{
		"id":     "recall-focus",
		"method": "pane.focus",
		"params": map[string]string{"pane_id": paneID},
	})
	if err != nil {
		return err
	}
	if _, err := conn.Write(append(req, '\n')); err != nil {
		return err
	}
	var resp struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&resp); err != nil {
		return err
	}
	if resp.Error != nil {
		return fmt.Errorf("%s", resp.Error.Message)
	}
	return nil
}

// ---------------------------------------------------------------------------
// picker frame: same shape as the built-in goto overlay
// ---------------------------------------------------------------------------

type row struct {
	PaneID        string
	WorkspaceID   string
	Workspace     string
	Agent         string
	CWD           string
	Label         string
	Note          string
	Section       string
	Visits        int64
	Last          int64
	Status        string
	Fav           bool
	ManualBlocked bool
	RemindAt      int64
	Done          bool
	DoneAt        int64
	DoneState     string
	SnoozeMode    string
	SnoozeState   string
}

// displayLabel is the Herdr tab name only, the same label prefix k uses
// for a tab row. Agent titles, workspace names, and cwd are not part of it.
func displayLabel(s *snapshot, id, stored string) string {
	var p *paneRec
	for i := range s.Panes {
		if s.Panes[i].PaneID == id {
			p = &s.Panes[i]
			break
		}
	}
	if p == nil {
		if stored != "" {
			return stored
		}
		return id
	}
	for _, t := range s.Tabs {
		if t.TabID == p.TabID {
			if name := strings.TrimSpace(t.Label); name != "" {
				return name
			}
		}
	}
	return id
}

func collectRows(s *snapshot, st *store) []row {
	statusOf := map[string]string{}
	for _, p := range s.Panes {
		statusOf[p.PaneID] = p.AgentStatus
	}
	live := map[string]bool{}
	for _, p := range s.Panes {
		live[p.PaneID] = true
	}
	seen := map[string]bool{}
	var ids []string
	addID := func(id string) {
		if id == "" || seen[id] || !live[id] {
			return
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, id := range st.Favorites {
		addID(id)
	}
	paneKeys := make([]string, 0, len(st.Panes))
	for id := range st.Panes {
		paneKeys = append(paneKeys, id)
	}
	sort.Strings(paneKeys)
	for _, id := range paneKeys {
		addID(id)
	}
	for _, p := range s.Panes {
		addID(p.PaneID)
	}
	var out []row
	for _, id := range ids {
		ps := st.Panes[id]
		label := ""
		if ps != nil {
			label = ps.Label
		}
		label = displayLabel(s, id, label)
		var visits, last, remindAt, doneAt int64
		var note, doneState, snoozeMode, snoozeState string
		var manualBlocked, done bool
		if ps != nil {
			visits, last = ps.Visits, ps.Last
			note = ps.Note
			manualBlocked = ps.ManualBlocked
			remindAt = ps.RemindAt
			done = ps.Done
			doneAt = ps.DoneAt
			doneState = ps.DoneState
			snoozeMode = ps.SnoozeMode
			snoozeState = ps.SnoozeState
		}
		status := statusOf[id]
		if manualBlocked {
			status = "blocked"
		}
		agent, cwd, ws, wsLabel := "", "", "", ""
		for _, p := range s.Panes {
			if p.PaneID == id {
				ws = p.WorkspaceID
				agent = p.Agent
				cwd = p.CWD
				if cwd == "" {
					cwd = p.ForegroundCWD
				}
				break
			}
		}
		for _, w := range s.Workspaces {
			if w.WorkspaceID == ws {
				wsLabel = w.Label
				break
			}
		}
		out = append(out, row{
			PaneID: id, WorkspaceID: ws, Workspace: wsLabel,
			Agent: agent, CWD: cwd, Label: label, Note: note,
			Visits: visits, Last: last, Status: status,
			Fav: st.isFavorite(id), ManualBlocked: manualBlocked,
			RemindAt: remindAt, Done: done, DoneAt: doneAt, DoneState: doneState,
			SnoozeMode: snoozeMode, SnoozeState: snoozeState,
		})
	}
	return out
}

func canonicalSort(sort, dir string) string {
	switch sort {
	case "newest", "oldest", "most", "least":
		return sort
	case "times":
		if dir == "asc" {
			return "least"
		}
		return "most"
	default:
		if dir == "asc" {
			return "oldest"
		}
		return "newest"
	}
}

func sortBy(list []row, how string) {
	how = canonicalSort(how, "")
	byTimes := how == "most" || how == "least"
	desc := how == "newest" || how == "most"
	sort.SliceStable(list, func(i, j int) bool {
		if byTimes && list[i].Visits != list[j].Visits {
			if desc {
				return list[i].Visits > list[j].Visits
			}
			return list[i].Visits < list[j].Visits
		}
		if !byTimes && list[i].Last != list[j].Last {
			if desc {
				return list[i].Last > list[j].Last
			}
			return list[i].Last < list[j].Last
		}
		return strings.ToLower(list[i].Label) < strings.ToLower(list[j].Label)
	})
}

func rowAllowed(r row, ct customTab, here string) bool {
	switch ct.Only {
	case "favorites":
		if !r.Fav {
			return false
		}
	}
	if ct.Where == "here" && (here == "" || r.WorkspaceID != here) {
		return false
	}
	return true
}

func inInbox(r row, now int64) bool {
	if r.ManualBlocked {
		return true
	}
	if r.RemindAt > now {
		// "if no reply" snoozes break early when the agent does something new.
		if r.SnoozeMode != "regardless" && activityChanged(statusWord(r.Status), r.SnoozeState) {
			return true
		}
		return false
	}
	if r.RemindAt > 0 && r.RemindAt <= now {
		return true
	}
	sw := statusWord(r.Status)
	if !r.Done {
		return r.Last > 0 || r.Note != "" || sw == "blocked" || sw == "done"
	}
	// Done is not mute. New agent activity brings the thread back.
	return activityChanged(sw, r.DoneState)
}

func inboxBucket(r row, now int64) int {
	if r.ManualBlocked || (r.RemindAt > 0 && r.RemindAt <= now) || statusWord(r.Status) == "blocked" {
		return 0
	}
	if statusWord(r.Status) == "done" {
		return 1
	}
	return 2
}

func sortInbox(list []row, now int64) {
	sort.SliceStable(list, func(i, j int) bool {
		bi, bj := inboxBucket(list[i], now), inboxBucket(list[j], now)
		if bi != bj {
			return bi < bj
		}
		if list[i].Last != list[j].Last {
			return list[i].Last > list[j].Last
		}
		return strings.ToLower(list[i].Label) < strings.ToLower(list[j].Label)
	})
}

func syncInboxRows(all []row, st *store, now int64) []row {
	byID := make(map[string]row, len(all))
	for _, r := range all {
		byID[r.PaneID] = r
		ps := st.ensurePane(r.PaneID)
		if r.RemindAt > now && !r.ManualBlocked {
			if ps.SnoozeMode != "regardless" && activityChanged(statusWord(r.Status), ps.SnoozeState) {
				// New agent activity cancels an "if no reply" snooze right away.
				ps.RemindAt = 0
				ps.SnoozeMode = ""
				ps.SnoozeState = ""
				r.RemindAt = 0
				byID[r.PaneID] = r
			} else {
				ps.InInbox = false
				continue
			}
		}
		if ps.InInbox {
			if r.Done != ps.Done {
				r.Done = ps.Done
				byID[r.PaneID] = r
			}
			continue
		}
		if inInbox(r, now) {
			ps.InInbox = true
			ps.Done = false
			r.Done = false
			byID[r.PaneID] = r
		}
	}
	seen := map[string]bool{}
	var orderedIDs []string
	for _, id := range st.InboxOrder {
		if _, ok := byID[id]; !ok || seen[id] {
			continue
		}
		if ps := st.Panes[id]; ps != nil && ps.InInbox {
			seen[id] = true
			orderedIDs = append(orderedIDs, id)
		}
	}
	var added []row
	for _, r := range all {
		if seen[r.PaneID] {
			continue
		}
		if ps := st.Panes[r.PaneID]; ps != nil && ps.InInbox {
			seen[r.PaneID] = true
			added = append(added, r)
		}
	}
	if st.jevOn() {
		// Only new arrivals may land at the front. Rows already in the queue
		// keep their order, so e archives one row and does not shuffle the rest.
		st.jevSortDesc(added, now)
		front := make([]string, 0, len(added)+len(orderedIDs))
		for _, r := range added {
			front = append(front, r.PaneID)
		}
		orderedIDs = append(front, orderedIDs...)
	} else {
		sortInbox(added, now)
		for _, r := range added {
			orderedIDs = append(orderedIDs, r.PaneID)
		}
	}
	// An overdue reminder rises to the top. The other rows keep their order.
	orderedIDs = promoteOverdue(orderedIDs, byID, now)
	st.InboxOrder = orderedIDs
	out := make([]row, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		out = append(out, byID[id])
	}
	return out
}

func promoteOverdue(ids []string, byID map[string]row, now int64) []string {
	var due, rest []string
	for _, id := range ids {
		r := byID[id]
		if r.RemindAt > 0 && r.RemindAt <= now {
			due = append(due, id)
		} else {
			rest = append(rest, id)
		}
	}
	return append(due, rest...)
}

func (s *store) removeFromInbox(id string) {
	if ps := s.Panes[id]; ps != nil {
		ps.InInbox = false
	}
	out := s.InboxOrder[:0]
	for _, existing := range s.InboxOrder {
		if existing != id {
			out = append(out, existing)
		}
	}
	s.InboxOrder = out
}

// addToInbox puts a pane back into the queue at the end, so a returning thread
// never displaces the thread the captain is reviewing.
func (s *store) addToInbox(id string) {
	for _, existing := range s.InboxOrder {
		if existing == id {
			return
		}
	}
	if ps := s.Panes[id]; ps != nil {
		ps.InInbox = true
	}
	s.InboxOrder = append(s.InboxOrder, id)
}

func rowsFor(s *snapshot, st *store, v view, mode int) []row {
	all := collectRows(s, st)
	now := time.Now().Unix()
	if v.Kind == "inbox" {
		return inboxVisible(syncInboxRows(all, st, now))
	}
	// Reference tabs (Recent, Most used, All, Favorites) show every pane, done or not,
	// so Recent and Most used keep their full 50 history slots. Done rows carry a check mark.
	return rowsForInner(all, s, st, v, mode, now)
}

// triageVisible drops rows done by e. A snoozed row keeps its badge and stays
// until its reminder is due.
func triageVisible(list []row) []row {
	out := make([]row, 0, len(list))
	for _, r := range list {
		if !r.Done || r.RemindAt > 0 {
			out = append(out, r)
		}
	}
	return out
}

// inboxVisible drops rows marked done. The active pane stays in the inbox
// queue so jumping between threads never shifts or hides rows.
func inboxVisible(list []row) []row {
	out := make([]row, 0, len(list))
	for _, r := range list {
		if r.Done && r.RemindAt <= 0 {
			continue
		}
		out = append(out, r)
	}
	return out
}

func rowsForInner(all []row, s *snapshot, st *store, v view, mode int, now int64) []row {
	switch v.Kind {
	case "inbox":
		return syncInboxRows(all, st, now)
	case "later":
		var later []row
		for _, r := range all {
			if r.RemindAt > 0 {
				later = append(later, r)
			}
		}
		sort.SliceStable(later, func(i, j int) bool {
			if later[i].RemindAt != later[j].RemindAt {
				return later[i].RemindAt < later[j].RemindAt
			}
			return later[i].PaneID < later[j].PaneID
		})
		return later
	case "favorites":
		var list []row
		for _, r := range all {
			if r.Fav {
				list = append(list, r)
			}
		}
		how := sortHow(mode, "starred")
		if how == "starred" {
			order := map[string]int{}
			for i, id := range st.Favorites {
				order[id] = i
			}
			sort.SliceStable(list, func(i, j int) bool {
				return order[list[i].PaneID] < order[list[j].PaneID]
			})
			return list
		}
		sortBy(list, how)
		return list
	case "custom":
		ct := st.Tabs[v.Idx]
		var list []row
		for _, r := range all {
			if !rowAllowed(r, ct, s.FocusedWorkspace) {
				continue
			}
			list = append(list, r)
		}
		sortBy(list, canonicalSort(ct.Sort, ct.Dir))
		if ct.Limit > 0 && len(list) > ct.Limit {
			list = list[:ct.Limit]
		}
		return list
	default:
		list := append([]row(nil), all...)
		sortBy(list, sortHow(mode, "newest"))
		return list
	}
}

func sortHow(mode int, fallback string) string {
	switch mode {
	case sortNewest:
		return "newest"
	case sortOldest:
		return "oldest"
	case sortMost:
		return "most"
	case sortLeast:
		return "least"
	default:
		return fallback
	}
}

func sortLabel(how string) string {
	switch canonicalSort(how, "") {
	case "oldest":
		return "Oldest"
	case "most":
		return "Most used"
	case "least":
		return "Least used"
	default:
		return "Most recent"
	}
}

func activeSortLabel(st *store, v view, mode int) string {
	if v.Kind == "inbox" {
		return ""
	}
	if v.Kind == "custom" && v.Idx >= 0 && v.Idx < len(st.Tabs) {
		return sortLabel(canonicalSort(st.Tabs[v.Idx].Sort, st.Tabs[v.Idx].Dir))
	}
	if mode != sortDefault {
		return sortLabel(sortHow(mode, "newest"))
	}
	switch v.Kind {
	case "favorites":
		return "Starred"
	default:
		return "Most recent"
	}
}

func metricText(r row, width int, kind string) string {
	when := ago(r.Last)
	times := "once"
	switch {
	case r.Visits == 0:
		times = "never"
	case r.Visits > 1:
		times = fmt.Sprintf("%d times", r.Visits)
	}
	switch kind {
	case "newest", "oldest", "last":
		return when
	case "most", "least", "times":
		return times
	default:
		if width < 68 {
			return when
		}
		return times + " · " + when
	}
}

func ago(ts int64) string {
	if ts == 0 {
		return "never"
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// statusWord strips the brackets and unknown-ness of a row status.
func statusWord(status string) string {
	switch status {
	case "", "unknown":
		return ""
	case "blocked", "failed":
		return "blocked"
	case "working", "running", "fixing":
		return "working"
	case "done", "completed":
		return "done"
	case "idle", "paused":
		return "idle"
	default:
		return status
	}
}

// statusColor maps a status to the same colour vocabulary herdr's own
// status.go state_dot uses: red blocked, yellow working, teal done, green idle.
func statusColor(status string) string {
	switch statusWord(status) {
	case "blocked":
		return keyStBad
	case "working":
		return keyStBusy
	case "done":
		return keyStGood
	case "idle":
		return keyStFree
	default:
		return keyOverlay
	}
}

// statusMark is the prefix-k dot: filled for blocked, working, and done,
// open for idle, and a dim mark when the pane has no status.
func statusMark(status string) (string, string) {
	switch statusWord(status) {
	case "idle":
		return "○", statusColor(status)
	case "":
		return "·", keyOverlay
	default:
		return "●", statusColor(status)
	}
}

func sectionColor(section string) string {
	switch section {
	case "FAVORITES":
		return keySecFav
	case "RECENT":
		return keySecRec
	case "MOST USED":
		return keySecUsed
	default:
		return keyOverlay
	}
}

// modeChip is the small badge left of the prompt. It carries the same
// information as vim's -- INSERT -- bar, in one bracketed cell.
func modeChip(m vimline.Mode, pending rune, count int) string {
	label := "N"
	color := keyAccentFg
	switch {
	case pending == 'd' || pending == 'c' || pending == 'y':
		label = strings.ToUpper(string(pending))
		color = keyStBad
	case pending == 'f' || pending == 'F' || pending == 't' || pending == 'T':
		label = string(pending)
		color = keyStBusy
	case pending == 'r':
		label = "r"
		color = keyStBusy
	case m == vimline.ModeInsert:
		label = "I"
		color = keyStFree
	case m == vimline.ModeReplace:
		label = "R"
		color = keyStBusy
	}
	if count > 0 {
		label += " " + strconv.Itoa(count)
	}
	return keySurface + "[" + keyReset + color + keyBold + label + keyReset + keySurface + "]" + keyReset
}

// fieldItem pairs a rune with its cursor state.
type fieldItem struct {
	ch    rune
	isCur bool
}

// renderField paints an editing field with visible cursor cell, wrapping lines
// cleanly at word boundaries when width is exceeded so text never runs off the screen.
func renderField(fv vimline.View, width int) string {
	if width <= 0 {
		width = 80
	}
	effectiveWidth := width - 2
	if effectiveWidth < 20 {
		effectiveWidth = 20
	}

	if !fv.Focused {
		if fv.Text == "" {
			return keyOverlay + "  " + fv.Label + " " + keyReset + keyOverlay + "search panes" + keyReset
		}
		prefix := keyOverlay + "  " + fv.Label + " " + keyReset
		indent := "    "
		return wrapWordsPlain(fv.Text, prefix, indent, effectiveWidth)
	}

	prefix := modeChip(fv.Mode, fv.Pending, fv.Count) + keyAccentFg + keyBold + "  " + fv.Label + " " + keyReset
	prefixPlainLen := len([]rune(stripANSI(prefix)))
	indent := "      "
	indentLen := len([]rune(indent))

	runes := []rune(fv.Text)
	cur := fv.Cursor
	total := len(runes)
	if cur >= total {
		total = cur + 1
	}

	var items []fieldItem
	for i := 0; i < total; i++ {
		ch := ' '
		if i < len(runes) {
			ch = runes[i]
		}
		isCur := (i == cur)
		if i == len(runes) && !isCur {
			continue
		}
		items = append(items, fieldItem{ch: ch, isCur: isCur})
	}

	type chunk struct {
		items   []fieldItem
		isSpace bool
	}
	var chunks []chunk
	idx := 0
	for idx < len(items) {
		start := idx
		isSp := items[idx].ch == ' '
		for idx < len(items) && (items[idx].ch == ' ') == isSp {
			idx++
		}
		chunks = append(chunks, chunk{items: items[start:idx], isSpace: isSp})
	}

	renderItems := func(its []fieldItem) string {
		var b strings.Builder
		for _, it := range its {
			if !it.isCur {
				b.WriteRune(it.ch)
				continue
			}
			switch fv.Mode {
			case vimline.ModeNormal:
				b.WriteString(keySelectBg + keySelectFg + string(it.ch) + keyReset)
			case vimline.ModeReplace:
				b.WriteString(keyStBusy + "\x1b[4m" + string(it.ch) + keyReset)
			default:
				b.WriteString(keyAccentFg + "\x1b[4m" + string(it.ch) + keyReset)
			}
		}
		return b.String()
	}

	var lines []string
	var curLineItems []fieldItem
	linePrefix := prefix
	curCol := prefixPlainLen
	isFirstChunk := true

	for _, chk := range chunks {
		chkLen := len(chk.items)
		// If chunk itself is longer than available line width, break by characters
		if !chk.isSpace && curCol+chkLen > effectiveWidth && chkLen > (effectiveWidth-indentLen) {
			for _, item := range chk.items {
				if curCol+1 > effectiveWidth && !isFirstChunk {
					lines = append(lines, linePrefix+renderItems(curLineItems))
					curLineItems = nil
					linePrefix = keyOverlay + indent + keyReset
					curCol = indentLen
				}
				curLineItems = append(curLineItems, item)
				curCol++
				isFirstChunk = false
			}
			continue
		}

		if curCol+chkLen > effectiveWidth && !isFirstChunk {
			if chk.isSpace {
				hasCur := false
				for _, it := range chk.items {
					if it.isCur {
						hasCur = true
						break
					}
				}
				if !hasCur {
					continue
				}
			}
			lines = append(lines, linePrefix+renderItems(curLineItems))
			curLineItems = append([]fieldItem(nil), chk.items...)
			linePrefix = keyOverlay + indent + keyReset
			curCol = indentLen + chkLen
		} else {
			curLineItems = append(curLineItems, chk.items...)
			curCol += chkLen
		}
		isFirstChunk = false
	}

	if len(curLineItems) > 0 || len(lines) == 0 {
		lines = append(lines, linePrefix+renderItems(curLineItems))
	}
	if fv.Pending != 0 && len(lines) > 0 {
		lines[len(lines)-1] += keyOverlay + "  " + string(fv.Pending) + keyReset
	}
	return strings.Join(lines, "\n")
}

func wrapWordsPlain(text, prefix, indent string, width int) string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return prefix
	}
	var lines []string
	var curLine strings.Builder
	curLine.WriteString(prefix)
	curCol := len([]rune(stripANSI(prefix)))

	for _, w := range words {
		wLen := len([]rune(w))
		if curCol+1+wLen > width && curLine.Len() > 0 {
			lines = append(lines, curLine.String())
			curLine.Reset()
			curLine.WriteString(indent)
			curCol = len([]rune(indent))
		}
		if curCol > len([]rune(indent)) && curLine.Len() > 0 && curCol > len([]rune(stripANSI(prefix))) {
			curLine.WriteString(" ")
			curCol++
		}
		curLine.WriteString(keyText + w + keyReset)
		curCol += wLen
	}
	if curLine.Len() > 0 {
		lines = append(lines, curLine.String())
	}
	return strings.Join(lines, "\n")
}

// hintFooter paints key names in accent and their hint words dim, the way the
// built-in goto footer does.
func hintFooter(pairs ...string) string {
	parts := make([]string, 0, (len(pairs)+1)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, keyAccentFg+pairs[i]+keyReset+keyOverlay+" "+pairs[i+1]+keyReset)
	}
	return strings.Join(parts, keyOverlay+"   "+keyReset)
}

// fitLine clips a rendered line to w display columns without breaking an ANSI
// sequence, so a line can never wrap inside a narrow overlay pane.
func fitLine(s string, w int) string {
	rs := []rune(s)
	var out strings.Builder
	count := 0
	for i := 0; i < len(rs); i++ {
		if rs[i] == 0x1b {
			j := i
			for j < len(rs) && rs[j] != 'm' {
				j++
			}
			if j < len(rs) {
				out.WriteString(string(rs[i : j+1]))
				i = j
				continue
			}
			out.WriteString(string(rs[i:]))
			return out.String()
		}
		if count >= w {
			out.WriteString(keyReset)
			return out.String()
		}
		out.WriteRune(rs[i])
		count++
	}
	return out.String()
}

// helpEntry is one key binding and what it does.
type helpEntry struct {
	key  string
	desc string
}

func allHelpEntries() []helpEntry {
	return []helpEntry{
		{"enter", "focus / switch to selected pane directly"},
		{"j / k / arrows", "move one row, or N rows with a count (e.g. 3j)"},
		{"gg / G", "jump to first / last row"},
		{"g i / g l / g r / g f / g m / g a", "jump to Inbox, Later, Recent, Favorites, Most used, All"},
		{"left / right", "switch tabs"},
		{"ctrl-d / ctrl-u", "scroll half-page down / up"},
		{"e", "mark done. Leaves current list. z undoes"},
		{"z / u", "undo the last e, returning thread to queue"},
		{"i", "teach Jev: prefer, demote, pick features, or type a rule"},
		{"space", "in teach: toggle the highlighted feature chip"},
		{"J", "Jev dashboard: inspect and delete learned weights, see preview"},
		{"h", "set, change, or cancel a reminder"},
		{"n", "sticky note on selected thread, edited with vim keys"},
		{"!", "toggle manual blocked override on selected thread"},
		{"m", "mark or unmark favorite star (*)"},
		{"b w i d a", "filter status: blocked, working, idle, done, all"},
		{"/", "search tab names and sticky notes, with vim editing"},
		{"left / right", "switch tabs. h and l do not"},
		{"s", "on All and Favorites: cycle sort order"},
		{"t c / t r / t x", "create, revise, or delete a custom tab"},
		{"?", "open or close this searchable help overlay"},
		{"esc", "insert to normal, normal to browse, clears search, closes"},
		{"ctrl+c", "close from anywhere"},
		{"jj / esc", "leave insert mode in any text field"},
		{"w / b / e", "move by word forward / backward / end in text field"},
		{"0 / $", "jump to start / end of line in text field"},
		{"x / X", "delete character under / before cursor in text field"},
		{"r / s", "replace character / substitute in text field"},
		{"d / c / y", "delete, change, yank with motion in text field"},
		{"p / P", "paste after / before in text field"},
		{"u / ctrl+r", "undo / redo in text field"},
		{".", "repeat last edit in text field"},
	}
}

func filterHelpEntries(all []helpEntry, query string) []helpEntry {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return all
	}
	var out []helpEntry
	for _, kv := range all {
		if strings.Contains(strings.ToLower(kv.key), q) || strings.Contains(strings.ToLower(kv.desc), q) {
			out = append(out, kv)
		}
	}
	return out
}

// renderHelp draws the ? overlay with windowed scrolling so the highlighted
// selection is always visible and never cut off past the screen height.
func renderHelp(width, height int, fv vimline.View, sel, scrollOffset int) string {
	maxLines := height
	if height > 6 {
		maxLines = height - 1
	}
	sc := newScreen(width, maxLines, 8)
	entries := filterHelpEntries(allHelpEntries(), fv.Text)
	total := len(entries)
	if sel >= total {
		sel = max(0, total-1)
	}
	if sel < 0 {
		sel = 0
	}

	posStr := ""
	if total > 0 {
		posStr = fmt.Sprintf("  (%d/%d)", sel+1, total)
	}
	sc.emit(keyAccentFg + keyBold + "? " + keyReset + keyText + "Help" + keyReset + keyOverlay + posStr + keyReset)
	if fv.Text != "" || fv.Focused {
		sc.emit(renderField(fv, sc.width))
	} else {
		sc.emit(keyOverlay + "  / to search keys" + keyReset)
	}
	sc.rule()
	headerN := len(sc.lines)
	footerN := 2 // separator + footer hint line
	visible := maxLines - headerN - footerN
	if visible < 1 {
		visible = 1
	}

	start := scrollOffset
	if sel < start {
		start = sel
	}
	if sel >= start+visible {
		start = sel - visible + 1
	}
	if start < 0 {
		start = 0
	}
	if maxStart := max(0, total-visible); start > maxStart {
		start = maxStart
	}
	end := start + visible
	if end > total {
		end = total
	}

	for i := start; i < end; i++ {
		kv := entries[i]
		if i == sel {
			sc.emit(keySelectBg + keySelectFg + keyBold + pad("  "+pad(kv.key, 18)+"  "+kv.desc, sc.width) + keyReset)
		} else {
			sc.emit("  " + keyAccentFg + pad(kv.key, 18) + keyReset + keyText + kv.desc + keyReset)
		}
	}
	if total == 0 {
		sc.emit(keyOverlay + "  no keys match" + keyReset)
	}

	sc.rule()
	if fv.Focused {
		sc.emit(hintFooter("enter", "select", "↑↓", "move", "ctrl+u", "clear", "esc", "normal", "esc", "browse"))
	} else {
		sc.emit(hintFooter("j/k", "move", "/", "search", "esc", "close"))
	}
	return sc.done()
}

// filterRows narrows the frozen list to the query without changing its order.
type jevItem struct {
	feature string
	value   float64
	section string // "FIRST", "BURY", "PROJ", "RULE"
	ruleID  string
}

func jevSelectableItems(st *store) []jevItem {
	if st.Jev == nil || (len(st.Jev.Weights) == 0 && len(st.Jev.Rules) == 0) {
		return nil
	}
	allWeights := sortedJevWeights(st)
	var pos, neg, labels, rules []jevItem
	for _, w := range allWeights {
		if strings.HasPrefix(w.feature, "label:") {
			labels = append(labels, jevItem{feature: w.feature, value: w.value, section: "PROJ"})
		} else if w.value >= 0 {
			pos = append(pos, jevItem{feature: w.feature, value: w.value, section: "FIRST"})
		} else {
			neg = append(neg, jevItem{feature: w.feature, value: w.value, section: "BURY"})
		}
	}
	for _, rule := range st.Jev.Rules {
		rules = append(rules, jevItem{
			feature: rule.Text,
			value:   rule.Sign * rule.Delta,
			section: "RULE",
			ruleID:  rule.ID,
		})
	}
	var out []jevItem
	out = append(out, pos...)
	out = append(out, neg...)
	out = append(out, labels...)
	out = append(out, rules...)
	return out
}

// renderJev is the Jev dashboard. All learned weights are selectable and
// windowed so the highlighted row is always visible and never cut off by height.
func renderJev(width, height int, names []string, active int, items []jevItem, sel, scrollOffset int, totalExamples int, inboxTop []jevRowScore) string {
	maxLines := height
	if height > 6 {
		maxLines = height - 1
	}
	sc := newScreen(width, maxLines, 10)
	sc.header(names, active)

	total := len(items)
	if total == 0 {
		sc.emit(keyText + keyBold + "  Jev dashboard" + keyReset)
		sc.emit("")
		sc.emit(keyOverlay + "  Nothing learned yet." + keyReset)
		sc.emit(keyOverlay + "  Press i on a row. Check the features that matter." + keyReset)
		sc.emit(keyOverlay + "  Type the rule in that dialog, such as billing over docs." + keyReset)
		sc.rule()
		sc.emit(hintFooter("esc", "back"))
		return sc.done()
	}

	if sel >= total {
		sel = max(0, total-1)
	}
	if sel < 0 {
		sel = 0
	}

	sc.emit(keyText + keyBold + "  Jev dashboard" + keyReset + keyOverlay + fmt.Sprintf("  (%d/%d)", sel+1, total) + "  " + fmt.Sprintf("%d actions taught these", totalExamples) + keyReset)
	sc.rule()
	headerN := len(sc.lines)

	showPreview := maxLines >= 26 && len(inboxTop) > 0
	previewN := 0
	if showPreview {
		previewN = 2 + minInt(3, len(inboxTop))
	}
	footerN := 2 // rule + hintFooter
	visible := maxLines - headerN - footerN - previewN
	if visible < 3 {
		visible = 3
	}

	start := scrollOffset
	if sel < start {
		start = sel
	}
	if sel >= start+visible {
		start = sel - visible + 1
	}
	if start < 0 {
		start = 0
	}
	if maxStart := max(0, total-visible); start > maxStart {
		start = maxStart
	}
	end := start + visible
	if end > total {
		end = total
	}

	for i := start; i < end; i++ {
		item := items[i]
		sc.emit(jevItemLine(item, i == sel, sc.width))
	}

	if showPreview {
		sc.rule()
		sc.emit(keyAccentFg + keyBold + "  INBOX PREVIEW (why top threads rank first)" + keyReset)
		for _, r := range inboxTop[:minInt(3, len(inboxTop))] {
			score := fmt.Sprintf("%+.2f", r.score)
			color := keyStGood
			if r.score < 0 {
				color = keyStBad
			}
			sc.emit("  " + color + pad(score, 7) + keyReset + keyText + pad(r.label, 24) + keyReset + keyOverlay + r.reason + keyReset)
		}
	}

	sc.rule()
	sc.emit(hintFooter("enter", "clear one", "esc", "back"))
	return sc.done()
}

func jevItemLine(item jevItem, selected bool, width int) string {
	val := fmt.Sprintf("%+.2f", item.value)
	desc := featureDescription(item.feature)
	var tag string
	switch item.section {
	case "FIRST":
		tag = keyStGood + "[FIRST] " + keyReset
	case "BURY":
		tag = keyStBad + "[BURY]  " + keyReset
	case "RULE":
		tag = keyAccentFg + "[RULE]  " + keyReset
	default:
		tag = keyAccentFg + "[PROJ]  " + keyReset
	}
	if selected {
		plainTag := "[" + item.section + "] "
		bar := weightBarPlain(item.value, 14)
		return keySelectBg + keySelectFg + keyBold + pad("  "+val+" "+bar+"  "+plainTag+desc, width) + keyReset
	}
	color := keyStGood
	if item.value < 0 {
		color = keyStBad
	}
	return "  " + color + pad(val, 7) + keyReset + weightBar(item.value, 14) + "  " + tag + keyText + desc + keyReset
}

func weightBarPlain(value float64, width int) string {
	if width < 4 {
		return ""
	}
	filled := int(abs64(value) * float64(width-2))
	if filled > width-2 {
		filled = width - 2
	}
	var b strings.Builder
	for i := 0; i < width-2; i++ {
		if i < filled {
			b.WriteString("█")
		} else {
			b.WriteString("░")
		}
	}
	return b.String()
}

func weightBar(value float64, width int) string {
	if width < 4 {
		return ""
	}
	filled := int(abs64(value) * float64(width-2))
	if filled > width-2 {
		filled = width - 2
	}
	color := keyStGood
	if value < 0 {
		color = keyStBad
	}
	var b strings.Builder
	b.WriteString(color)
	for i := 0; i < width-2; i++ {
		if i < filled {
			b.WriteString("█")
		} else {
			b.WriteString("░")
		}
	}
	b.WriteString(keyReset)
	return b.String()
}

// featureDescription turns a feature key into what it means to a human.
func featureDescription(f string) string {
	kind, val, ok := strings.Cut(f, ":")
	if !ok {
		return f
	}
	switch kind {
	case "status":
		switch val {
		case "":
			return "no agent status"
		case "blocked":
			return "blocked agents (need your input)"
		case "working":
			return "working agents (running now)"
		case "done":
			return "finished agents (check results)"
		case "idle":
			return "idle agents (waiting)"
		}
		return val + " agents"
	case "age":
		switch val {
		case "fresh":
			return "fresh threads (under 10 min)"
		case "hour":
			return "threads from the last hour"
		case "day":
			return "threads from today"
		case "old":
			return "older threads"
		case "none":
			return "threads you never opened"
		}
		return val
	case "label", "tok", "domain":
		return val
	case "agent":
		return val + " agent"
	case "repo":
		return val + " repository"
	case "visit":
		switch val {
		case "heavy":
			return "frequently visited threads"
		case "frequent":
			return "regularly visited threads"
		case "low":
			return "rarely visited threads"
		case "zero":
			return "unvisited threads"
		}
	case "attention":
		switch val {
		case "waiting_on_human":
			return "stuck waiting for you"
		case "hot":
			return "touched in the last 5 min"
		}
	case "cadence":
		return val + " triage"
	case "favorite":
		return "starred favorite threads"
	case "remind":
		if val == "due" {
			return "overdue reminders"
		}
		if val == "yes" {
			return "threads with a reminder set"
		}
		return val
	case "note":
		if val == "yes" {
			return "threads with a sticky note"
		}
		return val
	case "manual":
		if val == "yes" {
			return "manually blocked threads"
		}
		return val
	}
	return f
}

// jevRowScore pairs an inbox row with its Jev score and the human-readable
// reason it ranked where it did.
type jevRowScore struct {
	label  string
	score  float64
	reason string
}

func inboxTopScored(s *snapshot, st *store, n int) []jevRowScore {
	rows := rowsFor(s, st, view{Kind: "inbox"}, sortDefault)
	if len(rows) > n {
		rows = rows[:n]
	}
	var out []jevRowScore
	for _, r := range rows {
		score := st.jevPriority(r, time.Now().Unix())
		var parts []string
		for _, w := range sortedJevWeights(st) {
			if len(parts) >= 2 {
				break
			}
			if strings.HasPrefix(w.feature, "label:") && w.feature != "label:"+strings.Fields(r.Label)[0] {
				continue
			}
			if featureApplies(w.feature, r) {
				parts = append(parts, featureDescription(w.feature))
			}
		}
		reason := strings.Join(parts, ", ")
		if reason == "" {
			reason = "Jev score only"
		}
		out = append(out, jevRowScore{label: clip(r.Label, 22), score: score, reason: reason})
	}
	return out
}

func featureApplies(feature string, r row) bool {
	for _, f := range jevFeatures(r, time.Now().Unix()) {
		if f == feature {
			return true
		}
	}
	return false
}

// renderJevType is the free-text Jev feedback input. Type part of a feature
// name, see matching features, then + to prefer or - to demote.
func renderJevType(width, height int, names []string, active int, query string, weights []jevWeight) string {
	maxLines := height
	if height > 6 {
		maxLines = height - 1
	}
	sc := newScreen(width, maxLines, 10)
	sc.header(names, active)
	sc.emit(keyText + keyBold + "  Teach Jev" + keyReset)
	sc.emit(keyOverlay + "  Type a feature, then + to prefer or - to demote" + keyReset)
	sc.emit("")
	sc.emit(renderField(vimline.New(query).View("feature", true), sc.width))
	sc.emit("")
	headerN := len(sc.lines)
	footerN := 2 // rule + hintFooter
	visible := maxLines - headerN - footerN
	if visible < 2 {
		visible = 2
	}
	q := strings.ToLower(query)
	matched := 0
	for _, w := range weights {
		if q != "" && !strings.Contains(strings.ToLower(w.feature), q) && !strings.Contains(strings.ToLower(featureDescription(w.feature)), q) {
			continue
		}
		val := fmt.Sprintf("%+.2f", w.value)
		color := keyStGood
		if w.value < 0 {
			color = keyStBad
		}
		sc.emit("  " + color + pad(val, 7) + keyReset + keyText + featureDescription(w.feature) + keyReset)
		matched++
		if matched >= visible {
			break
		}
	}
	if matched == 0 && q != "" {
		sc.emit(keyOverlay + "  new feature: " + query + keyReset)
	}
	sc.rule()
	sc.emit(hintFooter("+", "prefer", "-", "demote", "esc", "cancel"))
	return sc.done()
}

// jevApplyFeedback turns dashboard text into weights or conditional rules.
// A bare word uses the known vocabulary. A sentence such as
// "billing over docs" is a comparison, not one label.
func jevApplyFeedback(st *store, text string, up bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	adj, rules := parseJevFeedback(text, nil)
	if len(adj) == 0 && len(rules) == 0 {
		feat := resolveJevToken(strings.ToLower(text), nil)
		if feat == "" {
			feat = "label:" + strings.ToLower(text)
		}
		sign := -0.5
		if up {
			sign = 0.5
		}
		adj = map[string]float64{feat: sign}
	} else if !jevTextHasPolarity(text) && !up {
		for f, d := range adj {
			adj[f] = -abs64(d)
		}
		for i := range rules {
			rules[i].Sign = -1
		}
	}
	st.applyJevDirective(adj, rules)
}

type jevWeight struct {
	feature string
	value   float64
}

func sortedJevWeights(st *store) []jevWeight {
	if st.Jev == nil || len(st.Jev.Weights) == 0 {
		return nil
	}
	out := make([]jevWeight, 0, len(st.Jev.Weights))
	for f, v := range st.Jev.Weights {
		if v != 0 {
			out = append(out, jevWeight{feature: f, value: v})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		ai, aj := abs64(out[i].value), abs64(out[j].value)
		if ai != aj {
			return ai > aj
		}
		return out[i].feature < out[j].feature
	})
	return out
}

func abs64(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func filterByState(rows []row, state string) []row {
	if state == "" {
		return rows
	}
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		if statusWord(r.Status) == state {
			out = append(out, r)
		}
	}
	return out
}

func filterRows(rows []row, query string) []row {
	q := strings.ToLower(query)
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Label), q) || strings.Contains(strings.ToLower(r.Note), q) {
			out = append(out, r)
		}
	}
	return out
}

// pinSel returns the index of paneID in rows, or sel clamped into rows.
func pinSel(rows []row, sel int, paneID string) int {
	for i, r := range rows {
		if r.PaneID == paneID {
			return i
		}
	}
	if sel >= len(rows) {
		sel = len(rows) - 1
	}
	if sel < 0 {
		sel = 0
	}
	return sel
}

// anchorSel returns the index in next of the pane the cursor holds in cur.
// With no row at sel the fallback pane pins the selection.
func anchorSel(cur, next []row, sel int, fallback string) int {
	id := fallback
	if sel >= 0 && sel < len(cur) {
		id = cur[sel].PaneID
	}
	return pinSel(next, sel, id)
}

func formatLabelWithNote(label, note, remind string, labelW int, selected, due bool) string {
	if note == "" && remind == "" {
		return pad(clipTail(label, max(1, labelW-1)), labelW)
	}
	suffixPlain := ""
	if remind != "" {
		suffixPlain = " · " + remind
	}
	if note != "" {
		suffixPlain += " · " + note
	}
	maxName := max(4, labelW-len([]rune(suffixPlain))-1)
	if maxName < 4 {
		maxName = 4
	}
	clippedName := clipTail(label, maxName)
	rem := labelW - len([]rune(clippedName)) - 3
	if rem < 4 {
		return pad(clipTail(label, max(1, labelW-1)), labelW)
	}
	if selected {
		return pad(clippedName+suffixPlain, labelW)
	}
	var b strings.Builder
	b.WriteString(clippedName)
	if remind != "" {
		color := keyAccentFg
		if due {
			color = keyStBad
		}
		shown := clip(remind, rem)
		b.WriteString(keySub + " · " + color + shown)
		rem -= len([]rune(shown)) + 3
	}
	if note != "" && rem >= 4 {
		b.WriteString(keySub + " · " + clip(note, rem-3))
	}
	b.WriteString(keyText)
	return pad(b.String(), labelW)
}

func remindBadge(r row, now int64) string {
	if r.RemindAt <= 0 {
		return ""
	}
	if r.RemindAt <= now {
		return "!"
	}
	return "~"
}

// remindWhen is the 12-hour phrase for a reminder. short fits a list cell.
// full is the sentence in the h dialog.
func remindWhen(at int64, now time.Time) (short, full string) {
	if at <= 0 {
		return "", "No reminder set"
	}
	t := time.Unix(at, 0).In(now.Location())
	clock := t.Format("3:04 PM")
	rel := t.Sub(now)
	y, m, d := now.Date()
	start := time.Date(y, m, d, 0, 0, 0, 0, now.Location())
	sameDay := !t.Before(start) && t.Before(start.AddDate(0, 0, 1))
	tomorrow := !t.Before(start.AddDate(0, 0, 1)) && t.Before(start.AddDate(0, 0, 2))
	switch {
	case rel <= 0:
		late := -rel
		var lateText string
		switch {
		case late < time.Hour:
			lateText = fmt.Sprintf("overdue %dm", max(1, int(late.Minutes())))
		case late < 24*time.Hour:
			lateText = fmt.Sprintf("overdue %dh", max(1, int(late.Hours())))
		default:
			lateText = fmt.Sprintf("overdue %dd", max(1, int(late.Hours()/24)))
		}
		return lateText, "Overdue. It was due at " + clock
	case rel < time.Hour:
		mins := max(1, int(rel.Minutes()))
		s := fmt.Sprintf("in %dm", mins)
		return s, s + ", at " + clock
	case sameDay:
		return clock, "Today at " + clock
	case tomorrow:
		return "tmr " + clock, "Tomorrow at " + clock
	case rel < 7*24*time.Hour:
		day := t.Format("Mon")
		return day + " " + clock, day + " at " + clock
	default:
		date := t.Format("Jan 2")
		return date + " " + clock, date + " at " + clock
	}
}

func remindSummary(at int64, mode string, now time.Time) string {
	_, full := remindWhen(at, now)
	if at <= 0 {
		return full
	}
	if mode == "regardless" {
		return full + ". Stays hidden until then."
	}
	return full + ". Returns early if the agent replies."
}

// screen is the shared frame builder every dialog uses.
type screen struct {
	lines  []string
	width  int
	height int
}

func newScreen(width, height, minHeight int) *screen {
	if height < minHeight {
		height = minHeight
	}
	return &screen{width: width, height: height}
}

func (sc *screen) emit(line string) {
	for _, l := range strings.Split(line, "\n") {
		sc.lines = append(sc.lines, fitLine(l, sc.width))
	}
}

func (sc *screen) header(names []string, active int) {
	sc.emit(tabBar(names, active))
	sc.rule()
}

func (sc *screen) rule() {
	sc.emit(keySurface + strings.Repeat("─", max(8, sc.width-1)) + keyReset)
}

func (sc *screen) done() string {
	if len(sc.lines) > sc.height {
		sc.lines = sc.lines[:sc.height]
	}
	return paint(sc.lines)
}

// dialog: header, body closure, footer rule, hints.
func dialog(width, height, minHeight int, names []string, active int, body func(*screen), hints string) string {
	sc := newScreen(width, height, minHeight)
	sc.header(names, active)
	body(sc)
	sc.rule()
	var pairs []string
	for _, tok := range strings.Split(hints, ",") {
		tok = strings.TrimSpace(tok)
		if f := strings.Fields(tok); len(f) >= 2 {
			pairs = append(pairs, f[0], strings.Join(f[1:], " "))
		} else if len(f) == 1 {
			pairs = append(pairs, f[0], "")
		}
	}
	sc.emit(hintFooter(pairs...))
	return sc.done()
}

// renderFrame draws one screenful. Exposed via --render so screenshots and
// tests use the exact bytes a live session would show.
func tabBar(names []string, active int) string {
	var b strings.Builder
	for i, name := range names {
		if i == active {
			b.WriteString(keySelectBg + keySelectFg + keyBold + " " + name + " " + keyReset)
		} else {
			b.WriteString(keyOverlay + " " + name + " " + keyReset)
		}
		if i < len(names)-1 {
			b.WriteString(" ")
		}
	}
	return b.String()
}

func renderFrame(rows []row, sel, scrollOffset int, fv vimline.View, status string, width, height int, names []string, active int, metricKind, state string, sortOpts ...string) string {
	sortPill := ""
	if len(sortOpts) > 0 {
		sortPill = sortOpts[0]
	} else if active >= 0 && active < len(names) {
		switch names[active] {
		case "Inbox":
			sortPill = ""
		case "Favorites":
			sortPill = "Starred"
		case "Most used":
			sortPill = "Most used"
		default:
			sortPill = "Most recent"
		}
	}
	// The tab bar, hint, and search line are painted first and never scroll
	// away. Capping at height-1 keeps the last line from triggering terminal
	// auto-scroll on long tabs like All.
	if height < 6 {
		height = 6
	}
	maxLines := height
	if height > 6 {
		maxLines = height - 1
	}
	var lines []string
	emit := func(line string) { lines = append(lines, fitLine(line, width)) }
	emit(tabBar(names, active))
	leftSearch := renderField(fv, width)
	if !fv.Focused && state != "" {
		leftSearch = renderField(vimline.View{Label: "/", Focused: false}, width) +
			keyOverlay + "  " + statusColor(state) + "● " + state + keyReset
	}
	if sortPill != "" {
		pillPlain := " sort: " + sortPill + " "
		pillStyled := keySurface + "[" + keyReset + keyOverlay + "sort: " + keyAccentFg + keyBold + sortPill + keyReset + keySurface + "]" + keyReset
		leftLen := len([]rune(stripANSI(leftSearch)))
		pillLen := len([]rune(pillPlain))
		if gap := width - leftLen - pillLen - 1; gap >= 2 {
			leftSearch += strings.Repeat(" ", gap) + pillStyled
		} else {
			leftSearch += "  " + pillStyled
		}
	}
	emit(leftSearch)
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	headerN := len(lines)
	footerN := 3 // separator plus two hint lines
	if status != "" {
		footerN++
	}
	visible := maxLines - headerN - footerN
	if visible < 1 {
		visible = 1
	}
	start := scrollOffset
	if sel < start {
		start = sel
	}
	if sel >= start+visible {
		start = sel - visible + 1
	}
	if start < 0 {
		start = 0
	}
	if maxStart := max(0, len(rows)-visible); start > maxStart {
		start = maxStart
	}
	lastSection := ""
	// Column budget: a narrow overlay keeps the status dot and a short visit
	// count on the same row as a clipped label; a wide one gets the full form.
	narrow := width < 80
	starW, dotW, statusW, metaW := 2, 2, 10, 22
	if narrow {
		// The name is the point of the picker. A narrow pane keeps the coloured
		// dot and a short "2m ago", and gives every remaining column to the label.
		statusW, metaW = 0, 8
	}
	labelW := width - starW - dotW - statusW - metaW
	if labelW < 4 {
		labelW = 4
	}
	now := time.Now().Unix()
	shown := 0
	for i, r := range rows {
		if i < start {
			continue
		}
		if shown >= visible {
			break
		}
		if r.Section != "" && r.Section != lastSection {
			if shown+1 >= visible {
				break
			}
			emit(sectionColor(r.Section) + keyBold + "  " + r.Section + keyReset)
			lastSection = r.Section
			shown++
		}
		starCell := "  "
		if r.Fav {
			starCell = keyStar + "*" + keyReset + " "
		}
		word := statusWord(r.Status)
		glyph, scol := statusMark(r.Status)
		// Prefix k puts the status dot immediately left of the name. The word
		// stays in its own column, and any sticky note sits dim beside the name.
		remind := ""
		if r.RemindAt > 0 {
			remind, _ = remindWhen(r.RemindAt, time.Unix(now, 0))
		}
		labelCell := formatLabelWithNote(r.Label, r.Note, remind, labelW, i == sel, r.RemindAt > 0 && r.RemindAt <= now)
		dotCell := scol + glyph
		if i == sel {
			dotCell += keySelectFg + " "
		} else {
			dotCell += keyReset + " "
		}
		var statusCell string
		switch {
		case r.Done && !r.ManualBlocked:
			// Triage state outranks the agent word: this is what e just did.
			mark := keyStGood + "\u2713 done"
			if i == sel {
				statusCell = pad(mark+keySelectFg, statusW)
			} else {
				statusCell = pad(mark+keyReset, statusW)
			}
		case statusW == 0 || word == "":
			statusCell = strings.Repeat(" ", statusW)
		case i == sel:
			statusCell = pad(scol+word+keySelectFg, statusW)
		default:
			statusCell = pad(scol+word+keyReset, statusW)
		}
		metaCol := metricText(r, width, metricKind)
		if i == sel {
			span := keySelectBg + keySelectFg + keyBold
			line := span
			if r.Fav {
				line += keyStar + "*" + span + " "
			} else {
				line += "  "
			}
			line += dotCell + labelCell + statusCell + metaCol
			emit(pad(line, width) + keyReset)
		} else {
			emit(starCell + dotCell + keyText + labelCell + statusCell + keyText + metaCol + keyReset)
		}
		shown++
	}
	if len(rows) == 0 {
		switch {
		case len(names) > 0 && names[active] == "Inbox":
			emit(keyOverlay + "  Inbox zero. All active threads are triaged." + keyReset)
		case len(names) > 0 && names[active] == "Favorites":
			emit(keyOverlay + "  Nothing pinned. Press m on a pane to mark it." + keyReset)
		case len(names) > 0 && names[active] == "Later":
			emit(keyOverlay + "  No reminders. Press h on a pane to set one." + keyReset)
		default:
			emit(keyOverlay + "  No panes in this tab." + keyReset)
		}
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	if fv.Focused {
		emit(hintFooter("enter", "jump", "j/k", "move", "esc", "normal", "esc", "browse", "ctrl+c", "close"))
	} else {
		emit(hintFooter("enter", "switch", "e", "done", "i", "teach", "J", "jev", "h", "remind", "n", "note", "!", "block", "/", "search"))
	}
	extra := "arrows tabs   g i/l/r tab   m mark   z undo   b/w/i/d/a states   t c/r/x tab   ? help"
	if len(names) > 0 && (names[active] == "All" || names[active] == "Favorites") {
		extra += "   s sort"
	}
	if !fv.Focused && (strings.TrimSpace(fv.Text) != "" || state != "") {
		extra += "   esc clear"
	}
	emit(keyOverlay + extra + keyReset)
	if status != "" {
		emit(keySub + status + keyReset)
	}
	if len(lines) > maxLines {
		lines = lines[:maxLines]
	}
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

func paint(lines []string) string {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	for i, line := range lines {
		if i > 0 {
			b.WriteString("\r\n")
		}
		b.WriteString(line)
	}
	return b.String()
}

func formSummary(sort string, limit int, only, where string) string {
	n := "every"
	if limit > 0 {
		n = fmt.Sprintf("the %d", limit)
	}
	which := "panes"
	if only == "favorites" {
		which = "favorites"
	}
	place := ""
	if where == "here" {
		place = " in this workspace"
	}
	return fmt.Sprintf("Shows %s %s%s, %s.", n, which, place, strings.ToLower(sortLabel(canonicalSort(sort, ""))))
}

func choiceLines(labels []string, selected, width int) []string {
	var lines []string
	var b strings.Builder
	used := 2
	flush := func() {
		if b.Len() == 0 {
			return
		}
		lines = append(lines, b.String())
		b.Reset()
		used = 2
	}
	for i, label := range labels {
		chip := " " + label + " "
		gap := 0
		if used > 2 {
			gap = 2
		}
		if used+gap+len(chip) > width-1 && used > 2 {
			flush()
		}
		if used == 2 {
			b.WriteString("  ")
		} else {
			b.WriteString("  ")
			used += 2
		}
		if i == selected {
			b.WriteString(keySelectBg + keySelectFg + keyBold + chip + keyReset)
		} else {
			b.WriteString(keyOverlay + chip + keyReset)
		}
		used += len(chip)
	}
	flush()
	if len(lines) == 0 {
		return []string{""}
	}
	return lines
}

func fieldTitle(label string, focused bool) string {
	if focused {
		return keyAccentFg + keyBold + "  " + label + keyReset
	}
	return keyOverlay + "  " + label + keyReset
}

func sortLabels() []string  { return []string{"Most recent", "Oldest", "Most used", "Least used"} }
func limitLabels() []string { return []string{"10", "20", "50", "All"} }
func onlyLabels() []string  { return []string{"Any", "Favorites"} }
func whereLabels() []string { return []string{"Anywhere", "This workspace"} }

func sortOpts() []string  { return []string{"newest", "oldest", "most", "least"} }
func onlyOpts() []string  { return []string{"", "favorites"} }
func whereOpts() []string { return []string{"", "here"} }

func indexOf(opts []string, cur string) int {
	for i, o := range opts {
		if o == cur {
			return i
		}
	}
	return 0
}

func sortChoice(sort string) int {
	return indexOf(sortOpts(), sort)
}

func limitChoice(limit int) int {
	switch limit {
	case 10:
		return 0
	case 50:
		return 2
	case 0:
		return 3
	default:
		return 1
	}
}

func onlyChoice(only string) int   { return indexOf(onlyOpts(), only) }
func whereChoice(where string) int { return indexOf(whereOpts(), where) }

func cycle(n, i, dir int) int {
	return (i + dir + n) % n
}

func stepFormValue(field, dir int, sort string, limit int, only, where string) (string, int, string, string) {
	switch field {
	case 1:
		opts := sortOpts()
		sort = opts[cycle(len(opts), sortChoice(sort), dir)]
	case 2:
		opts := []int{10, 20, 50, 0}
		limit = opts[cycle(len(opts), limitChoice(limit), dir)]
	case 3:
		opts := onlyOpts()
		only = opts[cycle(len(opts), onlyChoice(only), dir)]
	case 4:
		opts := whereOpts()
		where = opts[cycle(len(opts), whereChoice(where), dir)]
	}
	return sort, limit, only, where
}

// renderForm is the criteria editor. Every choice is on screen. j/k moves
// between fields. h/l changes the highlighted choice. Letters type the name.
func renderForm(width, height int, names []string, active, field int, editing bool, fv vimline.View, sort string, limit int, only, where, status string) string {
	title := "New tab"
	if editing {
		title = "Edit tab"
	}
	sc := newScreen(width, height, 8)
	sc.header(names, active)
	sc.emit(keyText + keyBold + "  " + title + keyReset)
	sc.emit(keyOverlay + "  " + formSummary(sort, limit, only, where) + keyReset)
	sc.emit(fieldTitle("Name", field == 0))
	sc.emit(renderField(fv, sc.width))
	sc.emit(fieldTitle("Sort", field == 1))
	for _, line := range choiceLines(sortLabels(), sortChoice(sort), sc.width) {
		sc.emit(line)
	}
	sc.emit(fieldTitle("How many", field == 2))
	for _, line := range choiceLines(limitLabels(), limitChoice(limit), sc.width) {
		sc.emit(line)
	}
	sc.emit(fieldTitle("Pinned", field == 3))
	for _, line := range choiceLines(onlyLabels(), onlyChoice(only), sc.width) {
		sc.emit(line)
	}
	sc.emit(fieldTitle("Workspace", field == 4))
	for _, line := range choiceLines(whereLabels(), whereChoice(where), sc.width) {
		sc.emit(line)
	}
	sc.rule()
	sc.emit(hintFooter("j/k", "field", "h/l", "choice", "enter", "save", "esc", "cancel"))
	if status != "" {
		sc.emit(keySub + status + keyReset)
	}
	return sc.done()
}

// renderConfirm asks before a custom tab is removed. Cancel is selected.
func renderConfirm(width, height int, names []string, active, sel int, name string) string {
	return dialog(width, height, 8, names, active, func(sc *screen) {
		sc.emit("")
		sc.emit(keyText + keyBold + "  Delete " + name + "?" + keyReset)
		sc.emit(keyOverlay + "  The panes stay. Only this tab is removed." + keyReset)
		sc.emit("")
		for _, line := range choiceLines([]string{"Delete", "Cancel"}, sel, sc.width) {
			sc.emit(line)
		}
	}, "h/l choose,enter confirm,esc back")
}

func remindModeLabels() []string { return []string{"If no reply", "Regardless"} }
func remindModeOpts() []string   { return []string{"", "regardless"} }

func remindTimeField(has bool) int {
	if has {
		return 3
	}
	return 2
}

func remindPickField(has bool) int {
	if has {
		return 2
	}
	return 1
}

func remindModeField(has bool) int {
	if has {
		return 1
	}
	return 0
}

func remindActionLabels(has bool) []string {
	if has {
		return []string{"Update", "Cancel"}
	}
	return nil
}

// renderRemind is the reminder dialog. A set reminder is shown first, with
// Update and Cancel. A new reminder starts on Quick picks so enter sets 20min.
func renderRemind(width, height int, names []string, active, field, mode, action, pick int, picks []string, fv vimline.View, label string, has bool, existingAt int64, now time.Time) string {
	modeField, pickField, timeField := 0, 1, 2
	if has {
		modeField, pickField, timeField = 1, 2, 3
	}
	return dialog(width, height, 16, names, active, func(sc *screen) {
		sc.emit("")
		sc.emit(keyText + keyBold + "  Remind me about " + label + keyReset)
		sc.emit(keyOverlay + "  " + remindSummary(existingAt, remindModeOpts()[mode], now) + keyReset)
		sc.emit("")
		if has {
			sc.emit(fieldTitle("Reminder", field == 0))
			for _, line := range choiceLines(remindActionLabels(true), action, sc.width) {
				sc.emit(line)
			}
		}
		sc.emit(fieldTitle("Return to Inbox", field == modeField))
		for _, line := range choiceLines(remindModeLabels(), mode, sc.width) {
			sc.emit(line)
		}
		sc.emit(fieldTitle("Quick picks", field == pickField))
		for _, line := range choiceLines(picks, pick, sc.width) {
			sc.emit(line)
		}
		sc.emit(fieldTitle("Custom time", field == timeField))
		sc.emit(renderField(fv, sc.width))
		switch _, ok, desc := duration.Parse(fv.Text, now); {
		case ok:
			sc.emit(keyStGood + "  -> " + desc + keyReset)
		case strings.TrimSpace(fv.Text) == "":
			sc.emit(keyOverlay + "  -> type a time: 3h 2w 1d12h tomorrow, or clear" + keyReset)
		default:
			sc.emit(keyStBad + "  -> not a time. Units: min h d w mo y" + keyReset)
		}
	}, func() string {
		if field == timeField {
			return "enter save,ctrl+p pin pick,k picks,esc back"
		}
		if field == pickField {
			return "h/l pick,enter save,x remove pick,j time,esc back"
		}
		if has && field == 0 {
			return "h/l choice,enter do it,x cancel reminder,j next,esc back"
		}
		return "h/l choice,j picks,enter save,esc back"
	}())
}

func renderNote(width, height int, names []string, active int, fv vimline.View) string {
	return dialog(width, height, 8, names, active, func(sc *screen) {
		sc.emit("")
		sc.emit(keyText + keyBold + "  Sticky note" + keyReset)
		sc.emit(keyOverlay + "  Shows beside the tab name so you remember what this thread is waiting on." + keyReset)
		sc.emit("")
		sc.emit(renderField(fv, sc.width))
	}, "enter save,ctrl+u clear,esc cancel")
}

func teachPrefLabels() []string { return []string{"Prefer (+)", "Neutral", "Demote (-)"} }

type teachFeat struct {
	key string
	on  bool
}

func teachDefaultOn(f string, st *store) bool {
	kind, _, _ := strings.Cut(f, ":")
	switch kind {
	case "cadence", "visit", "age", "ws":
		return false
	case "tok", "label", "repo":
		return st == nil || !st.jevCommon(f)
	default:
		return true
	}
}

func teachFeatureChoices(r row, st *store, now int64) []teachFeat {
	var on, off []teachFeat
	for _, f := range jevFeatures(r, now) {
		item := teachFeat{key: f, on: teachDefaultOn(f, st)}
		if item.on {
			on = append(on, item)
		} else {
			off = append(off, item)
		}
	}
	return append(on, off...)
}

func featureChipLabel(f string) string {
	kind, val, ok := strings.Cut(f, ":")
	if !ok {
		return f
	}
	switch kind {
	case "status", "agent", "domain", "repo", "label", "tok":
		return val
	case "age":
		return "age " + val
	case "visit":
		return "visits " + val
	case "attention":
		if val == "waiting_on_human" {
			return "waiting on you"
		}
		if val == "hot" {
			return "just touched"
		}
	case "cadence":
		return val
	case "note":
		return "note"
	case "manual":
		return "manual block"
	case "remind":
		if val == "due" {
			return "overdue"
		}
		return "reminder"
	case "favorite":
		return "favorite"
	}
	return val
}

func teachChipLines(feats []teachFeat, sel int, focused bool, width int, weights map[string]float64) []string {
	if len(feats) == 0 {
		return []string{keyOverlay + "  no features on this thread" + keyReset}
	}
	labels := make([]string, len(feats))
	for i, f := range feats {
		mark := "[ ]"
		if f.on {
			mark = "[x]"
		}
		w := 0.0
		if weights != nil {
			w = weights[f.key]
		}
		labels[i] = fmt.Sprintf("%s %s %+.2f", mark, featureChipLabel(f.key), w)
	}
	highlight := sel
	if !focused {
		highlight = -1
	}
	return choiceLines(labels, highlight, width)
}

func enabledTeachFeatures(feats []teachFeat) []string {
	var out []string
	for _, f := range feats {
		if f.on {
			out = append(out, f.key)
		}
	}
	return out
}

// renderTeach draws the 'i' teach dialog: preference, feature chips, and free text.
func renderTeach(width, height int, names []string, active int, r row, field, pref int, feats []teachFeat, featSel int, fv vimline.View, st *store, now int64) string {
	weights := map[string]float64{}
	if st != nil && st.Jev != nil {
		weights = st.Jev.Weights
	}
	return dialog(width, height, 22, names, active, func(sc *screen) {
		sc.emit("")
		sc.emit(keyText + keyBold + "  Teach Jev: " + keyReset + keyAccentFg + keyBold + r.Label + keyReset + keyOverlay + " (" + r.PaneID + ")" + keyReset)

		statusStr := statusWord(r.Status)
		glyph, scol := statusMark(r.Status)
		meta := "  " + scol + glyph + " " + statusStr + keyReset
		if r.Note != "" {
			meta += keyOverlay + "  note: " + keyReset + keyText + r.Note + keyReset
		}
		priority := st.jevPriority(r, now)
		scoreColor := keyStGood
		if priority < 0 {
			scoreColor = keyStBad
		}
		meta += keyOverlay + "  priority: " + keyReset + scoreColor + fmt.Sprintf("%+.2f", priority) + keyReset
		sc.emit(meta)
		sc.emit(keyOverlay + "  Checked chips get the plus or minus. Text can name a finer rule." + keyReset)
		sc.emit("")
		sc.emit(fieldTitle("Preference", field == 0))
		for _, line := range choiceLines(teachPrefLabels(), pref, sc.width) {
			sc.emit(line)
		}
		sc.emit(fieldTitle("Features to guide", field == 1))
		for _, line := range teachChipLines(feats, featSel, field == 1, sc.width, weights) {
			sc.emit(line)
		}
		sc.emit(fieldTitle("Instruction (optional)", field == 2))
		sc.emit(renderField(fv, sc.width))
	}, "h/l choose,j/k field,space toggle,enter save,esc back")
}

func clipTail(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return "…" + string(r[len(r)-(n-1):])
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func pad(s string, n int) string {
	r := []rune(stripANSI(s))
	if len(r) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(r))
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b {
			for i < len(s) && s[i] != 'm' {
				i++
			}
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ---------------------------------------------------------------------------
// interactive picker
// ---------------------------------------------------------------------------

// logicalKey turns one keystroke into a name the picker understands.
// Herdr may deliver a raw byte or a CSI / kitty sequence. Both must work,

func runPicker() int {
	// Herdr writes keystrokes to this process's stdin pty. Read that fd.
	// A second /dev/tty open, plus stty size on every frame, dropped keys.
	tty := os.Stdin
	raw := exec.Command("stty", "raw", "-echo")
	raw.Stdin = tty
	_ = raw.Run()
	defer func() {
		c := exec.Command("stty", "sane")
		c.Stdin = tty
		_ = c.Run()
		tty.WriteString("\x1b[?25h")
	}()
	// The fields paint their own cursor cell, so the terminal cursor is hidden
	// for the whole session. It cannot drift and the frame is screenshot-exact.
	tty.WriteString("\x1b[?25l")

	st, err := loadStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		return 1
	}
	// Recall state is shared with hook and CLI processes. Under the flock and
	// the content fingerprint, a refused save rolls back under the lock and
	// the note never claims a save that did not happen.
	lk := &storeLock{}
	defer lk.release()
	searchEd := vimline.New(st.ActiveQuery)
	sel := 0
	status := ""
	searching := false
	stateFilter := st.ActiveState
	showHelp := false
	helpEd := vimline.New("")
	helpSearching := false
	helpSel := 0
	showJev := false
	jevSel := 0
	jevScrollOffset := 0
	helpScrollOffset := 0
	scrollOffset := 0
	jevTyping := false
	jevEd := vimline.New("")
	// The snapshot is frozen when the picker opens so j/k does not reshuffle
	// rows. Tab and sort changes reorder that frozen set on purpose.
	frozen, err := loadSnapshot()
	if err != nil {
		frozen = &snapshot{}
	}
	st.observeRows(collectRows(frozen, st), time.Now().Unix())
	tab := openTab(st)
	sortMode := st.ActiveSort
	if sortMode < 0 || sortMode >= sortCount {
		sortMode = sortDefault
	}
	creating := false
	editing := false
	editIdx := -1
	formField := 0
	confirmName := ""
	confirmSel := 1
	tabKeys := keys.New()
	tabKeys.Register("t", map[string]string{
		"c": "tab-create",
		"r": "tab-revise",
		"x": "tab-delete",
	}, "c create   r revise   x delete")
	var commitRemind func()
	reminding := false
	remindField := 1
	remindMode := 0
	remindAction := 0
	remindHas := false
	remindExisting := int64(0)
	remindSel := 0
	lastDoneID := ""
	remindEd := vimline.New("")
	remindPaneID := ""
	remindLabel := ""
	noting := false
	noteEd := vimline.New("")
	notePaneID := ""
	noteLabel := ""
	teaching := false
	teachField := 0
	teachPref := 0
	teachFeats := []teachFeat{}
	teachFeatSel := 0
	teachEd := vimline.New("")
	teachRow := row{}
	teachRank := 0
	teachTotal := 0
	formEd := vimline.New("")
	newSort, newOnly, newWhere := "newest", "", ""
	newLimit := 20
	var allRows []row
	var rows []row
	pointedID := ""
	rebuild := func() {
		vs := views(st)
		if tab >= len(vs) {
			tab = 0
		}
		if tab < 0 {
			tab = 0
		}
		allRows = rowsFor(frozen, st, vs[tab], sortMode)
	}
	visible := func() []row {
		out := filterByState(allRows, stateFilter)
		if q := strings.TrimSpace(searchEd.String()); q != "" {
			out = filterRows(out, q)
		}
		return out
	}
	rebase := func(cur []row) []row {
		if cur != nil {
			cur = visible()
		}
		rebuild()
		next := visible()
		sel = anchorSel(cur, next, sel, st.ActivePaneID)
		return next
	}
	rebuild()
	// resync adopts another writer's on-disk update before this event is
	// handled, keeping the row the captain is pointing at selected by pane ID.
	resync := func(cur []row) []row {
		if lk.acquire() != nil {
			return visible()
		}
		if fresh := refreshStore(st); fresh != st {
			st = fresh
			return rebase(cur)
		}
		return visible()
	}
	if st.ActivePaneID != "" {
		sel = pinSel(visible(), sel, st.ActivePaneID)
	}
	note := saveNote{}
	saveSt := func() bool {
		next, err := persistStore(lk, st)
		if next != st {
			st = next
			rows = rebase(rows)
		}
		if err != nil {
			status = note.refuse(err)
			return false
		}
		status = note.clear(status)
		return true
	}
	// pointed reports whether the row the drawn cursor pointed at is still
	// the row under the cursor. A key never acts on a row the last frame did
	// not draw: not the neighbor the clamp picked when the pointed row left
	// the list, and not a row another writer added after the frame. With no
	// row drawn and none now there is nothing to act on, so the key falls
	// through to its empty-list behavior.
	pointed := func() bool {
		if pointedID != "" && sel >= 0 && sel < len(rows) && rows[sel].PaneID == pointedID {
			return true
		}
		if pointedID == "" && len(rows) == 0 {
			return true
		}
		status = "list changed - review and retry"
		return false
	}
	persistView := func(list []row) {
		st.ActiveTab = tab
		st.ActiveTabSet = true
		st.ActiveSort = sortMode
		st.ActiveState = stateFilter
		st.ActiveQuery = searchEd.String()
		if sel >= 0 && sel < len(list) {
			st.ActivePaneID = list[sel].PaneID
		}
		saveSt()
	}

	// commitTeach records full real-time teaching context and applies feedback.
	commitTeach := func() {
		text := strings.TrimSpace(teachEd.String())
		now := time.Now()
		nowUnix := now.Unix()

		selected := enabledTeachFeatures(teachFeats)
		intent := "neutral"
		switch teachPref {
		case 0:
			intent = "prefer"
			st.jevPreferSelected(selected, true)
		case 2:
			intent = "demote"
			st.jevPreferSelected(selected, false)
		default:
			if text != "" {
				lower := strings.ToLower(text)
				if strings.Contains(lower, "block") || strings.Contains(lower, "wait") || strings.Contains(lower, "hold") || strings.Contains(lower, "until") || strings.Contains(lower, " over ") {
					intent = "directive"
				} else {
					intent = "feedback"
				}
			}
		}

		var ruleTexts []string
		if text != "" {
			adj, rules := parseJevFeedback(text, jevFeatures(teachRow, nowUnix))
			st.applyJevDirective(adj, rules)
			for _, rule := range rules {
				ruleTexts = append(ruleTexts, rule.Text)
			}
			if intent == "neutral" && (len(adj) > 0 || len(rules) > 0) {
				intent = "directive"
			}
		}

		cwd := ""
		for _, p := range frozen.Panes {
			if p.PaneID == teachRow.PaneID {
				cwd = p.CWD
				break
			}
		}

		queueCtx := make([]jevQueueContext, 0, len(allRows))
		for _, r := range allRows {
			queueCtx = append(queueCtx, jevQueueContext{
				PaneID:   r.PaneID,
				Label:    r.Label,
				Status:   statusWord(r.Status),
				Priority: st.jevPriority(r, nowUnix),
			})
		}

		tRecord := jevTeaching{
			ID:          fmt.Sprintf("teach-%d-%s", nowUnix, strings.ReplaceAll(teachRow.PaneID, ":", "-")),
			Timestamp:   nowUnix,
			TimeRFC3339: now.Format(time.RFC3339),
			Feedback:    text,
			Intent:      intent,
			Pane: jevPaneContext{
				PaneID:        teachRow.PaneID,
				Label:         teachRow.Label,
				Status:        statusWord(teachRow.Status),
				CWD:           cwd,
				Note:          teachRow.Note,
				ManualBlocked: teachRow.ManualBlocked,
				RemindAt:      teachRow.RemindAt,
				SnoozeMode:    teachRow.SnoozeMode,
				SnoozeState:   teachRow.SnoozeState,
				Done:          teachRow.Done,
				DoneAt:        teachRow.DoneAt,
				DoneState:     teachRow.DoneState,
				Visits:        teachRow.Visits,
				Last:          teachRow.Last,
				JevScore:      st.JevScore(teachRow.PaneID),
				JevPriority:   st.jevPriority(teachRow, nowUnix),
			},
			InboxRank:  teachRank,
			InboxTotal: teachTotal,
			Queue:      queueCtx,
			Features:   jevFeatures(teachRow, nowUnix),
			Selected:   selected,
			RuleTexts:  ruleTexts,
			Session: jevSessionContext{
				ActiveTab:    views(st)[tab].Kind,
				ActiveFilter: stateFilter,
				ActiveQuery:  searchEd.String(),
				SocketPath:   os.Getenv("HERDR_SOCKET_PATH"),
				FocusedPane:  frozen.FocusedPaneID,
			},
		}

		saved := commitTeaching(st, tRecord, saveSt)

		taughtID := teachRow.PaneID
		teaching = false
		if saved {
			if teachPref == 0 {
				status = "taught Jev: prefer " + teachRow.Label
			} else if teachPref == 2 {
				status = "taught Jev: demote " + teachRow.Label
			} else if text != "" {
				status = "taught Jev: " + clip(text, 28)
			} else {
				status = "saved teaching for " + teachRow.Label
			}
		}
		rebuild()
		sel = pinSel(visible(), sel, taughtID)
	}

	// commitRemind parses the typed time and applies the snooze contract.
	commitRemind = func() {
		due, ok, desc := duration.Parse(remindEd.String(), time.Now())
		if !ok {
			status = "not a time. Units: min h d w mo y"
			return
		}
		ps := st.ensurePane(remindPaneID)
		if due > 0 {
			state := baselineState(liveStatusWord(frozen, remindPaneID))
			ps.RemindAt = due
			ps.SnoozeMode = remindModeOpts()[remindMode]
			ps.SnoozeState = state
			ps.Done = true
			ps.DoneAt = time.Now().Unix()
			ps.DoneState = state
			st.removeFromInbox(remindPaneID)
			status = "reminder set (" + desc + "): " + remindLabel
		} else if ps.RemindAt > 0 {
			// Clear cancels the snooze and returns the thread now.
			ps.RemindAt = 0
			ps.SnoozeMode = ""
			ps.SnoozeState = ""
			ps.Done = false
			st.addToInbox(remindPaneID)
			status = "reminder cleared: " + remindLabel
		} else {
			status = "no reminder set: " + remindLabel
		}
		saveSt()
		reminding = false
		rebuild()
	}

	clearRemind := func() {
		ps := st.ensurePane(remindPaneID)
		ps.RemindAt = 0
		ps.SnoozeMode = ""
		ps.SnoozeState = ""
		ps.Done = false
		st.addToInbox(remindPaneID)
		status = "reminder cleared: " + remindLabel
		saveSt()
		reminding = false
		remindHas = false
		remindExisting = 0
		rebuild()
	}

	// activeEd reports which vim editor owns the next key, if any.
	activeEd := func() *vimline.Editor {
		if teaching && teachField == 2 {
			return teachEd
		}
		if showHelp && helpSearching {
			return helpEd
		}
		if showJev && jevTyping {
			return jevEd
		}
		if noting {
			return noteEd
		}
		if searching {
			return searchEd
		}
		if creating && formField == 0 {
			return formEd
		}
		if reminding && remindField == remindTimeField(remindHas) {
			return remindEd
		}
		return nil
	}

	// Keys arrive through a channel so a held j can wait for its pair without
	// blocking the rest of the loop. keys.Split turns one read into ordered
	// logical keys, so a burst keeps every key instead of losing the tail.
	keyCh := make(chan string, 16)
	go func() {
		buf := make([]byte, 64)
		for {
			n, err := tty.Read(buf)
			if err != nil || n == 0 {
				close(keyCh)
				return
			}
			for _, k := range keys.Split(buf[:n]) {
				keyCh <- k
			}
		}
	}()

	// Jev scores new arrivals in the background. The candidate rows are copied
	// on this thread, so the goroutine never touches shared state.
	jevCh := make(chan map[string]jevScore, 1)
	if jevEnabled() {
		cand := collectRows(frozen, st)
		nowJ := time.Now().Unix()
		var fresh []row
		for _, r := range cand {
			if !inInbox(r, nowJ) {
				continue
			}
			if st.Jev != nil && st.Jev.ScoreKey[r.PaneID] == jevRowKey(r) {
				continue
			}
			fresh = append(fresh, r)
		}
		if len(fresh) > 0 {
			go func() {
				if sc := jevScoreRows(fresh, 2500*time.Millisecond); len(sc) > 0 {
					jevCh <- sc
				}
			}()
		}
	}
	applyJev := func(sc map[string]jevScore) {
		if len(sc) == 0 {
			return
		}
		st.ensureJev()
		if st.Jev.Scores == nil {
			st.Jev.Scores = map[string]float64{}
		}
		if st.Jev.ScoreKey == nil {
			st.Jev.ScoreKey = map[string]string{}
		}
		for id, v := range sc {
			st.Jev.Scores[id] = v.noul
			st.Jev.ScoreKey[id] = v.key
		}
		st.Jev.Proxy = jevProxyBin()
		saveSt()
		rows = rebase(rows)
	}

	for {
		rows = resync(rows)
		if sel >= len(rows) {
			sel = len(rows) - 1
		}
		if sel < 0 {
			sel = 0
		}
		persistView(rows)
		w, h := termSize(tty)
		headerLines := 3
		footerLines := 3
		if status != "" {
			footerLines++
		}
		maxVisible := h
		if h > 6 {
			maxVisible = h - 1
		}
		vis := maxVisible - headerLines - footerLines
		if vis < 1 {
			vis = 1
		}
		if sel < scrollOffset {
			scrollOffset = sel
		}
		if sel >= scrollOffset+vis {
			scrollOffset = sel - vis + 1
		}
		if scrollOffset < 0 {
			scrollOffset = 0
		}
		if maxS := max(0, len(rows)-vis); scrollOffset > maxS {
			scrollOffset = maxS
		}
		pointedID = ""
		if sel < len(rows) {
			pointedID = rows[sel].PaneID
		}
		var frame string
		switch {
		case showHelp:
			frame = renderHelp(w, h, helpEd.View("/", helpSearching), helpSel, helpScrollOffset)
		case showJev:
			ex := 0
			if st.Jev != nil {
				ex = st.Jev.Examples
			}
			if jevTyping {
				frame = renderJevType(w, h, viewNames(st), tab, jevEd.Text(), sortedJevWeights(st))
			} else {
				frame = renderJev(w, h, viewNames(st), tab, jevSelectableItems(st), jevSel, jevScrollOffset, ex, inboxTopScored(frozen, st, 5))
			}
		case confirmName != "":
			frame = renderConfirm(w, h, viewNames(st), tab, confirmSel, confirmName)
		case reminding:
			frame = renderRemind(w, h, viewNames(st), tab, remindField, remindMode, remindAction, remindSel, st.getSnoozePicks(), remindEd.View("at", remindField == remindTimeField(remindHas)), remindLabel, remindHas, remindExisting, time.Now())
		case noting:
			frame = renderNote(w, h, viewNames(st), tab, noteEd.View("note", true))
		case teaching:
			frame = renderTeach(w, h, viewNames(st), tab, teachRow, teachField, teachPref, teachFeats, teachFeatSel, teachEd.View("reason", teachField == 2), st, time.Now().Unix())
		case creating:
			frame = renderForm(w, h, viewNames(st), tab, formField, editing, formEd.View("Name", formField == 0), newSort, newLimit, newOnly, newWhere, status)
		default:
			v := views(st)[tab]
			kind := "both"
			if v.Kind == "custom" {
				kind = canonicalSort(st.Tabs[v.Idx].Sort, st.Tabs[v.Idx].Dir)
			}
			frame = renderFrame(rows, sel, scrollOffset, searchEd.View("/", searching), status, w, h, viewNames(st), tab, kind, stateFilter, activeSortLabel(st, v, sortMode))
		}
		// The lock is dropped before the frame write and the wait for input,
		// so hooks never stall behind a slow terminal or an idle picker, and
		// re-taken before the next mutation.
		lk.release()
		tty.WriteString(frame)
		note.rendered(frame)

		var key string
		if ed := activeEd(); ed != nil && ed.PendingJ() {
			// One j is held. Give its pair a short window, then let it through.
			select {
			case k, ok := <-keyCh:
				if !ok {
					return 0
				}
				key = k
			case <-time.After(150 * time.Millisecond):
				ed.FlushJ()
				continue
			}
		} else {
			select {
			case k, ok := <-keyCh:
				if !ok {
					return 0
				}
				key = k
			case sc := <-jevCh:
				rows = resync(rows)
				applyJev(sc)
				continue
			}
		}
		rows = resync(rows)
		if key == "" {
			continue
		}
		if os.Getenv("RECALL_DEBUG") != "" {
			f, _ := os.OpenFile("/tmp/recall-keys.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
			if f != nil {
				fmt.Fprintf(f, "key=%q tab=%d searching=%v rows=%d sel=%d\n", key, tab, searching, len(rows), sel)
				f.Close()
			}
		}
		if key == "\x03" || key == "ctrl-c" {
			return 0
		}
		half := max(1, (h-3)/2)
		if showHelp {
			entries := filterHelpEntries(allHelpEntries(), helpEd.Text())
			total := len(entries)
			if helpSel >= total {
				helpSel = max(0, total-1)
			}
			if helpSel < 0 {
				helpSel = 0
			}
			if helpSearching {
				switch helpEd.Feed(key) {
				case vimline.ActOut:
					helpSearching = false
				case vimline.ActCancel:
					showHelp = false
					helpSearching = false
				case vimline.ActDown:
					if helpSel < total-1 {
						helpSel++
					}
				case vimline.ActUp:
					if helpSel > 0 {
						helpSel--
					}
				case vimline.ActFirst:
					helpSel = 0
				case vimline.ActLast:
					helpSel = max(0, total-1)
				case vimline.ActPageDown:
					helpSel = minInt(helpSel+half, max(0, total-1))
				case vimline.ActPageUp:
					helpSel = max(0, helpSel-half)
				}
			} else {
				switch key {
				case "/":
					helpSearching = true
					helpEd = vimline.New("")
					helpSel = 0
				case "j", "down":
					if helpSel < total-1 {
						helpSel++
					}
				case "k", "up":
					if helpSel > 0 {
						helpSel--
					}
				case "g":
					helpSel = 0
				case "G":
					helpSel = max(0, total-1)
				case "ctrl-d":
					helpSel = minInt(helpSel+half, max(0, total-1))
				case "ctrl-u":
					helpSel = max(0, helpSel-half)
				case "?", "q", "esc":
					showHelp = false
				}
			}
			continue
		}
		if showJev {
			items := jevSelectableItems(st)
			total := len(items)
			if jevSel >= total {
				jevSel = max(0, total-1)
			}
			if jevSel < 0 {
				jevSel = 0
			}
			if jevTyping {
				switch key {
				case "+", "=", "-":
					status = "teach with i on a row"
					jevTyping = false
				case "esc":
					jevTyping = false
				default:
					jevEd.Feed(key)
				}
				continue
			}
			switch key {
			case "i":
				status = "teach with i on a row"
			case "esc", "q":
				showJev = false
			case "j", "down":
				if jevSel < total-1 {
					jevSel++
				}
			case "k", "up":
				if jevSel > 0 {
					jevSel--
				}
			case "g":
				jevSel = 0
			case "G":
				jevSel = max(0, total-1)
			case "ctrl-d":
				jevSel = minInt(jevSel+half, max(0, total-1))
			case "ctrl-u":
				jevSel = max(0, jevSel-half)
			case "enter":
				if jevSel < len(items) {
					st.ensureJev()
					if items[jevSel].ruleID != "" {
						st.removeJevRule(items[jevSel].ruleID)
					} else {
						delete(st.Jev.Weights, items[jevSel].feature)
					}
					saveSt()
					items = jevSelectableItems(st)
					if jevSel >= len(items) {
						jevSel = max(0, len(items)-1)
					}
					rebuild()
				}
			}
			continue
		}
		if reminding && !showHelp {
			picks := st.getSnoozePicks()
			timeField := remindTimeField(remindHas)
			pickField := remindPickField(remindHas)
			modeField := remindModeField(remindHas)
			if remindField == timeField {
				switch key {
				case "ctrl+p":
					txt := strings.TrimSpace(remindEd.String())
					if _, ok, _ := duration.Parse(txt, time.Now()); ok {
						if st.pinSnoozePick(txt) {
							if saveSt() {
								status = "pinned quick pick: " + txt
							}
							remindField = pickField
							picks = st.getSnoozePicks()
							remindSel = indexOf(picks, txt)
						} else {
							status = "quick pick already exists: " + txt
						}
					} else {
						status = "cannot pin: not a valid time"
					}
					continue
				case "esc":
					if remindEd.Mode() == vimline.ModeInsert {
						remindEd.Feed(key)
					} else {
						remindField = pickField
					}
					continue
				case "k", "up":
					if remindEd.Mode() != vimline.ModeInsert {
						remindField = pickField
						continue
					}
				}
				switch remindEd.Feed(key) {
				case vimline.ActAccept:
					commitRemind()
				case vimline.ActUp:
					remindField = pickField
				case vimline.ActCancel:
					remindField = pickField
				}
				continue
			}

			switch key {
			case "j", "down", "tab":
				if remindField < timeField {
					remindField++
				}
			case "k", "up":
				if remindField > 0 {
					remindField--
				}
			case "h", "left":
				switch remindField {
				case 0:
					if remindHas {
						remindAction = cycle(2, remindAction, -1)
					} else {
						remindMode = cycle(len(remindModeOpts()), remindMode, -1)
					}
				default:
					if remindField == modeField {
						remindMode = cycle(len(remindModeOpts()), remindMode, -1)
					} else if remindField == pickField {
						remindSel = cycle(len(picks), remindSel, -1)
						remindEd = vimline.New(picks[remindSel])
					}
				}
			case "l", "right":
				switch remindField {
				case 0:
					if remindHas {
						remindAction = cycle(2, remindAction, 1)
					} else {
						remindMode = cycle(len(remindModeOpts()), remindMode, 1)
					}
				default:
					if remindField == modeField {
						remindMode = cycle(len(remindModeOpts()), remindMode, 1)
					} else if remindField == pickField {
						remindSel = cycle(len(picks), remindSel, 1)
						remindEd = vimline.New(picks[remindSel])
					}
				}
			case "x":
				if remindHas && remindField == 0 {
					clearRemind()
				} else if remindField == pickField && len(picks) > 1 {
					removed := picks[remindSel]
					st.removeSnoozePick(remindSel)
					if saveSt() {
						status = "removed quick pick: " + removed
					}
					picks = st.getSnoozePicks()
					if remindSel >= len(picks) {
						remindSel = len(picks) - 1
					}
					remindEd = vimline.New(picks[remindSel])
				}
			case "enter":
				if remindHas && remindField == 0 {
					if remindAction == 1 {
						clearRemind()
					} else {
						remindField = pickField
					}
				} else {
					commitRemind()
				}
			case "esc", "q":
				reminding = false
				status = ""
			}
			continue
		}
		if teaching && !showHelp {
			switch key {
			case "esc":
				if teachField == 2 && teachEd.Mode() == vimline.ModeInsert {
					teachEd.Feed(key)
					continue
				}
				if teachField > 0 {
					teachField--
					continue
				}
				teaching = false
				status = ""
				continue
			case "enter":
				if teachField == 2 {
					if teachEd.Feed(key) == vimline.ActAccept {
						commitTeach()
					}
					continue
				}
				commitTeach()
				continue
			case "j", "down", "tab":
				if teachField == 2 && teachEd.Mode() == vimline.ModeInsert {
					teachEd.Feed(key)
					continue
				}
				if teachField < 2 {
					teachField++
				}
				continue
			case "k", "up":
				if teachField == 2 && teachEd.Mode() == vimline.ModeInsert {
					teachEd.Feed(key)
					continue
				}
				if teachField > 0 {
					teachField--
				}
				continue
			}
			if teachField == 0 {
				switch key {
				case "h", "left":
					teachPref = cycle(3, teachPref, -1)
				case "l", "right":
					teachPref = cycle(3, teachPref, 1)
				}
				continue
			}
			if teachField == 1 {
				n := len(teachFeats)
				switch key {
				case "h", "left":
					if n > 0 {
						teachFeatSel = cycle(n, teachFeatSel, -1)
					}
				case "l", "right":
					if n > 0 {
						teachFeatSel = cycle(n, teachFeatSel, 1)
					}
				case " ", "x":
					if n > 0 && teachFeatSel >= 0 && teachFeatSel < n {
						teachFeats[teachFeatSel].on = !teachFeats[teachFeatSel].on
					}
				case "a":
					allOn := true
					for _, f := range teachFeats {
						if !f.on {
							allOn = false
							break
						}
					}
					for i := range teachFeats {
						teachFeats[i].on = !allOn
					}
				}
				continue
			}
			switch teachEd.Feed(key) {
			case vimline.ActAccept:
				commitTeach()
			case vimline.ActCancel, vimline.ActUp:
				teachField = 1
			}
			continue
		}
		if noting && !showHelp {
			switch noteEd.Feed(key) {
			case vimline.ActAccept:
				status = commitNote(st, notePaneID, noteLabel, noteEd.String())
				saveSt()
				noting = false
				rebuild()
			case vimline.ActOut, vimline.ActCancel:
				noting = false
				status = ""
			}
			continue
		}
		if searching && !showHelp {
			switch searchEd.Feed(key) {
			case vimline.ActAccept:
				if !pointed() {
					break
				}
				if len(rows) > 0 {
					target := rows[sel].PaneID
					lk.release()
					tty.WriteString("\x1b[2J\x1b[H")
					if err := jumpTo(target); err != nil {
						status = "jump: " + err.Error()
						continue
					}
					return 0
				}
			case vimline.ActOut:
				// Level two of the ladder: leave the field, keep the filter.
				searching = false
			case vimline.ActCancel:
				return 0
			case vimline.ActUp:
				if sel > 0 {
					sel--
				}
			case vimline.ActDown:
				if sel < len(rows)-1 {
					sel++
				}
			case vimline.ActFirst:
				sel = 0
			case vimline.ActLast:
				sel = max(0, len(rows)-1)
			case vimline.ActPageUp:
				sel = max(0, sel-half)
			case vimline.ActPageDown:
				sel = min(sel+half, max(0, len(rows)-1))
			}
			continue
		}
		if confirmName != "" && !showHelp {
			switch key {
			case "left", "h":
				confirmSel = cycle(2, confirmSel, -1)
			case "right", "l":
				confirmSel = cycle(2, confirmSel, 1)
			case "enter":
				if confirmSel == 0 {
					v := views(st)[tab]
					if v.Kind == "custom" {
						st.Tabs = append(st.Tabs[:v.Idx], st.Tabs[v.Idx+1:]...)
						if saveSt() {
							status = "deleted " + confirmName
						}
						tab = 0
						rebuild()
					}
				}
				confirmName = ""
			case "esc":
				confirmName = ""
			}
			continue
		}
		if creating && !showHelp {
			saveForm := func() {
				name := strings.TrimSpace(formEd.String())
				if name == "" {
					formField = 0
					formEd.SetMode(vimline.ModeInsert)
					status = "Type a name, then press enter."
					return
				}
				ct := customTab{Name: name, Sort: canonicalSort(newSort, ""), Limit: newLimit, Only: newOnly, Where: newWhere}
				created := false
				if editing && editIdx >= 0 && editIdx < len(st.Tabs) {
					ct.Fixed = st.Tabs[editIdx].Fixed
					st.Tabs[editIdx] = ct
					status = "saved " + name
				} else {
					st.Tabs = append(st.Tabs, ct)
					status = "created " + name
					created = true
				}
				if saveSt() && created {
					tab = 0
					for i, v := range views(st) {
						if v.Kind == "custom" && v.Idx == len(st.Tabs)-1 {
							tab = i
							break
						}
					}
				}
				creating = false
				editing = false
				sel = 0
				rebuild()
			}
			if formField == 0 {
				switch formEd.Feed(key) {
				case vimline.ActAccept:
					saveForm()
				case vimline.ActOut:
					formField = 1
				case vimline.ActCancel:
					creating = false
					editing = false
					status = ""
				case vimline.ActUp:
					formField = 0
				case vimline.ActDown:
					formField = 1
				}
				continue
			}
			switch key {
			case "esc":
				creating = false
				editing = false
				status = ""
			case "enter":
				saveForm()
			case "j", "down":
				if formField < 4 {
					formField++
				}
				status = ""
			case "k", "up":
				if formField > 0 {
					formField--
				}
				status = ""
			case "h", "left":
				newSort, newLimit, newOnly, newWhere = stepFormValue(formField, -1, newSort, newLimit, newOnly, newWhere)
				status = ""
			case "l", "right":
				newSort, newLimit, newOnly, newWhere = stepFormValue(formField, 1, newSort, newLimit, newOnly, newWhere)
				status = ""
			}
			continue
		}
		// Tab mutations and tab jumps live behind the shared chord dispatcher.
		registerGoChords(tabKeys, st)
		if a, r := tabKeys.Feed(key); r != keys.Pass {
			if r == keys.Consumed {
				if p, hint, ok := tabKeys.Pending(); ok {
					if p == "g" {
						status = "go: " + hint
					} else {
						status = "tab: " + hint
					}
				} else {
					status = ""
				}
				continue
			}
			if a.Name == "goto-top" {
				sel = 0
				status = ""
				continue
			}
			if idx, ok := strings.CutPrefix(a.Name, "goto:"); ok {
				n, err := strconv.Atoi(idx)
				vs := views(st)
				if err == nil && n >= 0 && n < len(vs) {
					tab = n
					sel = 0
					scrollOffset = 0
					status = viewName(st, vs[n])
					rebuild()
				}
				continue
			}
			v := views(st)[tab]
			switch a.Name {
			case "tab-create":
				creating = true
				editing = false
				editIdx = -1
				formField = 0
				formEd = vimline.New("")
				newSort, newOnly, newWhere, newLimit = "newest", "", "", 20
				status = ""
			case "tab-revise":
				if v.Kind != "custom" {
					break
				}
				ct := st.Tabs[v.Idx]
				creating = true
				editing = true
				editIdx = v.Idx
				formField = 1
				formEd = vimline.New(ct.Name)
				newSort, newOnly, newWhere, newLimit = canonicalSort(ct.Sort, ct.Dir), ct.Only, ct.Where, ct.Limit
				status = ""
			case "tab-delete":
				if v.Kind != "custom" {
					break
				}
				confirmName = st.Tabs[v.Idx].Name
				confirmSel = 1
			}
			continue
		}
		browseCount := tabKeys.TakeCount()
		switch {
		case key == "esc":
			// esc unwinds one thing per press, the same ladder the field
			// uses: the search words go first, then the status filter, then
			// the picker closes.
			if strings.TrimSpace(searchEd.Text()) != "" {
				searchEd = vimline.New("")
				status = ""
				sel = 0
				continue
			}
			if stateFilter != "" {
				stateFilter = ""
				status = ""
				sel = 0
				continue
			}
			return 0 // esc closes from browse mode
		case key == "up":
			if sel > 0 {
				sel--
			}
		case key == "down":
			if sel < len(rows)-1 {
				sel++
			}
		case key == "enter":
			if !pointed() {
				break
			}
			if dbgEnter := os.Getenv("RECALL_DEBUG"); dbgEnter != "" {
				f, _ := os.OpenFile("/tmp/recall-enter.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
				if f != nil {
					fmt.Fprintf(f, "ENTER tab=%d kind=%s rows=%d sel=%d\n", tab, views(st)[tab].Kind, len(rows), sel)
					if len(rows) > 0 && sel < len(rows) {
						fmt.Fprintf(f, "  target=%s label=%s\n", rows[sel].PaneID, rows[sel].Label)
					}
					f.Close()
				}
			}
			if len(rows) == 0 {
				return 0
			}
			target := rows[sel].PaneID
			lk.release()
			tty.WriteString("\x1b[2J\x1b[H")
			if err := jumpTo(target); err != nil {
				// keep the frame open with the reason instead of failing silently
				status = "jump: " + err.Error()
				continue
			}
			return 0
		case key == "?" && !searching:
			showHelp = true
			helpSel = 0
			helpScrollOffset = 0
			helpSearching = false
			helpEd = vimline.New("")
		case key == "J" && !searching && !showHelp:
			showJev = true
			jevSel = 0
			jevScrollOffset = 0
			jevTyping = false
			jevEd = vimline.New("")
		case (key == "left" || key == "right") && !searching && !showHelp:
			nTabs := len(views(st))
			if nTabs < 1 {
				nTabs = 1
			}
			if key == "left" {
				tab = (tab + nTabs - 1) % nTabs
			} else {
				tab = (tab + 1) % nTabs
			}
			sel = 0
			scrollOffset = 0
			rebuild()
		case key == "s" && !searching && !showHelp:
			v := views(st)[tab]
			if v.Kind != "all" && v.Kind != "favorites" {
				break
			}
			if v.Kind == "all" && sortMode == sortDefault {
				sortMode = sortOldest
			} else {
				sortMode = (sortMode + 1) % sortCount
				if v.Kind == "all" && sortMode == sortDefault {
					sortMode = sortNewest
				}
			}
			sel = 0
			status = "sort: " + activeSortLabel(st, v, sortMode)
			rebuild()
		case key == "i" && !searching && !showHelp && !showJev:
			if !pointed() {
				break
			}
			if len(rows) > 0 && sel < len(rows) {
				teaching = true
				teachField = 0
				teachPref = 0
				teachRow = rows[sel]
				teachFeats = teachFeatureChoices(teachRow, st, time.Now().Unix())
				teachFeatSel = 0
				teachRank = sel
				teachTotal = len(rows)
				teachEd = vimline.New("")
			}
		case key == "e" && !searching && !showHelp:
			if !pointed() {
				break
			}
			if len(rows) == 0 {
				break
			}
			r := rows[sel]
			ps := st.ensurePane(r.PaneID)
			now := time.Now().Unix()
			doneID := ""
			if views(st)[tab].Kind == "inbox" || ps.InInbox || inInbox(r, now) {
				ps.Done = true
				ps.DoneAt = now
				ps.ManualBlocked = false
				ps.RemindAt = 0
				ps.SnoozeMode = ""
				ps.SnoozeState = ""
				ps.DoneState = baselineState(liveStatusWord(frozen, r.PaneID))
				st.removeFromInbox(r.PaneID)
				// e archives. It is neutral, so it does not teach Jev.
				status = "done: " + r.Label + " (z undo)"
				doneID = r.PaneID
			} else {
				ps.Done = false
				st.addToInbox(r.PaneID)
				status = "moved to inbox: " + r.Label
			}
			if saveSt() {
				lastDoneID = doneID
			}
			rebuild()
		case (key == "z" || key == "u") && !searching && !showHelp:
			// z is undo. u stays as the vim alias.
			if lastDoneID == "" {
				break
			}
			if st.Panes[lastDoneID] == nil {
				break
			}
			status = "undone"
			lastDoneID = undoDone(st, lastDoneID, saveSt)
			rebuild()
		case key == "h" && !searching && !showHelp:
			if !pointed() {
				break
			}
			if len(rows) == 0 {
				break
			}
			reminding = true
			remindAction = 0
			remindSel = 0
			psRemind := st.ensurePane(rows[sel].PaneID)
			remindHas = psRemind.RemindAt > 0
			remindExisting = psRemind.RemindAt
			if psRemind.SnoozeMode == "regardless" {
				remindMode = 1
			} else {
				remindMode = 0
			}
			if remindHas {
				remindField = 0
			} else {
				remindField = 1
			}
			picks := st.getSnoozePicks()
			if len(picks) > 0 {
				remindEd = vimline.New(picks[0])
			} else {
				remindEd = vimline.New("")
			}
			remindPaneID = rows[sel].PaneID
			remindLabel = rows[sel].Label
			status = ""
		case key == "n" && !searching && !showHelp:
			if !pointed() {
				break
			}
			if len(rows) == 0 {
				break
			}
			noting = true
			notePaneID = rows[sel].PaneID
			noteLabel = rows[sel].Label
			noteEd = vimline.New(rows[sel].Note)
			status = ""
		case key == "!" && !searching && !showHelp:
			if !pointed() {
				break
			}
			if len(rows) == 0 {
				break
			}
			r := rows[sel]
			ps := st.ensurePane(r.PaneID)
			ps.ManualBlocked = !ps.ManualBlocked
			if ps.ManualBlocked {
				ps.Done = false
				st.addToInbox(r.PaneID)
				status = "marked blocked: " + r.Label
			} else {
				status = "cleared manual blocked: " + r.Label
			}
			saveSt()
			rebuild()
		case key == "/" && !searching:
			searching = true
			searchEd.SetMode(vimline.ModeInsert)
			searchEd.End()
			stateFilter = ""
			sel = 0
		case key == "g" && !searching:
			sel = 0
		case key == "G" && !searching:
			sel = max(0, len(rows)-1)
		case (key == "j" || key == "down") && !searching:
			n := browseCount
			if n < 1 {
				n = 1
			}
			sel += n
			if sel > len(rows)-1 {
				sel = len(rows) - 1
			}
			if sel < 0 {
				sel = 0
			}
		case (key == "k" || key == "up") && !searching:
			n := browseCount
			if n < 1 {
				n = 1
			}
			sel -= n
			if sel < 0 {
				sel = 0
			}
		case key == "ctrl-n" || (key == "down" && searching):
			if sel < len(rows)-1 {
				sel++
			}
		case key == "ctrl-p" || (key == "up" && searching):
			if sel > 0 {
				sel--
			}
		case !searching && !showHelp && (key == "b" || key == "w" || key == "i" || key == "d"):
			stateFilter = map[string]string{"b": "blocked", "w": "working", "i": "idle", "d": "done"}[key]
			searchEd = vimline.New("")
			searching = false
			sel = 0
		case key == "a" && !searching && !showHelp:
			stateFilter = ""
			searchEd = vimline.New("")
		case key == "m" && !searching:
			if !pointed() {
				break
			}
			if len(rows) > 0 {
				id := rows[sel].PaneID
				if st.toggleFavorite(id) {
					status = "marked favorite: " + rows[sel].Label
				} else {
					status = "unmarked favorite: " + rows[sel].Label
				}
				saveSt()
				rebuild()
			}
		case key == "ctrl-u":
			if sel > half {
				sel -= half
			} else {
				sel = 0
			}
		case key == "ctrl-d" && !searching:
			if sel+half < len(rows) {
				sel += half
			}
			if sel >= len(rows) {
				sel = max(0, len(rows)-1)
			}
		case key == "bs":
			if stateFilter != "" {
				stateFilter = ""
				sel = 0
			}
		case key == "q":
			return 0
		}
	}
}

// ---------------------------------------------------------------------------
// entrypoints
// ---------------------------------------------------------------------------

func openPicker() int {
	// Popup is a session modal, like prefix+k: it floats over the current view
	// and does not change the tab layout. Exiting the process dismisses it, so
	// no pane close or tab close is ever issued. The CLI help omits popup, but
	// the 0.9.3 server implements it (width/height are popup-only flags).
	if _, err := herdr("plugin", "pane", "open", "--plugin", pluginID, "--entrypoint", "picker", "--placement", "popup", "--width", "95%", "--height", "95%"); err != nil {
		fmt.Fprintf(os.Stderr, "recall: open picker: %v\n", err)
		return 1
	}
	return 0
}

func renderOnce() int {
	st, err := loadStore()
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: %v\n", err)
		return 1
	}
	snap, err := loadSnapshot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: snapshot: %v\n", err)
		return 1
	}
	fv := vimline.New(os.Getenv("RECALL_QUERY")).View("/", os.Getenv("RECALL_SEARCH") == "1")
	if os.Getenv("RECALL_SEARCH") != "1" {
		fv = vimline.View{Label: "/", Text: os.Getenv("RECALL_QUERY"), Focused: false}
	}
	rows := rowsFor(snap, st, view{Kind: "all"}, sortDefault)
	if fv.Text != "" {
		rows = filterRows(rows, fv.Text)
	}
	sel := 0
	if v := os.Getenv("RECALL_SEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			sel = n
		}
	}
	w, h := 80, 24
	if v := os.Getenv("RECALL_W"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			w = n
		}
	}
	if v := os.Getenv("RECALL_H"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			h = n
		}
	}
	os.Stdout.WriteString(renderFrame(rows, sel, 0, fv, os.Getenv("RECALL_STATUS"), w, h, viewNames(st), 0, "both", os.Getenv("RECALL_STATE")))
	return 0
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: herdr-recall <hook|open-picker|picker|record|jump|stats|favorite|render> [args]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "open-picker":
		os.Exit(openPicker())
	case "picker":
		for _, a := range os.Args[2:] {
			if a == "--render" {
				os.Exit(renderOnce())
			}
		}
		os.Exit(runPicker())
	case "hook":
		// any focus event: record the currently focused pane
		if _, err := recordFocus(); err != nil {
			// never fail a hook loudly; the pane is still usable
			os.Exit(0)
		}
		os.Exit(0)
	case "record":
		id, err := recordFocus()
		if err != nil {
			fmt.Fprintf(os.Stderr, "recall: record: %v\n", err)
			os.Exit(1)
		}
		fmt.Println(id)
	case "jump":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: herdr-recall jump <pane-id>")
			os.Exit(2)
		}
		if err := jumpTo(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
			os.Exit(1)
		}
	case "stats":
		st, err := loadStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
			os.Exit(1)
		}
		b, _ := json.MarshalIndent(st, "", "  ")
		fmt.Println(string(b))
	case "favorite":
		if len(os.Args) < 3 {
			os.Exit(2)
		}
		lk := &storeLock{}
		if err := lk.acquire(); err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
			os.Exit(1)
		}
		defer lk.release()
		st, err := loadStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
			os.Exit(1)
		}
		added := st.toggleFavorite(os.Args[2])
		if err := saveStore(st); err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
			os.Exit(1)
		}
		if added {
			fmt.Println("favorite added")
		} else {
			fmt.Println("favorite removed")
		}
	case "jev":
		st, err := loadStore()
		if err != nil {
			fmt.Fprintf(os.Stderr, "recall: %v\n", err)
			os.Exit(1)
		}
		if len(os.Args) < 3 {
			// list
			for _, w := range sortedJevWeights(st) {
				fmt.Printf("%+.2f  %s\n", w.value, w.feature)
			}
			if st.Jev != nil {
				fmt.Printf("\nexamples: %d, scored panes: %d\n", st.Jev.Examples, len(st.Jev.Scores))
			}
			os.Exit(0)
		}
		switch os.Args[2] {
		case "reset":
			lk := &storeLock{}
			if err := lk.acquire(); err != nil {
				fmt.Fprintf(os.Stderr, "recall: %v\n", err)
				os.Exit(1)
			}
			defer lk.release()
			st, err = loadStore()
			if err != nil {
				fmt.Fprintf(os.Stderr, "recall: %v\n", err)
				os.Exit(1)
			}
			feature := ""
			if len(os.Args) > 3 {
				feature = os.Args[3]
			}
			st.ensureJev()
			if feature != "" {
				delete(st.Jev.Weights, feature)
			} else {
				st.Jev.Weights = map[string]float64{}
			}
			if err := saveStore(st); err != nil {
				fmt.Fprintf(os.Stderr, "recall: %v\n", err)
				os.Exit(1)
			}
			if feature != "" {
				fmt.Println("cleared:", feature)
			} else {
				fmt.Println("all learnings cleared")
			}
		case "scores":
			if st.Jev != nil {
				for id, sc := range st.Jev.Scores {
					fmt.Printf("%.2f  %s\n", sc, id)
				}
			}
		default:
			fmt.Fprintf(os.Stderr, "usage: herdr-recall jev [reset [feature]|scores]\n")
			os.Exit(2)
		}
	case "render":
		os.Exit(renderOnce())
	default:
		fmt.Fprintf(os.Stderr, "recall: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
