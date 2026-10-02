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
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

const pluginID = "RooseveltAdvisors.herdr-recall"

// recentLimit caps the RECENT section so MOST USED has room to list
// high-visit panes whose last-seen timestamps aged out of recency.
const recentLimit = 8

const (
	keyReset    = "\x1b[0m"
	keyDim      = "\x1b[2m"
	keyBold     = "\x1b[1m"
	keyAccentFg = "\x1b[38;5;81m"  // accent, used for selected-row bar, key names
	keySub      = "\x1b[38;5;246m"  // subtext, kept for compatibility
	keyOverlay  = "\x1b[38;5;244m"  // overlay0: hints and placeholders only
	keySelectBg = "\x1b[48;5;81m"   // selected row background
	keySelectFg = "\x1b[38;5;235m"  // contrast text on accent
	keyText     = "\x1b[38;5;252m"  // readable body text on the panel
	keySurface  = "\x1b[38;5;238m"  // mid surface colour for separators
	keyStar     = "\x1b[38;5;221m"  // warm yellow favourite star
	keySecFav   = "\x1b[38;5;221m"  // FAVORITES header, warm
	keySecRec   = "\x1b[38;5;81m"   // RECENT header, accent
	keySecUsed  = "\x1b[38;5;44m"   // MOST USED header, teal
	keyStBad    = "\x1b[38;5;203m"  // blocked: red
	keyStBusy   = "\x1b[38;5;221m"  // working: yellow
	keyStGood   = "\x1b[38;5;44m"   // done: teal
	keyStFree   = "\x1b[38;5;114m"  // idle: green
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
	TabID         string `json:"tab_id"`
	WorkspaceID   string `json:"workspace_id"`
	Label         string `json:"label"`
	Focused       bool   `json:"focused"`
	ActivePaneID  string `json:"-"`
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
	TabID          string      `json:"tab_id"`
	WorkspaceID    string      `json:"workspace_id"`
	FocusedPaneID  string      `json:"focused_pane_id"`
	Panes          []layoutPane `json:"panes"`
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

type store struct {
	Version   int                `json:"version"`
	Panes     map[string]*paneStat `json:"panes"`
	Favorites []string           `json:"favorites"`
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
		return &store{Version: 1, Panes: map[string]*paneStat{}}
	}
	return s
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

func dirFor(cur, dst rect) string {
	dx := dst.X - cur.X
	dy := dst.Y - cur.Y
	if abs(dx) >= abs(dy) {
		if dx >= 0 {
			return "right"
		}
		return "left"
	}
	if dy >= 0 {
		return "down"
	}
	return "up"
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func jumpTo(paneID string) error {
	snap, err := loadSnapshot()
	if err != nil {
		return err
	}
	var pane *paneRec
	for i := range snap.Panes {
		if snap.Panes[i].PaneID == paneID {
			pane = &snap.Panes[i]
			break
		}
	}
	if pane == nil {
		return fmt.Errorf("pane %s not in snapshot", paneID)
	}
	if _, err := herdr("workspace", "focus", pane.WorkspaceID); err != nil {
		return err
	}
	if _, err := herdr("tab", "focus", pane.TabID); err != nil {
		return err
	}
	for hop := 0; hop < 8; hop++ {
		s, err := loadSnapshot()
		if err != nil {
			return err
		}
		if s.FocusedPaneID == paneID {
			return nil
		}
		var cur, dst rect
		found := false
		for _, l := range s.Layouts {
			if l.TabID != pane.TabID {
				continue
			}
			for _, lp := range l.Panes {
				if lp.PaneID == s.FocusedPaneID {
					cur = lp.Rect
					found = true
				}
				if lp.PaneID == paneID {
					dst = lp.Rect
				}
			}
		}
		if !found {
			break
		}
		if dst == (rect{}) {
			break
		}
		d := dirFor(cur, dst)
		if _, err := herdr("pane", "focus", "--direction", d, "--pane", s.FocusedPaneID); err != nil {
			return err
		}
	}
	s, _ := loadSnapshot()
	if s != nil && s.FocusedPaneID != paneID {
		return fmt.Errorf("focused %s after hops, wanted %s", s.FocusedPaneID, paneID)
	}
	return nil
}

// ---------------------------------------------------------------------------
// picker frame: same shape as the built-in goto overlay
// ---------------------------------------------------------------------------

type row struct {
	PaneID  string
	Label   string
	Section string
	Visits  int64
	Last    int64
	Status  string
	Fav     bool
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

func buildRows(s *snapshot, st *store, query string) []row {
	q := strings.ToLower(strings.TrimSpace(query))
	match := func(id, label string) bool {
		if q == "" {
			return true
		}
		return strings.Contains(strings.ToLower(id), q) || strings.Contains(strings.ToLower(label), q)
	}
	statusOf := map[string]string{}
	for _, p := range s.Panes {
		statusOf[p.PaneID] = p.AgentStatus
	}
	seen := map[string]bool{}
	var out []row
	add := func(id, section string) {
		if seen[id] {
			return
		}
		ps := st.Panes[id]
		label := ""
		if ps != nil {
			label = ps.Label
		}
		label = displayLabel(s, id, label)
		if !match(id, label) {
			return
		}
		seen[id] = true
		var visits, last int64
		if ps != nil {
			visits, last = ps.Visits, ps.Last
		}
		out = append(out, row{
			PaneID: id, Label: label, Section: section,
			Visits: visits, Last: last, Status: statusOf[id],
			Fav: st.isFavorite(id),
		})
	}
	// favorites first, then recency, then most used (each pane once).
	// RECENT is capped so panes with high visit counts but older last-seen
	// timestamps surface in MOST USED instead of hiding behind it.
	for _, f := range st.Favorites {
		add(f, "FAVORITES")
	}
	var byLast, byVisits []string
	for id := range st.Panes {
		byLast = append(byLast, id)
		byVisits = append(byVisits, id)
	}
	sort.Slice(byLast, func(i, j int) bool { return st.Panes[byLast[i]].Last > st.Panes[byLast[j]].Last })
	sort.Slice(byVisits, func(i, j int) bool { return st.Panes[byVisits[i]].Visits > st.Panes[byVisits[j]].Visits })
	for i, id := range byLast {
		if i >= recentLimit {
			break
		}
		add(id, "RECENT")
	}
	for _, id := range byVisits {
		add(id, "MOST USED")
	}
	return out
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
func renderFrame(rows []row, sel int, query string, status string, searching bool, width, height int) string {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H") // clear
	// Every line is clipped to the pane's real width: one pane, one row, no
	// wrapping, whatever width the overlay happens to be.
	emit := func(line string) { b.WriteString(fitLine(line, width) + "\r\n") }
	if searching {
		prompt := keyOverlay + "search panes" + keyReset
		if query != "" {
			prompt = keyText + query + keyReset
		}
		emit(keyAccentFg + keyBold + " / " + keyReset + prompt)
	} else {
		emit(keyAccentFg + keyBold + "> " + keyReset + keyOverlay + "/ search panes" + keyReset)
	}
	emit(keySurface + strings.Repeat("─", max(8, width-1)) + keyReset)
	footer := 2
	visible := height - footer - 1
	if visible < 3 {
		visible = 3
	}
	start := 0
	if sel >= visible {
		start = sel - visible + 1
	}
	lastSection := ""
	// Column budget: a narrow overlay keeps the status dot and a short visit
	// count on the same row as a clipped label; a wide one gets the full form.
	narrow := width < 80
	starW, statusW, metaW := 2, 12, 8
	if narrow {
		// The name is the point of the picker: a narrow pane keeps only the
		// coloured dot and a short visit count and gives every remaining
		// column to the label.
		statusW, metaW = 2, 6
	}
	labelW := width - starW - statusW - metaW
	if labelW < 4 {
		labelW = 4
	}
	for i, r := range rows {
		if i < start {
			continue
		}
		if i-start >= visible {
			break
		}
		if r.Section != lastSection {
			emit(sectionColor(r.Section) + keyBold + "  " + r.Section + keyReset)
			lastSection = r.Section
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
		// The name gets the columns: visits is a short "3v" either way, and the
		// status is a dot plus its word, so "pi - wiseman . THE-FM" fits.
		metaCol := fmt.Sprintf("%dv", r.Visits)
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
		if narrow {
			emit(keyAccentFg + "/" + keyReset + keyOverlay + " search  enter jump  f fav" + keyReset)
			emit(keyAccentFg + "?" + keyReset + keyOverlay + " help  gg/G first/last  j/k move  esc close" + keyReset)
		} else {
			emit(hintFooter("/", "search", "enter", "jump", "f", "favorite", "?", "help", "j/k", "move", "gg/G", "ends", "esc", "close"))
		}
	}
	if status != "" {
		emit(keySub + status + keyReset)
	}
	return b.String()
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
	w, h := 0, 0
	cmd := exec.Command("stty", "size")
	cmd.Stdin = tty
	if out, err := cmd.Output(); err == nil {
		f := strings.Fields(string(out))
		if len(f) == 2 {
			if v, err := strconv.Atoi(f[0]); err == nil && v > 0 {
				h = v
			}
			if v, err := strconv.Atoi(f[1]); err == nil && v > 0 {
				w = v
			}
		}
	}
	if w <= 0 {
		w = 80 // herdr's own panel width, never 100
	}
	if h <= 0 {
		h = 24
	}
	return w, h
}

func runPicker() int {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: no tty: %v\n", err)
		return 1
	}
	defer tty.Close()
	// Exiting the process dismisses the popup (Esc, q and a jump all return to
	// the same view); no pane close or tab close is ever issued, so no tab can
	// be left behind.
	// raw mode without external dependencies
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
	// The row order is frozen when the picker opens: one snapshot, one sort.
	// Keys never rebuild or re-sort the list; search only narrows it.
	frozen, err := loadSnapshot()
	if err != nil {
		frozen = &snapshot{}
	}
	allRows := buildRows(frozen, st, "")
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
		// Draw to the larger of the tty and the layout rect: a 45 column pane
		// must not paint itself 23 wide because the tty read lagged.
		if rw := paneRectWidth(frozen, frozen.FocusedPaneID); rw > w {
			w = rw
		}
		var frame string
		if showHelp {
			frame = renderHelp(w, h)
		} else {
			frame = renderFrame(rows, sel, query, status, searching, w, h)
		}
		tty.WriteString(frame)

		buf := make([]byte, 8)
		n, err := tty.Read(buf)
		if err != nil || n == 0 {
			return 0
		}
		b := buf[:n]
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
		// A lone g waits for its second g; any other key discards it.
		if b[0] != 'g' || n != 1 {
			pendingG = false
		}
		half := max(1, (h-3)/2)
		switch {
		case b[0] == 0x1b && n == 1:
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
			switch b[0] {
			case '?', 'q':
				showHelp = false
			}
		case b[0] == 0x1b && n > 1 && b[1] == '[':
			switch {
			case n > 2 && (b[2] == 'A'):
				if sel > 0 {
					sel--
				}
			case n > 2 && (b[2] == 'B'):
				if sel < len(rows)-1 {
					sel++
				}
			}
		case b[0] == '\r' || b[0] == '\n':
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
		case b[0] == '?' && !searching:
			showHelp = true
		case b[0] == '/' && !searching:
			// slash enters search mode, the way prefix+k does; it is never a
			// filter character in browse mode.
			searching = true
			query = ""
			sel = 0
		case b[0] == 'g' && !searching && n == 1:
			if pendingG {
				sel = 0 // gg jumps to the first row
				pendingG = false
			} else {
				pendingG = true
			}
		case b[0] == 'G' && !searching:
			sel = max(0, len(rows)-1) // G jumps to the last row
		case b[0] == 'j' && !searching:
			if sel < len(rows)-1 {
				sel++
			}
		case b[0] == 'k' && !searching:
			if sel > 0 {
				sel--
			}
		case b[0] == 'f' && !searching:
			if len(rows) > 0 {
				id := rows[sel].PaneID
				if st.toggleFavorite(id) {
					status = "favorited " + id
				} else {
					status = "unfavorited " + id
				}
				_ = saveStore(st)
				for i := range allRows {
					if allRows[i].PaneID == id {
						allRows[i].Fav = st.isFavorite(id)
					}
				}
			}
		case b[0] == 0x15: // ctrl+u
			if searching {
				query = ""
				sel = 0
			} else if sel > half {
				sel -= half // browse: half a page up
			} else {
				sel = 0
			}
		case b[0] == 0x04: // ctrl+d
			if sel+half < len(rows) {
				sel += half // half a page down, in browse and in search
			}
			if sel >= len(rows) {
				sel = max(0, len(rows)-1)
			}
		case b[0] == 0x7f || b[0] == 0x08:
			if searching && len(query) > 0 {
				query = query[:len(query)-1]
				sel = 0
			}
		case b[0] == 'q' && !searching:
			return 0
		case searching && b[0] >= 0x20 && b[0] < 0x7f:
			// only search mode filters; browse mode ignores typed letters
			query += string(b[0])
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
	rows := buildRows(snap, st, query)
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
	os.Stdout.WriteString(renderFrame(rows, sel, query, os.Getenv("RECALL_STATUS"), searching, w, h))
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
