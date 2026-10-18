// herdr-recall: a pane recall picker for Herdr bound to prefix+shift+l.
//
// It remembers which panes you actually used (recency and visit counts) and
// lets you mark favorites, then jumps to a pane from one overlay list.
// The look and key model copy Herdr's built-in goto overlay (prefix+k) rather
// than inventing a new visual language.
package main

import (
	"encoding/json"

	"fmt"
	"github.com/RooseveltAdvisors/vimline"
	"github.com/RooseveltAdvisors/vimline/duration"
	"github.com/RooseveltAdvisors/vimline/keys"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
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
	TerminalTitle      string `json:"terminal_title"`
	TerminalTitleStrip string `json:"terminal_title_stripped"`
	CWD                string `json:"cwd"`
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
	ActiveSort   int                  `json:"active_sort,omitempty"`
	ActiveState  string               `json:"active_state,omitempty"`
	ActiveQuery  string               `json:"active_query,omitempty"`
	ActivePaneID string               `json:"active_pane_id,omitempty"`
	Jev          *jevState            `json:"jev,omitempty"`
}

type view struct {
	Kind string
	Idx  int
}

func configDir() string {
	if v := os.Getenv("HERDR_PLUGIN_CONFIG_DIR"); v != "" {
		return v
	}
	out, err := herdr("plugin", "config-dir", pluginID)
	if err == nil {
		if d := strings.TrimSpace(string(out)); d != "" {
			return d
		}
	}
	// Deterministic fallback: the same encoded-id layout herdr uses.
	enc := strings.ReplaceAll(pluginID, "R", "%52")
	enc = strings.ReplaceAll(enc, "A", "%41")
	return filepath.Join(os.Getenv("HOME"), ".config", "herdr", "plugins", "config", enc)
}

func statePath() string { return filepath.Join(configDir(), "recall.json") }

func loadStore() *store {
	s := &store{Version: 1, Panes: map[string]*paneStat{}}
	b, err := os.ReadFile(statePath())
	if err != nil {
		return s
	}
	if err := json.Unmarshal(b, s); err != nil || s.Panes == nil {
		s = &store{Version: 1, Panes: map[string]*paneStat{}}
	}
	if !s.TabsInit {
		s.Tabs = defaultTabs()
		s.TabsInit = true
		_ = saveStore(s)
	}
	// Bump persisted 20 limits to 50 so Most used fills the popup.
	for i := range s.Tabs {
		if s.Tabs[i].Limit == 20 {
			s.Tabs[i].Limit = 50
		}
	}
	return s
}

func defaultTabs() []customTab {
	return []customTab{
		{Name: "Recent", Sort: "last", Limit: 50, Fixed: true},
		{Name: "Most used", Sort: "times", Limit: 50, Fixed: true},
	}
}

func views(st *store) []view {
	out := []view{{Kind: "favorites"}, {Kind: "inbox"}}
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

func openTab(st *store) int {
	vs := views(st)
	if st.ActiveTab >= 0 && st.ActiveTab < len(vs) {
		return st.ActiveTab
	}
	return 0
}

func saveStore(s *store) error {
	dir := configDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
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
	return os.Rename(tmp, statePath())
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
		ps := s.Panes[e.id]
		keepPin := ps != nil && (ps.Note != "" || ps.ManualBlocked || ps.RemindAt > 0)
		if !s.isFavorite(e.id) && !keepPin {
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
	st := loadStore()
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
	st.prune(liveSet(snap), 200)
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
		ws := ""
		for _, p := range s.Panes {
			if p.PaneID == id {
				ws = p.WorkspaceID
				break
			}
		}
		out = append(out, row{
			PaneID: id, WorkspaceID: ws, Label: label, Note: note,
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
		// Jev puts the most important new arrival first. Existing rows keep
		// their pinned positions, so reviewing never reshuffles the queue.
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
	st.InboxOrder = orderedIDs
	out := make([]row, 0, len(orderedIDs))
	for _, id := range orderedIDs {
		out = append(out, byID[id])
	}
	return out
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
	// Reference tabs show every pane, done or not, so the captain can always
	// find a thread. Only the Inbox drains on e. Done rows carry a check.
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

// renderField paints one editing line with a visible cursor cell. The real
// terminal cursor is hidden while the picker runs, so the painted cell is the
// only cursor and it cannot drift or wrap.
func renderField(fv vimline.View, width int) string {
	if !fv.Focused {
		if fv.Text == "" {
			return keyOverlay + "  " + fv.Label + " " + keyReset + keyOverlay + "search panes" + keyReset
		}
		return keyOverlay + "  " + fv.Label + " " + keyReset + keyText + fv.Text + keyReset
	}
	var b strings.Builder
	b.WriteString(modeChip(fv.Mode, fv.Pending, fv.Count))
	b.WriteString(keyAccentFg + keyBold + "  " + fv.Label + " " + keyReset)
	runes := []rune(fv.Text)
	cur := fv.Cursor
	if cur > len(runes) {
		cur = len(runes)
	}
	for i := 0; i <= len(runes); i++ {
		ch := ' '
		if i < len(runes) {
			ch = runes[i]
		}
		if i != cur {
			b.WriteRune(ch)
			continue
		}
		switch fv.Mode {
		case vimline.ModeNormal:
			b.WriteString(keySelectBg + keySelectFg + string(ch) + keyReset)
		case vimline.ModeReplace:
			b.WriteString(keyStBusy + "\x1b[4m" + string(ch) + keyReset)
		default:
			b.WriteString(keyAccentFg + "\x1b[4m" + string(ch) + keyReset)
		}
	}
	if fv.Pending != 0 {
		b.WriteString(keyOverlay + "  " + string(fv.Pending) + keyReset)
	}
	return b.String()
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
		{"j / k / arrows", "move one row, or N rows with a count"},
		{"g / G", "first row / last row"},
		{"e", "mark done. Leaves current list. u undoes"},
		{"i", "teach Jev on selected thread with context"},
		{"z", "snooze with typed time (3h, 2w, tomorrow, clear)"},
		{"n", "sticky note, edited with vim keys"},
		{"!", "toggle manual blocked override"},
		{"m", "mark or unmark favorite (*)"},
		{"b w i d a", "filter status: blocked, working, idle, done, all"},
		{"/", "search, edited with vim keys"},
		{"h / l", "switch tabs"},
		{"t c / t r / t x", "create, revise, or delete a custom tab"},
		{"s", "on All and Favorites: cycle sort"},
		{"+ / -", "tell Jev: more like this row / less like it"},
		{"J", "inspect and correct Jev learnings"},
		{"u", "undo the last e"},
		{"?", "open or close this help"},
		{"esc", "insert to normal, normal to browse, clears search, closes"},
		{"ctrl+c", "close from anywhere"},
		{"jj / esc", "leave insert mode in any text field"},
		{"w / b / e", "move by word in any text field"},
		{"0 / $", "start / end of line in any text field"},
		{"x / r / s", "delete or replace char in any text field"},
		{"d / c / y", "delete, change, yank with motion in any text field"},
		{"p / P", "paste after / before in any text field"},
		{"u / ctrl+r", "undo / redo in any text field"},
		{".", "repeat last edit in any text field"},
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
	section string // "FIRST", "BURY", "PROJ"
}

func jevSelectableItems(st *store) []jevItem {
	if st.Jev == nil || len(st.Jev.Weights) == 0 {
		return nil
	}
	allWeights := sortedJevWeights(st)
	var pos, neg, labels []jevItem
	for _, w := range allWeights {
		if strings.HasPrefix(w.feature, "label:") {
			labels = append(labels, jevItem{feature: w.feature, value: w.value, section: "PROJ"})
		} else if w.value >= 0 {
			pos = append(pos, jevItem{feature: w.feature, value: w.value, section: "FIRST"})
		} else {
			neg = append(neg, jevItem{feature: w.feature, value: w.value, section: "BURY"})
		}
	}
	var out []jevItem
	out = append(out, pos...)
	out = append(out, neg...)
	out = append(out, labels...)
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
		sc.emit(keyOverlay + "  Press + on a row to teach Jev what to surface first." + keyReset)
		sc.emit(keyOverlay + "  Press - to teach it what to bury." + keyReset)
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
	sc.emit(hintFooter("enter", "clear", "i", "teach", "D", "clear all", "esc", "back"))
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
	case "label":
		return val
	case "remind":
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
	kind, val, ok := strings.Cut(feature, ":")
	if !ok {
		return false
	}
	switch kind {
	case "status":
		return statusWord(r.Status) == val
	case "age":
		return jevAgeBucket(r, time.Now().Unix()) == feature
	case "label":
		if toks := strings.Fields(r.Label); len(toks) > 0 {
			return "label:"+toks[0] == feature
		}
	case "remind":
		return r.RemindAt > 0
	case "note":
		return r.Note != ""
	case "manual":
		return r.ManualBlocked
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

// jevApplyFeedback sets a feature weight from the typing input. A bare
// word becomes a label feature. A prefix like status: or age: is kept.
func jevApplyFeedback(st *store, text string, up bool) {
	feature := strings.TrimSpace(strings.ToLower(text))
	if feature == "" {
		return
	}
	st.ensureJev()
	for f := range st.Jev.Weights {
		if f == feature || strings.HasSuffix(f, ":"+feature) {
			feature = f
			break
		}
	}
	if !strings.Contains(feature, ":") {
		feature = "label:" + feature
	}
	sign := -0.5
	if up {
		sign = 0.5
	}
	w := st.Jev.Weights[feature] + sign
	if w > 1 {
		w = 1
	}
	if w < -1 {
		w = -1
	}
	st.Jev.Weights[feature] = w
	st.Jev.Examples++
	_ = saveStore(st)
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

func formatLabelWithNote(label, note string, labelW int, selected bool) string {
	if note == "" {
		return pad(clipTail(label, max(1, labelW-1)), labelW)
	}
	maxName := max(4, labelW/2)
	clippedName := clipTail(label, maxName)
	nameRunes := len([]rune(clippedName))
	rem := labelW - nameRunes - 3
	if rem < 4 {
		return pad(clipTail(label, max(1, labelW-1)), labelW)
	}
	clippedNote := clip(note, rem)
	if selected {
		return pad(clippedName+" · "+clippedNote, labelW)
	}
	return pad(clippedName+keySub+" · "+clippedNote+keyText, labelW)
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
	sc.lines = append(sc.lines, fitLine(line, sc.width))
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
	sc.emit(hintFooter(hints))
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
		badge := remindBadge(r, now)
		if badge != "" {
			if word != "" {
				word = badge + " " + word
			} else {
				word = badge
			}
		}
		glyph, scol := statusMark(r.Status)
		if badge == "!" && word == "!" {
			scol = keyStar
		}
		// Prefix k puts the status dot immediately left of the name. The word
		// stays in its own column, and any sticky note sits dim beside the name.
		labelCell := formatLabelWithNote(r.Label, r.Note, labelW, i == sel)
		dotCell := scol + glyph
		if i == sel {
			dotCell += keySelectFg + " "
		} else {
			dotCell += keyReset + " "
		}
		var statusCell string
		switch {
		case r.Done && badge == "" && !r.ManualBlocked:
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
		default:
			emit(keyOverlay + "  No panes in this tab." + keyReset)
		}
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	if fv.Focused {
		emit(hintFooter("enter", "jump", "j/k", "move", "esc", "normal", "esc", "browse", "ctrl+c", "close"))
	} else {
		emit(hintFooter("enter", "switch", "e", "done", "i", "teach", "z", "remind", "n", "note", "!", "block", "/", "search"))
	}
	extra := "h/l tabs   m mark   b/w/i/d/a states   t c/r/x tab"
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

func remindPicks() []string { return []string{"20min", "1h", "3h", "tomorrow", "clear"} }

func remindModeLabels() []string { return []string{"If no reply", "Regardless"} }
func remindModeOpts() []string   { return []string{"", "regardless"} }

// renderRemind is the snooze dialog. The return rule is a contract, so it is a
// field. The time is typed with the shared vim editor and the resolved
// absolute time is shown while you type, the way Superhuman does it.
func renderRemind(width, height int, names []string, active, field, mode, pick int, fv vimline.View, label string) string {
	return dialog(width, height, 14, names, active, func(sc *screen) {
		sc.emit("")
		sc.emit(keyText + keyBold + "  Remind me about " + label + keyReset)
		sc.emit(keyOverlay + "  Hides this thread from Inbox. New agent activity returns it early." + keyReset)
		sc.emit("")
		sc.emit(fieldTitle("Return to Inbox", field == 0))
		for _, line := range choiceLines(remindModeLabels(), mode, sc.width) {
			sc.emit(line)
		}
		sc.emit(fieldTitle("Quick picks", field == 1))
		for _, line := range choiceLines(remindPicks(), pick, sc.width) {
			sc.emit(line)
		}
		sc.emit(fieldTitle("Time", field == 2))
		sc.emit(renderField(fv, sc.width))
		switch _, ok, desc := duration.Parse(fv.Text, time.Now()); {
		case ok:
			sc.emit(keyStGood + "  → " + desc + keyReset)
		case strings.TrimSpace(fv.Text) == "":
			sc.emit(keyOverlay + "  → type a time: 3h 2w 1d12h tomorrow, or clear" + keyReset)
		default:
			sc.emit(keyStBad + "  → not a time. Units: min h d w mo y" + keyReset)
		}
	}, "j/k field,h/l choice,enter set reminder,esc cancel")
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

// renderTeach draws the 'i' teach dialog for the selected thread.
func renderTeach(width, height int, names []string, active int, r row, fv vimline.View, st *store, now int64) string {
	return dialog(width, height, 12, names, active, func(sc *screen) {
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

		feats := jevFeatures(r, now)
		var featDesc []string
		for _, f := range feats {
			featDesc = append(featDesc, featureDescription(f))
		}
		sc.emit(keyOverlay + "  features: " + keyReset + keyText + strings.Join(featDesc, ", ") + keyReset)
		sc.emit("")
		sc.emit(keyOverlay + "  Feedback / instruction (type + to prefer, - to demote, or reasons):" + keyReset)
		sc.emit(renderField(fv, sc.width))
	}, "enter save teaching,ctrl+u clear,esc cancel")
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

// termSize measures the pane's own tty. The overlay pane this plugin runs in
// is narrow (31 columns in the live session), so the size must come from the
// tty the picker itself holds, and a failure must never fall back to a wide
// default: a frame wider than the pane wraps and every row becomes unreadable.
func termSize(tty *os.File) (int, int) {
	var ws struct {
		Row, Col, X, Y uint16
	}
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), 0x5413, uintptr(unsafe.Pointer(&ws)))
	if errno != 0 || ws.Col < 8 || ws.Row < 4 {
		return 62, 12
	}
	return int(ws.Col), int(ws.Row)
}

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

	st := loadStore()
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
	remindField := 2
	remindMode := 0
	remindSel := 0
	lastDoneID := ""
	remindEd := vimline.New("")
	remindPaneID := ""
	remindLabel := ""
	noting := false
	noteEd := vimline.New("")
	notePaneID := ""
	teaching := false
	teachEd := vimline.New("")
	teachRow := row{}
	teachRank := 0
	teachTotal := 0
	formEd := vimline.New("")
	newSort, newOnly, newWhere := "newest", "", ""
	newLimit := 20
	var allRows []row
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
	rebuild()
	if st.ActivePaneID != "" {
		cur := filterByState(allRows, stateFilter)
		if q := strings.TrimSpace(searchEd.String()); q != "" {
			cur = filterRows(cur, q)
		}
		for i, r := range cur {
			if r.PaneID == st.ActivePaneID {
				sel = i
				break
			}
		}
	}
	persistView := func(rows []row) {
		st.ActiveTab = tab
		st.ActiveSort = sortMode
		st.ActiveState = stateFilter
		st.ActiveQuery = searchEd.String()
		if sel >= 0 && sel < len(rows) {
			st.ActivePaneID = rows[sel].PaneID
		}
		_ = saveStore(st)
	}

	// commitTeach records full real-time teaching context and applies feedback.
	commitTeach := func() {
		text := strings.TrimSpace(teachEd.String())
		if text == "" {
			teaching = false
			return
		}
		now := time.Now()
		nowUnix := now.Unix()

		intent := "feedback"
		up := false
		hasSign := false
		lower := strings.ToLower(text)
		if text == "+" || text == "=" || strings.HasPrefix(lower, "+") || strings.Contains(lower, "prefer") || strings.Contains(lower, "priority") || strings.Contains(lower, "first") || strings.Contains(lower, "up") {
			intent = "prefer"
			up = true
			hasSign = true
		} else if text == "-" || strings.HasPrefix(lower, "-") || strings.Contains(lower, "demote") || strings.Contains(lower, "bury") || strings.Contains(lower, "down") || strings.Contains(lower, "later") {
			intent = "demote"
			up = false
			hasSign = true
		} else if strings.Contains(lower, "block") || strings.Contains(lower, "wait") || strings.Contains(lower, "hold") {
			intent = "directive"
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
			Session: jevSessionContext{
				ActiveTab:    views(st)[tab].Kind,
				ActiveFilter: stateFilter,
				ActiveQuery:  searchEd.String(),
				SocketPath:   os.Getenv("HERDR_SOCKET_PATH"),
				FocusedPane:  frozen.FocusedPaneID,
			},
		}

		if hasSign {
			st.jevPrefer(teachRow, up)
		} else {
			jevApplyFeedback(st, text, true)
		}

		_ = recordJevTeaching(st, tRecord)
		_ = saveStore(st)

		teaching = false
		status = "taught Jev: " + clip(text, 28)
		rebuild()
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
		_ = saveStore(st)
		reminding = false
		rebuild()
	}

	// activeEd reports which vim editor owns the next key, if any.
	activeEd := func() *vimline.Editor {
		if teaching {
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
		if reminding && remindField == 2 {
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
		_ = saveStore(st)
		activePane := ""
		if sel >= 0 && sel < len(allRows) {
			activePane = allRows[sel].PaneID
		}
		rebuild()
		if activePane != "" {
			for i, r := range allRows {
				if r.PaneID == activePane {
					sel = i
					break
				}
			}
		}
	}

	for {
		rows := filterByState(allRows, stateFilter)
		if q := strings.TrimSpace(searchEd.String()); q != "" {
			rows = filterRows(rows, q)
		}
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
			frame = renderRemind(w, h, viewNames(st), tab, remindField, remindMode, remindSel, remindEd.View("at", remindField == 2), remindLabel)
		case noting:
			frame = renderNote(w, h, viewNames(st), tab, noteEd.View("note", true))
		case teaching:
			frame = renderTeach(w, h, viewNames(st), tab, teachRow, teachEd.View("teach", true), st, time.Now().Unix())
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
		tty.WriteString(frame)

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
				applyJev(sc)
				continue
			}
		}
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
				case "+", "=":
					jevApplyFeedback(st, jevEd.Text(), true)
					jevTyping = false
					rebuild()
				case "-":
					jevApplyFeedback(st, jevEd.Text(), false)
					jevTyping = false
					rebuild()
				case "esc":
					jevTyping = false
				default:
					jevEd.Feed(key)
				}
				continue
			}
			switch key {
			case "i":
				jevTyping = true
				jevEd = vimline.New("")
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
					delete(st.Jev.Weights, items[jevSel].feature)
					_ = saveStore(st)
					items = jevSelectableItems(st)
					if jevSel >= len(items) {
						jevSel = max(0, len(items)-1)
					}
					rebuild()
				}
			case "D":
				confirmName = "all Jev learnings"
				confirmSel = 1
				showJev = false
			}
			continue
		}
		if reminding && !showHelp {
			if remindField == 2 {
				switch remindEd.Feed(key) {
				case vimline.ActAccept:
					commitRemind()
				case vimline.ActUp:
					if remindField > 0 {
						remindField--
					}
				case vimline.ActOut, vimline.ActCancel:
					reminding = false
					status = ""
				}
				continue
			}
			picks := remindPicks()
			switch key {
			case "j", "down":
				if remindField < 2 {
					remindField++
				}
			case "k", "up":
				if remindField > 0 {
					remindField--
				}
			case "h", "left":
				if remindField == 0 {
					remindMode = cycle(len(remindModeOpts()), remindMode, -1)
				} else {
					remindSel = cycle(len(picks), remindSel, -1)
					remindEd = vimline.New(picks[remindSel])
				}
			case "l", "right":
				if remindField == 0 {
					remindMode = cycle(len(remindModeOpts()), remindMode, 1)
				} else {
					remindSel = cycle(len(picks), remindSel, 1)
					remindEd = vimline.New(picks[remindSel])
				}
			case "enter":
				commitRemind()
			case "esc":
				reminding = false
				status = ""
			}
			continue
		}
		if teaching && !showHelp {
			switch key {
			case "esc":
				if teachEd.Mode() == vimline.ModeInsert {
					teachEd.Feed(key)
				} else {
					teaching = false
					status = ""
				}
				continue
			}
			switch teachEd.Feed(key) {
			case vimline.ActAccept:
				commitTeach()
			case vimline.ActCancel:
				teaching = false
				status = ""
			}
			continue
		}
		if noting && !showHelp {
			switch noteEd.Feed(key) {
			case vimline.ActAccept:
				ps := st.ensurePane(notePaneID)
				ps.Note = strings.TrimSpace(noteEd.String())
				if ps.Note != "" {
					ps.Done = false
					status = "note saved: " + rows[sel].Label
				} else {
					status = "note cleared: " + rows[sel].Label
				}
				_ = saveStore(st)
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
				if len(rows) > 0 {
					target := rows[sel].PaneID
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
					if confirmName == "all Jev learnings" {
						st.ensureJev()
						st.Jev.Weights = map[string]float64{}
						_ = saveStore(st)
						status = "all Jev learnings cleared"
					} else {
						v := views(st)[tab]
						if v.Kind == "custom" {
							st.Tabs = append(st.Tabs[:v.Idx], st.Tabs[v.Idx+1:]...)
							_ = saveStore(st)
							tab = 0
							status = "deleted " + confirmName
							rebuild()
						}
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
				if editing && editIdx >= 0 && editIdx < len(st.Tabs) {
					ct.Fixed = st.Tabs[editIdx].Fixed
					st.Tabs[editIdx] = ct
					status = "saved " + name
				} else {
					st.Tabs = append(st.Tabs, ct)
					tab = len(views(st)) - 2
					status = "created " + name
				}
				_ = saveStore(st)
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
		// Tab mutations live behind the shared chord dispatcher, the way vim
		// gates structure behind : and tmux gates create behind prefix+c.
		if a, r := tabKeys.Feed(key); r != keys.Pass {
			if r == keys.Consumed {
				if _, hint, ok := tabKeys.Pending(); ok {
					status = "tab: " + hint
				} else {
					status = ""
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
		case (key == "left" || key == "right" || (key == "h" || key == "l") && !searching) && !showHelp:
			nTabs := len(views(st))
			if nTabs < 1 {
				nTabs = 1
			}
			if key == "left" || key == "h" {
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
			if len(rows) > 0 && sel < len(rows) {
				teaching = true
				teachRow = rows[sel]
				teachRank = sel
				teachTotal = len(rows)
				teachEd = vimline.New("")
			}
		case key == "e" && !searching && !showHelp:
			if len(rows) == 0 {
				break
			}
			r := rows[sel]
			ps := st.ensurePane(r.PaneID)
			now := time.Now().Unix()
			if views(st)[tab].Kind == "inbox" || ps.InInbox || inInbox(r, now) {
				ps.Done = true
				ps.DoneAt = now
				ps.ManualBlocked = false
				ps.RemindAt = 0
				ps.SnoozeMode = ""
				ps.SnoozeState = ""
				ps.DoneState = baselineState(liveStatusWord(frozen, r.PaneID))
				st.removeFromInbox(r.PaneID)
				if views(st)[tab].Kind == "inbox" {
					st.jevLearn(r, rows[:sel])
				}
				lastDoneID = r.PaneID
				status = "done: " + r.Label + " (u undo)"
			} else {
				ps.Done = false
				lastDoneID = ""
				st.addToInbox(r.PaneID)
				status = "moved to inbox: " + r.Label
			}
			_ = saveStore(st)
			rebuild()
		case (key == "+" || key == "=" || key == "-") && !searching && !showHelp:
			// Explicit Jev feedback: plus bumps this row's features, minus
			// decays them. The next arrival with the same features ranks
			// higher or lower.
			if len(rows) == 0 {
				break
			}
			r := rows[sel]
			up := key == "+" || key == "="
			st.jevPrefer(r, up)
			_ = saveStore(st)
			if up {
				status = "preferred: " + r.Label
			} else {
				status = "demoted: " + r.Label
			}
		case key == "u" && !searching && !showHelp:
			// Undo the last e, the way vim's u undoes the last change. The
			// row returns to the list it left.
			if lastDoneID == "" {
				break
			}
			if ps := st.Panes[lastDoneID]; ps != nil {
				ps.Done = false
				ps.DoneState = ""
				st.addToInbox(lastDoneID)
				status = "undone"
				lastDoneID = ""
				_ = saveStore(st)
				rebuild()
			}
		case key == "z" && !searching && !showHelp:
			if len(rows) == 0 {
				break
			}
			reminding = true
			remindField = 2
			remindMode = 0
			remindSel = 0
			remindEd = vimline.New("")
			remindPaneID = rows[sel].PaneID
			remindLabel = rows[sel].Label
			status = ""
		case key == "n" && !searching && !showHelp:
			if len(rows) == 0 {
				break
			}
			noting = true
			notePaneID = rows[sel].PaneID
			noteEd = vimline.New(rows[sel].Note)
			status = ""
		case key == "!" && !searching && !showHelp:
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
			_ = saveStore(st)
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
			if len(rows) > 0 {
				id := rows[sel].PaneID
				if st.toggleFavorite(id) {
					status = "marked favorite: " + rows[sel].Label
				} else {
					status = "unmarked favorite: " + rows[sel].Label
				}
				_ = saveStore(st)
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
	st := loadStore()
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
		b, _ := json.MarshalIndent(loadStore(), "", "  ")
		fmt.Println(string(b))
	case "favorite":
		if len(os.Args) < 3 {
			os.Exit(2)
		}
		st := loadStore()
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
		st := loadStore()
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
			if len(os.Args) > 3 {
				// reset one feature
				st.ensureJev()
				delete(st.Jev.Weights, os.Args[3])
				fmt.Println("cleared:", os.Args[3])
			} else {
				st.ensureJev()
				st.Jev.Weights = map[string]float64{}
				fmt.Println("all learnings cleared")
			}
			_ = saveStore(st)
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
