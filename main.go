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
	sortLast    = 1
	sortTimes   = 2
	sortOldest  = 3
	sortRarely  = 4
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
	Visits int64  `json:"visits"`
	Last   int64  `json:"last"`
	Label  string `json:"label,omitempty"`
}

type customTab struct {
	Name  string `json:"name"`
	Sort  string `json:"sort"`
	Limit int    `json:"limit"`
	Only  string `json:"only,omitempty"`
	Where string `json:"where,omitempty"`
	State string `json:"state,omitempty"`
	Fixed bool   `json:"fixed,omitempty"`
}

type store struct {
	Version   int                  `json:"version"`
	Panes     map[string]*paneStat `json:"panes"`
	Favorites []string             `json:"favorites"`
	TabsInit  bool                 `json:"tabs_init"`
	Tabs      []customTab          `json:"tabs,omitempty"`
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
	return s
}

func defaultTabs() []customTab {
	return []customTab{
		{Name: "Recent", Sort: "last", Limit: 20, Fixed: true},
		{Name: "Most used", Sort: "times", Limit: 20, Fixed: true},
	}
}

func views(st *store) []view {
	out := []view{{Kind: "all"}, {Kind: "favorites"}}
	for i := range st.Tabs {
		out = append(out, view{Kind: "custom", Idx: i})
	}
	return out
}

func viewName(st *store, v view) string {
	switch v.Kind {
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
	for i, v := range views(st) {
		if v.Kind == "custom" && st.Tabs[v.Idx].Name == "Recent" {
			return i
		}
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

func (s *store) prune(keep int) {
	if len(s.Panes) <= keep {
		return
	}
	type kv struct {
		id string
		ts int64
	}
	var all []kv
	for id, st := range s.Panes {
		all = append(all, kv{id, st.Last})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ts > all[j].ts })
	for _, e := range all[keep:] {
		if !s.isFavorite(e.id) {
			delete(s.Panes, e.id)
		}
	}
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
	ps := st.Panes[id]
	if ps == nil {
		ps = &paneStat{}
		st.Panes[id] = ps
	}
	ps.Visits++
	ps.Last = now
	if label != "" {
		ps.Label = label
	}
	st.prune(200)
	return id, saveStore(st)
}

func paneLabel(s *snapshot, id string) string {
	for _, p := range s.Panes {
		if p.PaneID != id {
			continue
		}
		title := strings.TrimSpace(p.TerminalTitleStrip)
		if title == "" {
			title = strings.TrimSpace(p.TerminalTitle)
		}
		if title != "" {
			return title
		}
		for _, t := range s.Tabs {
			if t.TabID == p.TabID && t.Label != "" {
				return t.Label
			}
		}
		for _, w := range s.Workspaces {
			if w.WorkspaceID == p.WorkspaceID && w.Label != "" {
				return w.Label
			}
		}
	}
	return id
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
	PaneID      string
	WorkspaceID string
	Label       string
	Section     string
	Visits      int64
	Last        int64
	Status      string
	Fav         bool
}

// displayLabel builds a recallable name for a pane: agent title, tab label,
// workspace label and cwd basename, de-duplicated. The pane id is used only
// when nothing else identifies the pane, and never printed twice.
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
	title := strings.TrimSpace(p.TerminalTitleStrip)
	if title == "" {
		title = strings.TrimSpace(p.TerminalTitle)
	}
	tabLabel, wsLabel := "", ""
	for _, t := range s.Tabs {
		if t.TabID == p.TabID {
			tabLabel = strings.TrimSpace(t.Label)
		}
	}
	for _, w := range s.Workspaces {
		if w.WorkspaceID == p.WorkspaceID {
			wsLabel = strings.TrimSpace(w.Label)
		}
	}
	cwd := ""
	if dir := strings.TrimSpace(p.CWD); dir != "" && dir != "/" {
		cwd = filepath.Base(dir)
	}
	var parts []string
	seen := map[string]bool{}
	add := func(v string) {
		if v == "" || v == "." || seen[v] {
			return
		}
		seen[v] = true
		parts = append(parts, v)
	}
	add(title)
	add(tabLabel)
	add(wsLabel)
	if cwd != "" {
		add("@" + cwd)
	}
	if len(parts) == 0 {
		if stored != "" {
			return stored
		}
		return id
	}
	return strings.Join(parts, " · ")
}

func tabTitle(tab int) string {
	switch tab {
	case tabMostUsed:
		return "Most used"
	case tabFavorites:
		return "Favorites"
	case tabAll:
		return "All"
	default:
		return "Recent"
	}
}

func sortLabel(tab, mode int) string {
	if mode == sortDefault {
		switch tab {
		case tabFavorites:
			return "starred order"
		case tabMostUsed:
			return "times opened"
		default:
			return "last used"
		}
	}
	switch mode {
	case sortLast:
		return "last used"
	case sortTimes:
		return "times opened"
	default:
		return "name"
	}
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
	for id := range st.Panes {
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
		var visits, last int64
		if ps != nil {
			visits, last = ps.Visits, ps.Last
		}
		ws := ""
		for _, p := range s.Panes {
			if p.PaneID == id {
				ws = p.WorkspaceID
				break
			}
		}
		out = append(out, row{
			PaneID: id, WorkspaceID: ws, Label: label,
			Visits: visits, Last: last, Status: statusOf[id],
			Fav: st.isFavorite(id),
		})
	}
	return out
}

func rowLess(a, b row, tab, mode int) bool {
	kind := mode
	if kind == sortDefault {
		switch tab {
		case tabMostUsed:
			kind = sortTimes
		case tabFavorites:
			kind = sortLast
		default:
			kind = sortLast
		}
	}
	switch kind {
	case sortLast:
		if a.Last != b.Last {
			return a.Last > b.Last
		}
	case sortTimes:
		if a.Visits != b.Visits {
			return a.Visits > b.Visits
		}
	default:
		return strings.ToLower(a.Label) < strings.ToLower(b.Label)
	}
	return strings.ToLower(a.Label) < strings.ToLower(b.Label)
}

func sortBy(list []row, how string) {
	sort.SliceStable(list, func(i, j int) bool {
		switch how {
		case "times":
			if list[i].Visits != list[j].Visits {
				return list[i].Visits > list[j].Visits
			}
		case "rarely":
			if list[i].Visits != list[j].Visits {
				return list[i].Visits < list[j].Visits
			}
		case "oldest":
			if list[i].Last != list[j].Last {
				return list[i].Last < list[j].Last
			}
		default:
			if list[i].Last != list[j].Last {
				return list[i].Last > list[j].Last
			}
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
	case "unpinned":
		if r.Fav {
			return false
		}
	}
	switch ct.Where {
	case "here":
		if here == "" || r.WorkspaceID != here {
			return false
		}
	case "elsewhere":
		if here != "" && r.WorkspaceID == here {
			return false
		}
	}
	switch ct.State {
	case "working", "blocked", "idle", "done":
		if statusWord(r.Status) != ct.State {
			return false
		}
	}
	return true
}

func rowsFor(s *snapshot, st *store, v view, mode int) []row {
	all := collectRows(s, st)
	switch v.Kind {
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
		sortBy(list, ct.Sort)
		if ct.Limit > 0 && len(list) > ct.Limit {
			list = list[:ct.Limit]
		}
		return list
	default:
		list := append([]row(nil), all...)
		sortBy(list, sortHow(mode, "last"))
		return list
	}
}

func sortHow(mode int, fallback string) string {
	switch mode {
	case sortLast:
		return "last"
	case sortTimes:
		return "times"
	case sortOldest:
		return "oldest"
	case sortRarely:
		return "rarely"
	default:
		return fallback
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
	case "last", "oldest":
		return when
	case "times", "rarely":
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
		return ""
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

// hintFooter paints key names in accent and their hint words dim, the way the
// built-in goto footer does.
func hintFooter(pairs ...string) string {
	parts := make([]string, 0, (len(pairs)+1)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		parts = append(parts, keyAccentFg+pairs[i]+keyReset+keyOverlay+" "+pairs[i+1]+keyReset)
	}
	return strings.Join(parts, keyOverlay+"   "+keyReset) + "\r\n"
}

// agoShort is the compact age form used when the overlay pane is narrow.
func agoShort(ts int64) string {
	if ts == 0 {
		return "-"
	}
	d := time.Since(time.Unix(ts, 0))
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
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

// renderHelp draws the ? overlay: every key and what it does. Esc returns to
// the list on the same row; the picker itself stays open.
func renderHelp(width, height int) string {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H")
	emit := func(line string) { b.WriteString(fitLine(line, width) + "\r\n") }
	emit(keyAccentFg + keyBold + "? " + keyReset + keyOverlay + "help - esc returns to the list" + keyReset)
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	keys := [][2]string{
		{"h / l", "switch tabs"},
		{"n", "new custom tab"},
		{"e", "edit the current custom tab"},
		{"x", "delete the current custom tab"},
		{"s", "on All and Favorites: cycle sort"},
		{"/", "search panes (esc back to the list)"},
		{"enter", "jump to the selected pane"},
		{"j / k", "move one row (list scrolls one row)"},
		{"g g", "jump to the first row"},
		{"G", "jump to the last row"},
		{"ctrl+u", "half page up (in search: clear query)"},
		{"ctrl+d", "half page down (in search: page down)"},
		{"f", "favorite or unfavorite the row"},
		{"?", "open or close this help"},
		{"esc", "close help, then close the picker"},
		{"q", "quit"},
	}
	for _, kv := range keys {
		emit("  " + keyAccentFg + pad(kv[0], 8) + keyReset + keyText + kv[1] + keyReset)
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	emit(keyOverlay + "body text stays light, status dot colours: red blocked, yellow working, teal done, green idle" + keyReset)
	return b.String()
}

// filterRows narrows the frozen list to the query without changing its order.
func filterRows(rows []row, query string) []row {
	q := strings.ToLower(query)
	out := make([]row, 0, len(rows))
	for _, r := range rows {
		if strings.Contains(strings.ToLower(r.Label), q) {
			out = append(out, r)
		}
	}
	return out
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

func renderFrame(rows []row, sel int, query string, status string, searching bool, width, height int, names []string, active int, metricKind string) string {
	// The tab bar, hint, and search line are painted first and never scroll
	// away. All is the long list, so a trailing newline on a full frame used
	// to push those headers off the popup.
	if height < 6 {
		height = 6
	}
	var lines []string
	emit := func(line string) { lines = append(lines, fitLine(line, width)) }
	emit(tabBar(names, active))
	if searching {
		prompt := keyOverlay + "search panes" + keyReset
		if query != "" {
			prompt = keyText + query + keyReset
		}
		emit(keyAccentFg + keyBold + " / " + keyReset + prompt)
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	headerN := len(lines)
	footerN := 3 // separator plus two hint lines
	if status != "" {
		footerN++
	}
	visible := height - headerN - footerN
	if visible < 1 {
		visible = 1
	}
	start := 0
	if sel >= visible {
		start = sel - visible + 1
	}
	lastSection := ""
	// Column budget: a narrow overlay keeps the status dot and a short visit
	// count on the same row as a clipped label; a wide one gets the full form.
	narrow := width < 80
	starW, statusW, metaW := 2, 12, 22
	if narrow {
		// The name is the point of the picker. A narrow pane keeps the coloured
		// dot and a short "2m ago", and gives every remaining column to the label.
		statusW, metaW = 2, 8
	}
	labelW := width - starW - statusW - metaW
	if labelW < 4 {
		labelW = 4
	}
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
		scol := statusColor(r.Status)
		// One fixed layout for every row, the way prefix+k paints it: star,
		// a label cell that always ends in a separator space, a status cell at
		// a stable column, then the counts. The selected row is painted as ONE
		// span so the accent bar covers label, status and metadata together,
		// with the dot keeping its colour inside the bar.
		labelCell := pad(clipTail(r.Label, labelW), labelW)
		var statusCell string
		switch {
		case word == "":
			statusCell = strings.Repeat(" ", statusW)
		case narrow && i == sel:
			statusCell = scol + "●" + keySelectFg + " "
		case narrow:
			statusCell = scol + "●" + keyReset + " "
		case i == sel:
			statusCell = pad(scol+"●"+keySelectFg+" "+word, statusW)
		default:
			statusCell = pad(scol+"● "+word+keyReset, statusW)
		}
		metaCol := metricText(r, width, metricKind)
		if i == sel {
			span := keySelectBg + keySelectFg + keyBold
			line := span
			if r.Fav {
				line += keyStar + "*" + span
			} else {
				line += " "
			}
			line += " " + labelCell + statusCell + " " + metaCol
			emit(pad(line, width) + keyReset)
		} else {
			emit(starCell + keyText + labelCell + statusCell + keyText + metaCol + keyReset)
		}
		shown++
	}
	if len(rows) == 0 {
		if len(names) > 0 && names[active] == "Favorites" {
			emit(keyOverlay + "  Nothing pinned. Press f on a pane to pin it." + keyReset)
		} else {
			emit(keyOverlay + "  No panes in this tab." + keyReset)
		}
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	if searching {
		if narrow {
			emit(keyAccentFg + "esc" + keyReset + keyOverlay + " back  enter jump" + keyReset)
			emit(keyOverlay + "ctrl+u clear  ctrl+d down  type to filter" + keyReset)
		} else {
			emit(hintFooter("enter", "jump", "j/k", "move", "ctrl+u", "clear", "ctrl+d", "down", "esc", "back"))
		}
	} else {
		showSort := len(names) > 0 && (names[active] == "All" || names[active] == "Favorites")
		custom := len(names) > 0 && names[active] != "All" && names[active] != "Favorites"
		if narrow {
			emit(keyAccentFg + "h/l" + keyReset + keyOverlay + " tabs  j/k move  / search" + keyReset)
			extra := "f pin  n new  esc close"
			if showSort {
				extra = "s sort  " + extra
			}
			if custom {
				extra = "e edit  x delete  " + extra
			}
			emit(keyOverlay + extra + keyReset)
		} else if showSort {
			emit(hintFooter("h/l", "tabs", "s", "sort", "n", "new tab", "f", "pin", "/", "search", "esc", "close"))
		} else if custom {
			emit(hintFooter("h/l", "tabs", "e", "edit", "x", "delete", "n", "new", "/", "search", "esc", "close"))
		} else {
			emit(hintFooter("h/l", "tabs", "n", "new tab", "j/k", "move", "/", "search", "esc", "close"))
		}
	}
	if status != "" {
		emit(keySub + status + keyReset)
	}
	if len(lines) > height {
		lines = lines[:height]
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

func formSummary(sort string, limit int, only, where, state string) string {
	n := "every"
	if limit > 0 {
		n = fmt.Sprintf("the %d", limit)
	}
	which := "panes"
	switch only {
	case "favorites":
		which = "favorites"
	case "unpinned":
		which = "unpinned panes"
	}
	place := ""
	switch where {
	case "here":
		place = " in this workspace"
	case "elsewhere":
		place = " in other workspaces"
	}
	cond := ""
	switch state {
	case "working", "blocked", "idle", "done":
		cond = " that are " + state
	}
	order := "most recent first"
	switch sort {
	case "oldest":
		order = "oldest first"
	case "times":
		order = "most opened first"
	case "rarely":
		order = "least opened first"
	}
	return fmt.Sprintf("Shows %s %s%s%s, %s.", n, which, place, cond, order)
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

func sortLabels() []string {
	return []string{"Last used", "Oldest", "Times opened", "Rarely"}
}
func limitLabels() []string { return []string{"10", "20", "50", "All"} }
func onlyLabels() []string  { return []string{"Any", "Favorites", "Not pinned"} }
func whereLabels() []string { return []string{"Anywhere", "This workspace", "Other workspaces"} }
func stateLabels() []string { return []string{"Any", "Working", "Blocked", "Idle", "Done"} }

func sortOpts() []string { return []string{"last", "oldest", "times", "rarely"} }
func onlyOpts() []string { return []string{"", "favorites", "unpinned"} }
func whereOpts() []string {
	return []string{"", "here", "elsewhere"}
}
func stateOpts() []string {
	return []string{"", "working", "blocked", "idle", "done"}
}

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
func stateChoice(state string) int { return indexOf(stateOpts(), state) }

func cycle(n, i, dir int) int {
	return (i + dir + n) % n
}

func stepFormValue(field, dir int, sort string, limit int, only, where, state string) (string, int, string, string, string) {
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
	case 5:
		opts := stateOpts()
		state = opts[cycle(len(opts), stateChoice(state), dir)]
	}
	return sort, limit, only, where, state
}

// renderForm is the criteria editor. Every choice is on screen. j/k moves
// between fields. h/l changes the highlighted choice. Letters type the name.
func renderForm(width, height int, names []string, active, field int, editing bool, name, sort string, limit int, only, where, state, status string) string {
	if height < 8 {
		height = 8
	}
	title := "New tab"
	if editing {
		title = "Edit tab"
	}
	var lines []string
	emit := func(line string) { lines = append(lines, fitLine(line, width)) }
	emit(tabBar(names, active))
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	emit(keyText + keyBold + "  " + title + keyReset)
	emit(keyOverlay + "  " + formSummary(sort, limit, only, where, state) + keyReset)
	emit(fieldTitle("Name", field == 0))
	if field == 0 {
		shown := name
		if shown == "" {
			shown = "type a name"
			emit(keySelectBg + keyOverlay + "  " + shown + " " + keyReset)
		} else {
			emit(keySelectBg + keySelectFg + keyBold + "  " + shown + " " + keyReset)
		}
	} else if name == "" {
		emit(keyOverlay + "  type a name" + keyReset)
	} else {
		emit(keyText + "  " + name + keyReset)
	}
	emit(fieldTitle("Sort", field == 1))
	for _, line := range choiceLines(sortLabels(), sortChoice(sort), width) {
		emit(line)
	}
	emit(fieldTitle("How many", field == 2))
	for _, line := range choiceLines(limitLabels(), limitChoice(limit), width) {
		emit(line)
	}
	emit(fieldTitle("Pinned", field == 3))
	for _, line := range choiceLines(onlyLabels(), onlyChoice(only), width) {
		emit(line)
	}
	emit(fieldTitle("Workspace", field == 4))
	for _, line := range choiceLines(whereLabels(), whereChoice(where), width) {
		emit(line)
	}
	emit(fieldTitle("Status", field == 5))
	for _, line := range choiceLines(stateLabels(), stateChoice(state), width) {
		emit(line)
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	emit(hintFooter("j/k", "field", "h/l", "choice", "enter", "save", "esc", "cancel"))
	if status != "" {
		emit(keySub + status + keyReset)
	}
	if len(lines) > height {
		lines = lines[:height]
	}
	return paint(lines)
}

// renderConfirm asks before a custom tab is removed. Cancel is selected.
func renderConfirm(width, height int, names []string, active, sel int, name string) string {
	if height < 8 {
		height = 8
	}
	var lines []string
	emit := func(line string) { lines = append(lines, fitLine(line, width)) }
	emit(tabBar(names, active))
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	emit("")
	emit(keyText + keyBold + "  Delete " + name + "?" + keyReset)
	emit(keyOverlay + "  The panes stay. Only this tab is removed." + keyReset)
	emit("")
	for _, line := range choiceLines([]string{"Delete", "Cancel"}, sel, width) {
		emit(line)
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	emit(hintFooter("h/l", "choose", "enter", "confirm", "esc", "back"))
	if len(lines) > height {
		lines = lines[:height]
	}
	return paint(lines)
}

// clipTail keeps the distinguishing end of a name: "portal-internal-scheduler"
// shows its tail, never the "portal-int…" head clip.
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
// or the popup looks open and ignores typing.
func logicalKey(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if b[0] != 0x1b {
		switch b[0] {
		case '\r', '\n':
			return "enter"
		case 0x7f, 0x08:
			return "bs"
		case 0x15:
			return "ctrl-u"
		case 0x04:
			return "ctrl-d"
		default:
			if b[0] >= 0x20 && b[0] < 0x7f {
				return string(b[0])
			}
			return ""
		}
	}
	if len(b) == 1 {
		return "esc"
	}
	s := string(b)
	if strings.Contains(s, "[") && strings.HasSuffix(s, "A") {
		return "up"
	}
	if strings.Contains(s, "[") && strings.HasSuffix(s, "B") {
		return "down"
	}
	if strings.Contains(s, "[") && strings.HasSuffix(s, "C") {
		return "right"
	}
	if strings.Contains(s, "[") && strings.HasSuffix(s, "D") {
		return "left"
	}
	if i := strings.Index(s, "["); i >= 0 && strings.HasSuffix(s, "u") {
		code := 0
		for _, c := range s[i+1:] {
			if c < '0' || c > '9' {
				break
			}
			code = code*10 + int(c-'0')
		}
		switch code {
		case 13:
			return "enter"
		case 27:
			return "esc"
		case 127, 8:
			return "bs"
		case 57417:
			return "left"
		case 57418:
			return "right"
		case 57419:
			return "up"
		case 57420:
			return "down"
		default:
			if code >= 32 && code < 127 {
				return string(byte(code))
			}
		}
	}
	return "esc"
}

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
	}()

	st := loadStore()
	query := ""
	sel := 0
	status := ""
	searching := false
	showHelp := false
	pendingG := false
	// The snapshot is frozen when the picker opens so j/k does not reshuffle
	// rows. Tab and sort changes reorder that frozen set on purpose.
	frozen, err := loadSnapshot()
	if err != nil {
		frozen = &snapshot{}
	}
	tab := openTab(st)
	sortMode := sortDefault
	creating := false
	editing := false
	editIdx := -1
	formField := 0
	confirmName := ""
	confirmSel := 1
	newName, newSort, newOnly, newWhere, newState := "", "last", "", "", ""
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
	for {
		rows := allRows
		if q := strings.TrimSpace(query); q != "" {
			rows = filterRows(allRows, q)
		}
		if sel >= len(rows) {
			sel = len(rows) - 1
		}
		if sel < 0 {
			sel = 0
		}
		w, h := termSize(tty)
		var frame string
		switch {
		case showHelp:
			frame = renderHelp(w, h)
		case confirmName != "":
			frame = renderConfirm(w, h, viewNames(st), tab, confirmSel, confirmName)
		case creating:
			frame = renderForm(w, h, viewNames(st), tab, formField, editing, newName, newSort, newLimit, newOnly, newWhere, newState, status)
		default:
			v := views(st)[tab]
			kind := "both"
			if v.Kind == "custom" {
				kind = st.Tabs[v.Idx].Sort
			}
			frame = renderFrame(rows, sel, query, status, searching, w, h, viewNames(st), tab, kind)
		}
		tty.WriteString(frame)

		buf := make([]byte, 64)
		n, err := tty.Read(buf)
		if err != nil || n == 0 {
			return 0
		}
		b := buf[:n]
		key := logicalKey(b)
		if key == "" {
			continue
		}
		// Rapid g presses arrive in one read ("gg"): replay every leading g as
		// its own press so gg always jumps to the top row.
		if !searching && !showHelp && n >= 2 && b[0] == 'g' {
			k := 0
			for k < n && b[k] == 'g' {
				k++
			}
			for i := 0; i < k; i++ {
				if pendingG {
					sel = 0
					pendingG = false
				} else {
					pendingG = true
				}
			}
			if k >= n {
				continue // the whole read was g presses; redraw at the new row
			}
			b, n = b[k:], len(b)-k // mixed read: handle the rest normally
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
						_ = saveStore(st)
						tab = 0
						status = "deleted " + confirmName
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
			switch key {
			case "esc":
				creating = false
				editing = false
				status = ""
			case "enter":
				name := strings.TrimSpace(newName)
				if name == "" {
					formField = 0
					status = "Type a name, then press enter."
					break
				}
				ct := customTab{Name: name, Sort: newSort, Limit: newLimit, Only: newOnly, Where: newWhere, State: newState}
				if editing && editIdx >= 0 && editIdx < len(st.Tabs) {
					ct.Fixed = st.Tabs[editIdx].Fixed
					st.Tabs[editIdx] = ct
					status = "saved " + name
				} else {
					st.Tabs = append(st.Tabs, ct)
					tab = len(views(st)) - 1
					status = "created " + name
				}
				_ = saveStore(st)
				creating = false
				editing = false
				sel = 0
				rebuild()
			case "j", "down":
				if formField < 5 {
					formField++
				}
				status = ""
			case "k", "up":
				if formField > 0 {
					formField--
				}
				status = ""
			case "h", "left":
				newSort, newLimit, newOnly, newWhere, newState = stepFormValue(formField, -1, newSort, newLimit, newOnly, newWhere, newState)
				status = ""
			case "l", "right":
				newSort, newLimit, newOnly, newWhere, newState = stepFormValue(formField, 1, newSort, newLimit, newOnly, newWhere, newState)
				status = ""
			case "bs":
				if formField == 0 {
					rs := []rune(newName)
					if len(rs) > 0 {
						newName = string(rs[:len(rs)-1])
					}
				}
			default:
				if formField == 0 && len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f {
					newName += key
					status = ""
				}
			}
			continue
		}
		// A lone g waits for its second g; any other key discards it.
		if key != "g" {
			pendingG = false
		}
		half := max(1, (h-3)/2)
		switch {
		case key == "esc":
			if showHelp {
				showHelp = false // esc closes help, the row stays
				continue
			}
			if searching {
				// Esc leaves search mode first; a second Esc closes.
				searching = false
				query = ""
				sel = 0
				continue
			}
			return 0 // esc closes from browse mode
		case showHelp:
			// While help is open only esc, ? and q act; the list does not move.
			if key == "?" || key == "q" {
				showHelp = false
			}
		case key == "up":
			if sel > 0 {
				sel--
			}
		case key == "down":
			if sel < len(rows)-1 {
				sel++
			}
		case key == "enter":
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
			rebuild()
		case key == "s" && !searching && !showHelp:
			v := views(st)[tab]
			if v.Kind != "all" && v.Kind != "favorites" {
				break
			}
			sortMode = (sortMode + 1) % sortCount
			sel = 0
			status = "sort: " + sortHow(sortMode, "last")
			rebuild()
		case key == "n" && !searching && !showHelp:
			creating = true
			editing = false
			editIdx = -1
			formField = 0
			newName, newSort, newOnly, newWhere, newState, newLimit = "", "last", "", "", "", 20
			status = ""
		case key == "e" && !searching && !showHelp:
			v := views(st)[tab]
			if v.Kind != "custom" {
				break
			}
			ct := st.Tabs[v.Idx]
			creating = true
			editing = true
			editIdx = v.Idx
			formField = 1
			newName, newSort, newOnly, newWhere, newState, newLimit = ct.Name, ct.Sort, ct.Only, ct.Where, ct.State, ct.Limit
			status = ""
		case key == "x" && !searching && !showHelp:
			v := views(st)[tab]
			if v.Kind != "custom" {
				break
			}
			confirmName = st.Tabs[v.Idx].Name
			confirmSel = 1
		case key == "/" && !searching:
			// slash enters search mode, the way prefix+k does; it is never a
			// filter character in browse mode.
			searching = true
			query = ""
			sel = 0
		case key == "g" && !searching:
			if pendingG {
				sel = 0 // gg jumps to the first row
				pendingG = false
			} else {
				pendingG = true
			}
		case key == "G" && !searching:
			sel = max(0, len(rows)-1) // G jumps to the last row
		case key == "j" || key == "down":
			if sel < len(rows)-1 {
				sel++
			}
		case key == "k" || key == "up":
			if sel > 0 {
				sel--
			}
		case key == "f" && !searching:
			if len(rows) > 0 {
				id := rows[sel].PaneID
				if st.toggleFavorite(id) {
					status = "favorited " + id
				} else {
					status = "unfavorited " + id
				}
				_ = saveStore(st)
				rebuild()
			}
		case key == "ctrl-u":
			if searching {
				query = ""
				sel = 0
			} else if sel > half {
				sel -= half // browse: half a page up
			} else {
				sel = 0
			}
		case key == "ctrl-d":
			if sel+half < len(rows) {
				sel += half // half a page down, in browse and in search
			}
			if sel >= len(rows) {
				sel = max(0, len(rows)-1)
			}
		case key == "bs":
			if searching && len(query) > 0 {
				query = query[:len(query)-1]
				sel = 0
			}
		case key == "q" && !searching:
			return 0
		case searching && len(key) == 1 && key[0] >= 0x20 && key[0] < 0x7f:
			// Letters reach the search box only after /. In browse mode, h/l move
			// tabs and f/j/k keep their own actions.
			query += key
			sel = 0
		}
	}
}

// ---------------------------------------------------------------------------
// entrypoints
// ---------------------------------------------------------------------------

func paneRectWidth(s *snapshot, paneID string) int {
	for _, l := range s.Layouts {
		for _, lp := range l.Panes {
			if lp.PaneID == paneID {
				return lp.Rect.Width
			}
		}
	}
	return 0
}

func openPicker() int {
	if id, err := recordFocus(); err == nil {
		_ = id
	}
	// Popup is a session modal, like prefix+k: it floats over the current view
	// and does not change the tab layout. Exiting the process dismisses it, so
	// no pane close or tab close is ever issued. The CLI help omits popup, but
	// the 0.9.3 server implements it (width/height are popup-only flags).
	if _, err := herdr("plugin", "pane", "open", "--plugin", pluginID, "--entrypoint", "picker", "--placement", "popup", "--width", "80%", "--height", "70%"); err != nil {
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
	query := os.Getenv("RECALL_QUERY")
	searching := os.Getenv("RECALL_SEARCH") == "1"
	rows := rowsFor(snap, st, view{Kind: "all"}, sortDefault)
	if query != "" {
		rows = filterRows(rows, query)
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
	os.Stdout.WriteString(renderFrame(rows, sel, query, os.Getenv("RECALL_STATUS"), searching, w, h, viewNames(st), 0, "both"))
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
	case "render":
		os.Exit(renderOnce())
	default:
		fmt.Fprintf(os.Stderr, "recall: unknown command %q\n", os.Args[1])
		os.Exit(2)
	}
}
