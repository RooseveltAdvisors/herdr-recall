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
	keyReset   = "\x1b[0m"
	keyDim     = "\x1b[2m"
	keyBold    = "\x1b[1m"
	keyAccentFg = "\x1b[38;5;81m"  // matches the overlay accent line style
	keySub     = "\x1b[38;5;246m"  // subtext
	keyOverlay = "\x1b[38;5;244m"  // overlay0, used for placeholder and hints
	keySelectBg = "\x1b[48;5;81m"  // selected row background
	keySelectFg = "\x1b[38;5;235m" // text on accent
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

// renderFrame draws one screenful. Exposed via --render so screenshots and
// tests use the exact bytes a live session would show.
func renderFrame(rows []row, sel int, query string, status string, width, height int) string {
	var b strings.Builder
	b.WriteString("\x1b[2J\x1b[H") // clear
	if query == "" {
		b.WriteString(keyAccentFg + keyBold + "> " + keyReset + keyOverlay + "search panes" + keyReset)
	} else {
		b.WriteString(keyAccentFg + keyBold + "> " + keyReset + query + keyReset)
	}
	b.WriteString("\r\n")
	b.WriteString(keySub + strings.Repeat("─", max(8, width-1)) + keyReset + "\r\n")
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
	for i, r := range rows {
		if i < start {
			continue
		}
		if i-start >= visible {
			break
		}
		if r.Section != lastSection {
			b.WriteString(keyOverlay + keyBold + "  " + r.Section + keyReset + "\r\n")
			lastSection = r.Section
		}
		fav := " "
		if r.Fav {
			fav = "*"
		}
		chipText := ""
		if r.Status != "" && r.Status != "unknown" {
			chipText = "[" + r.Status + "]"
		}
		// One fixed layout for every row, the way prefix+k paints it: a two
		// column favourite marker, a label cell, a chip cell at a stable
		// column, then the counts. The selected row is painted as ONE span so
		// the accent bar covers label, status and metadata together.
		favCol := fav + " "
		labelCell := pad(clip(r.Label, 44), 44)
		chipCell := pad(chipText, 12)
		metaCol := fmt.Sprintf("%d visits %s", r.Visits, ago(r.Last))
		plain := favCol + labelCell + chipCell + " " + metaCol
		if i == sel {
			b.WriteString(keySelectBg + keySelectFg + keyBold + pad(plain, width-1) + keyReset + "\r\n")
		} else {
			b.WriteString(" " + favCol + labelCell + keySub + chipCell + metaCol + keyReset + "\r\n")
		}
	}
	b.WriteString(keySub + strings.Repeat("─", max(8, width-1)) + keyReset + "\r\n")
	b.WriteString(keyOverlay + " enter jump   f favorite   j/k move   type filter   esc close" + keyReset + "\r\n")
	if status != "" {
		b.WriteString(keySub + status + keyReset + "\r\n")
	}
	return b.String()
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

func termSize() (int, int) {
	w, h := 100, 30
	if out, err := exec.Command("stty", "size").Output(); err == nil {
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
	return w, h
}

func runPicker() int {
	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "recall: no tty: %v\n", err)
		return 1
	}
	defer tty.Close()
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
	for {
		snap, err := loadSnapshot()
		if err != nil {
			status = "snapshot: " + err.Error()
			snap = &snapshot{}
		}
		rows := buildRows(snap, st, query)
		if sel >= len(rows) {
			sel = len(rows) - 1
		}
		if sel < 0 {
			sel = 0
		}
		w, h := termSize()
		frame := renderFrame(rows, sel, query, status, w, h)
		tty.WriteString(frame)

		buf := make([]byte, 8)
		n, err := tty.Read(buf)
		if err != nil || n == 0 {
			return 0
		}
		b := buf[:n]
		switch {
		case b[0] == 0x1b && n == 1:
			return 0 // esc closes
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
		case b[0] == 'j':
			if sel < len(rows)-1 {
				sel++
			}
		case b[0] == 'k':
			if sel > 0 {
				sel--
			}
		case b[0] == 'f':
			if len(rows) > 0 {
				id := rows[sel].PaneID
				if st.toggleFavorite(id) {
					status = "favorited " + id
				} else {
					status = "unfavorited " + id
				}
				_ = saveStore(st)
			}
		case b[0] == 0x7f || b[0] == 0x08:
			if len(query) > 0 {
				query = query[:len(query)-1]
				sel = 0
			}
		case b[0] == 'q' && query == "":
			return 0
		case b[0] >= 0x20 && b[0] < 0x7f:
			query += string(b[0])
			sel = 0
		}
	}
}

// ---------------------------------------------------------------------------
// entrypoints
// ---------------------------------------------------------------------------

func openPicker() int {
	if id, err := recordFocus(); err == nil {
		_ = id
	}
	if _, err := herdr("plugin", "pane", "open", "--plugin", pluginID, "--entrypoint", "picker", "--placement", "overlay", "--focus"); err != nil {
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
	rows := buildRows(snap, st, query)
	sel := 0
	if v := os.Getenv("RECALL_SEL"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			sel = n
		}
	}
	w, h := 100, 30
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
	os.Stdout.WriteString(renderFrame(rows, sel, query, os.Getenv("RECALL_STATUS"), w, h))
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
