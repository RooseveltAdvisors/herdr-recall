package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/RooseveltAdvisors/vimline"
)

// txHit is one ccrider search match the Transcripts tab can open or fork.
type txHit struct {
	SessionID string
	MsgID     int64
	Provider  string
	CWD       string
	Summary   string
	Snippet   string
	When      string
	Rank      float64
}

// transcriptsEnabled reports whether the Transcripts tab should appear.
// The tab stays hidden unless the optional ccrider dependency is installed.
// HERDR_RECALL_TRANSCRIPTS=0 hides it. HERDR_RECALL_TRANSCRIPTS=1 shows it.
func transcriptsEnabled() bool {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("HERDR_RECALL_TRANSCRIPTS"))) {
	case "0", "off", "no", "false":
		return false
	case "1", "on", "yes", "true":
		return true
	}
	_, err := exec.LookPath("ccrider")
	return err == nil
}

func ccriderDB() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "ccrider", "sessions.db")
}

// parseCcriderSearch reads the text from `ccrider search`.
func parseCcriderSearch(text string) []txHit {
	var out []txHit
	var cur *txHit
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], "\r")
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "=== Session ") {
			if cur != nil && cur.SessionID != "" {
				out = append(out, *cur)
			}
			cur = &txHit{}
			continue
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(trim, "ID: ") {
			cur.SessionID = strings.TrimSpace(strings.TrimPrefix(trim, "ID: "))
			continue
		}
		if strings.Contains(trim, " | ") && strings.Contains(trim, " msgs | ") {
			parts := strings.Split(trim, " | ")
			if len(parts) >= 3 {
				cur.CWD = strings.TrimSpace(parts[0])
				cur.When = strings.TrimSpace(parts[2])
			}
			continue
		}
		if strings.HasPrefix(trim, "Match ") {
			var snip []string
			for i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if next == "" || strings.HasPrefix(next, "Match ") || strings.HasPrefix(next, "=== Session ") || strings.HasPrefix(next, "Showing first ") {
					break
				}
				i++
				snip = append(snip, strings.TrimSpace(lines[i]))
			}
			if cur.Snippet == "" {
				cur.Snippet = strings.Join(snip, " ")
			}
			continue
		}
		if cur.Summary == "" && trim != "" && !strings.HasPrefix(trim, "Found ") && !strings.HasPrefix(trim, "Showing ") && !strings.HasPrefix(trim, "[No summary]") {
			cur.Summary = trim
		}
	}
	if cur != nil && cur.SessionID != "" {
		out = append(out, *cur)
	}
	return out
}

func transcriptSender(role string) string {
	switch role {
	case "replies":
		return "assistant"
	case "everything":
		return ""
	default:
		return "human"
	}
}

func nextTxRole(role string) string {
	switch role {
	case "typed":
		return "any-user"
	case "any-user":
		return "replies"
	case "replies":
		return "everything"
	default:
		return "typed"
	}
}

func txRoleLabel(role string) string {
	switch role {
	case "any-user":
		return "Any user"
	case "replies":
		return "Replies"
	case "everything":
		return "Everything"
	default:
		return "Typed"
	}
}

func nextTxSort(sort string) string {
	switch sort {
	case "newest":
		return "oldest"
	case "oldest":
		return "relevance"
	default:
		return "newest"
	}
}

func txSortLabel(sort string) string {
	switch sort {
	case "newest":
		return "Newest"
	case "oldest":
		return "Oldest"
	default:
		return "Relevance"
	}
}

func parseTxTime(s string) (time.Time, bool) {
	for _, layout := range []string{
		"2006-01-02 15:04:05.000 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
		time.RFC3339,
	} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// relevanceScore mixes lexical match quality with a 14-day recency weight.
// FTS5 bm25 is negative. A lower value is a better word match.
func relevanceScore(bm25 float64, when string, now time.Time) float64 {
	lexical := -bm25
	if lexical < 0 {
		lexical = 0
	}
	ageDays := 30.0
	if t, ok := parseTxTime(when); ok {
		ageDays = now.Sub(t).Hours() / 24
		if ageDays < 0 {
			ageDays = 0
		}
	}
	recency := 1.0 / (1.0 + ageDays/14.0)
	return lexical * (0.35 + 0.65*recency)
}

func sortTxHits(hits []txHit, mode string, now time.Time) []txHit {
	out := append([]txHit(nil), hits...)
	switch mode {
	case "newest":
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].When > out[j].When
		})
	case "oldest":
		sort.SliceStable(out, func(i, j int) bool {
			return out[i].When < out[j].When
		})
	default:
		sort.SliceStable(out, func(i, j int) bool {
			return relevanceScore(out[i].Rank, out[i].When, now) > relevanceScore(out[j].Rank, out[j].When, now)
		})
	}
	return out
}

func txRoleFromKey(key string) (string, bool) {
	switch key {
	case "1":
		return "typed", true
	case "2":
		return "any-user", true
	case "3":
		return "replies", true
	case "4":
		return "everything", true
	default:
		return "", false
	}
}

func typedOnly(text string) (string, bool) {
	s := strings.TrimLeft(text, "\u2063\t\r\n ")
	if i := strings.LastIndex(s, "CURRENT USER MESSAGE:"); i >= 0 {
		s = strings.TrimSpace(s[i+len("CURRENT USER MESSAGE:"):])
		s = strings.TrimLeft(s, ":\n ")
	}
	if s == "" || injectedUserText(s) {
		return "", false
	}
	return s, true
}

func injectedUserText(s string) bool {
	s = strings.TrimLeft(s, "\u2063\t\r\n ")
	prefixes := []string{
		"FIRSTMATE_OP:",
		"Beads enforcement",
		"Workspace boundary",
		"PREVIOUS AI RESPONSE",
		"BEADS NOTIFY",
		"Firstmate instruction",
		": Firstmate instruction",
		"Analyze this Claude Code",
		"reply with exactly:",
		"Reply to me at:",
		"<recommended_plugins>",
		"<task-notification>",
		"<!-- backpass",
		"30-minute signage",
		"Supervisor escalate",
		"You are a crewmate",
		"You are an autonomous",
		"# Current worker role",
	}
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func ftsQuery(q string) string {
	var parts []string
	for _, w := range strings.Fields(q) {
		w = strings.ReplaceAll(w, "\"", "")
		if w == "" {
			continue
		}
		parts = append(parts, "\""+w+"\"")
	}
	return strings.Join(parts, " AND ")
}

func filterHits(hits []txHit, query string) []txHit {
	words := strings.Fields(strings.ToLower(query))
	if len(words) == 0 {
		return hits
	}
	out := make([]txHit, 0, len(hits))
	for _, h := range hits {
		blob := strings.ToLower(h.Snippet + " " + h.Summary)
		ok := true
		for _, w := range words {
			if !strings.Contains(blob, w) {
				ok = false
				break
			}
		}
		if ok {
			out = append(out, h)
		}
	}
	return out
}

func searchTranscripts(query, role, order string) ([]txHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	var resp txReply
	if err := txAsk(map[string]any{"op": "search", "match": match, "sender": transcriptSender(role), "sort": order}, &resp); err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("%s", resp.Err)
	}
	hits := resp.Hits
	if role == "typed" || role == "" || role == "user" {
		kept := make([]txHit, 0, len(hits))
		for _, h := range hits {
			body, ok := typedOnly(h.Snippet)
			if !ok {
				continue
			}
			h.Snippet = matchWindow(body, query, 180)
			kept = append(kept, h)
			if len(kept) == 40 {
				break
			}
		}
		hits = kept
	} else if len(hits) > 40 {
		hits = hits[:40]
	}
	for i := range hits {
		hits[i].Snippet = matchWindow(hits[i].Snippet, query, 180)
	}
	hits = sortTxHits(hits, order, time.Now())
	if len(hits) > 40 {
		hits = hits[:40]
	}
	return hits, nil
}

func previewTranscript(sessionID string, msgID int64) ([]string, error) {
	var resp txReply
	if err := txAsk(map[string]any{"op": "preview", "session_id": sessionID, "msg_id": msgID}, &resp); err != nil {
		return nil, err
	}
	if resp.Err != "" {
		return nil, fmt.Errorf("%s", resp.Err)
	}
	return resp.Lines, nil
}

type txJob struct {
	Gen   int
	Query string
	Role  string
	Sort  string
}

type txAsync struct {
	Kind      string
	Gen       int
	Hits      []txHit
	Lines     []string
	PreviewID string
	Err       error
}

func serveTranscriptJobs(jobs <-chan txJob, results chan<- txAsync) {
	var pending *txJob
	for {
		if pending == nil {
			job, ok := <-jobs
			if !ok {
				return
			}
			pending = &job
		}
		select {
		case job := <-jobs:
			pending = &job
			continue
		default:
		}
		job := *pending
		pending = nil
		hits, err := searchTranscripts(job.Query, job.Role, job.Sort)
		results <- txAsync{Kind: "search", Gen: job.Gen, Hits: hits, Err: err}
	}
}

type txReply struct {
	Hits  []txHit  `json:"hits"`
	Lines []string `json:"lines"`
	Err   string   `json:"err"`
}

type txProc struct {
	mu  sync.Mutex
	cmd *exec.Cmd
	in  io.WriteCloser
	out *bufio.Reader
}

var txSearch txProc

func txAsk(req map[string]any, resp *txReply) error {
	txSearch.mu.Lock()
	defer txSearch.mu.Unlock()
	if err := txSearch.ensure(); err != nil {
		return err
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := txSearch.in.Write(append(b, '\n')); err != nil {
		txSearch.stop()
		return err
	}
	line, err := txSearch.out.ReadString('\n')
	if err != nil {
		txSearch.stop()
		return err
	}
	return json.Unmarshal([]byte(line), resp)
}

func (p *txProc) stop() {
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
	}
	p.cmd, p.in, p.out = nil, nil, nil
}

func (p *txProc) ensure() error {
	if p.cmd != nil {
		return nil
	}
	db := ccriderDB()
	if db == "" {
		return fmt.Errorf("ccrider index is missing")
	}
	if _, err := os.Stat(db); err != nil {
		return fmt.Errorf("ccrider index is not ready")
	}
	cmd := exec.Command("python3", "-u", "-c", txWorkerScript, db)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return err
	}
	p.cmd = cmd
	p.in = stdin
	p.out = bufio.NewReader(stdout)
	return nil
}

const txWorkerScript = `
import json, sqlite3, sys
db = sys.argv[1]
con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
con.execute("PRAGMA query_only=ON")
con.execute("PRAGMA temp_store=MEMORY")
def emit(obj):
    sys.stdout.write(json.dumps(obj) + "\n")
    sys.stdout.flush()
for raw in sys.stdin:
    raw = raw.strip()
    if not raw:
        continue
    try:
        req = json.loads(raw)
        op = req.get("op")
        if op == "search":
            # Filter the speaker before the rank limit. A global top-200 is
            # almost all tool text, so a user word never appears.
            sender = req.get("sender") or ""
            sql = """
            SELECT s.session_id, m.id, ifnull(s.provider,''), ifnull(s.cwd,''), ifnull(s.summary,''),
                   substr(ifnull(m.text_content,''), 1, 1600), ifnull(m.timestamp,''), bm25(messages_fts)
            FROM messages_fts
            JOIN messages m ON m.id = messages_fts.rowid
            JOIN sessions s ON s.id = m.session_id
            WHERE messages_fts MATCH ?
            """
            args = [req.get("match") or ""]
            if sender:
                sql += " AND m.sender = ?"
                args.append(sender)
            order = req.get("sort") or "relevance"
            if order == "newest":
                sql += " AND m.timestamp > datetime('now', '-365 days') ORDER BY m.timestamp DESC LIMIT 120"
            elif order == "oldest":
                sql += " AND m.timestamp > datetime('now', '-365 days') ORDER BY m.timestamp ASC LIMIT 120"
            else:
                sql += " ORDER BY rank LIMIT 200"
            hits = []
            for row in con.execute(sql, args):
                    hits.append({
                        "SessionID": row[0], "MsgID": row[1], "Provider": row[2], "CWD": row[3],
                        "Summary": row[4] or "", "Snippet": " ".join((row[5] or "").split()), "When": row[6] or "",
                        "Rank": row[7] if len(row) > 7 else 0,
                    })
            emit({"hits": hits})
        elif op == "preview":
            rows = con.execute("""
                SELECT ifnull(m.sender,''), ifnull(m.text_content,''), m.id
                FROM messages m
                JOIN sessions s ON s.id = m.session_id
                WHERE s.session_id = ? AND m.sender IN ('human','assistant')
                  AND m.id BETWEEN ? - 6 AND ? + 6
                ORDER BY m.sequence
            """, (req.get("session_id"), int(req.get("msg_id") or 0), int(req.get("msg_id") or 0))).fetchall()
            lines = []
            for sender, text, mid in rows:
                who = "you" if sender == "human" else "agent"
                body = " ".join((text or "").split())
                if len(body) > 280:
                    body = body[:279] + "…"
                mark = ">" if mid == int(req.get("msg_id") or 0) else " "
                lines.append(mark + " " + who + ": " + body)
            emit({"lines": lines})
        else:
            emit({"err": "unknown op"})
    except Exception as exc:
        emit({"err": str(exc)})
`

func fillProviders(hits []txHit) {
	db := ccriderDB()
	if db == "" || len(hits) == 0 {
		return
	}
	if _, err := os.Stat(db); err != nil {
		return
	}
	var ids []string
	for _, h := range hits {
		ids = append(ids, h.SessionID)
	}
	script := `
import json, sqlite3, sys
db, ids = sys.argv[1], json.loads(sys.argv[2])
con = sqlite3.connect(db)
cur = con.cursor()
out = {}
for i in ids:
    row = cur.execute("select provider from sessions where session_id = ?", (i,)).fetchone()
    if row:
        out[i] = row[0] or ""
print(json.dumps(out))
`
	cmd := exec.Command("python3", "-c", script, db, mustJSON(ids))
	out, err := cmd.Output()
	if err != nil {
		return
	}
	var got map[string]string
	if json.Unmarshal(out, &got) != nil {
		return
	}
	for i := range hits {
		hits[i].Provider = got[hits[i].SessionID]
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func transcriptRows(hits []txHit) []row {
	out := make([]row, 0, len(hits))
	for _, h := range hits {
		label := txTitle(h, txLive{})
		if label == "" {
			label = h.Snippet
		}
		out = append(out, row{
			PaneID: h.SessionID,
			Label:  label,
			Note:   h.Snippet,
			Agent:  h.Provider,
			CWD:    h.CWD,
		})
	}
	return out
}

type txLive struct {
	Tab   string
	Pane  string
	Agent string
}

func txLiveMeta(s *snapshot, sessionID string) txLive {
	if s == nil || sessionID == "" {
		return txLive{}
	}
	for _, p := range s.Panes {
		if !sessionMatchesPane(sessionID, p.AgentSession.Value) {
			continue
		}
		tab := ""
		for _, t := range s.Tabs {
			if t.TabID == p.TabID {
				tab = strings.TrimSpace(t.Label)
				break
			}
		}
		pane := strings.TrimSpace(p.TerminalTitleStrip)
		if pane == "" || strings.Contains(pane, "/") || len([]rune(pane)) > 28 {
			pane = p.PaneID
		}
		return txLive{Tab: tab, Pane: pane, Agent: p.Agent}
	}
	return txLive{}
}

func agentLabel(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "pi":
		return "Pi"
	case "claude":
		return "Claude"
	case "codex":
		return "Codex"
	case "antigravity", "agy":
		return "Antigravity"
	default:
		return strings.TrimSpace(name)
	}
}

func projectName(cwd string) string {
	base := filepath.Base(strings.TrimRight(cwd, "/"))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	return base
}

func txTitle(hit txHit, live txLive) string {
	var parts []string
	if live.Tab != "" {
		parts = append(parts, live.Tab)
	} else if name := projectName(hit.CWD); name != "" {
		parts = append(parts, name)
	}
	if live.Pane != "" && live.Pane != live.Tab {
		parts = append(parts, live.Pane)
	}
	who := agentLabel(live.Agent)
	if who == "" {
		who = agentLabel(hit.Provider)
	}
	if who != "" {
		parts = append(parts, who)
	}
	if len(parts) == 0 {
		return "session"
	}
	return strings.Join(parts, " · ")
}

func txWhenShort(when string, now time.Time) string {
	t, ok := parseTxTime(when)
	if !ok {
		return ""
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	case d < 14*24*time.Hour:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	default:
		return t.Format("Jan 2")
	}
}

func matchWindow(text, query string, width int) string {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" || width < 8 {
		return text
	}
	low := strings.ToLower(text)
	pos := -1
	for _, w := range strings.Fields(query) {
		w = strings.ToLower(strings.Trim(w, `"`))
		if w == "" {
			continue
		}
		if i := strings.Index(low, w); i >= 0 && (pos < 0 || i < pos) {
			pos = i
		}
	}
	if pos < 0 {
		return clip(text, width)
	}
	r := []rune(text)
	posR := len([]rune(text[:pos]))
	start := posR - width/3
	if start < 0 {
		start = 0
	}
	end := start + width
	if end > len(r) {
		end = len(r)
		start = max(0, end-width)
	}
	out := string(r[start:end])
	if start > 0 {
		out = "…" + out
	}
	return out
}

func highlightWords(text, query string, selected bool) string {
	markOn := keyBold + keyAccentFg
	markOff := keyText
	if selected {
		markOn = "\x1b[4m"
		markOff = "\x1b[24m"
	}
	var words []string
	for _, w := range strings.Fields(query) {
		w = strings.ToLower(strings.Trim(w, `"`))
		if len(w) >= 2 {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		return text
	}
	var b strings.Builder
	i := 0
	for i < len(text) {
		best, blen := -1, 0
		rest := strings.ToLower(text[i:])
		for _, w := range words {
			if strings.HasPrefix(rest, w) && len(w) > blen {
				best, blen = i, len(w)
			}
		}
		if best < 0 {
			_, size := utf8.DecodeRuneInString(text[i:])
			if size < 1 {
				size = 1
			}
			b.WriteString(text[i : i+size])
			i += size
			continue
		}
		b.WriteString(markOn)
		b.WriteString(text[best : best+blen])
		b.WriteString(markOff)
		i = best + blen
	}
	return b.String()
}

func txHitLines(hit txHit, query string, live txLive, width int, now time.Time) (string, string) {
	title := txTitle(hit, live)
	age := txWhenShort(hit.When, now)
	if age != "" {
		title += " · " + age
	}
	if live.Tab != "" {
		title += " · live"
	}
	match := highlightWords(hit.Snippet, query, false)
	if match == "" {
		match = "no snippet"
	}
	return fitLine(title, width), fitLine(match, width)
}

func sessionMatchesPane(sessionID, sessionPath string) bool {
	if sessionID == "" || sessionPath == "" {
		return false
	}
	return strings.Contains(sessionPath, sessionID)
}

func forkPiSession(hit txHit) (string, error) {
	if hit.Provider != "" && hit.Provider != "pi" {
		return "", fmt.Errorf("fork from this tab is ready for Pi. This session is %s", hit.Provider)
	}
	ws := os.Getenv("HERDR_WORKSPACE_ID")
	args := []string{"tab", "create", "--label", "fork", "--no-focus"}
	if ws != "" {
		args = append(args, "--workspace", ws)
	}
	if hit.CWD != "" {
		args = append(args, "--cwd", hit.CWD)
	}
	out, err := herdr(args...)
	if err != nil {
		return "", err
	}
	pane := paneFromHerdr(out)
	if pane == "" {
		return "", fmt.Errorf("fork tab opened, but no pane id came back")
	}
	if _, err := herdr("pane", "run", pane, "pi", "--fork", hit.SessionID); err != nil {
		return pane, err
	}
	return pane, nil
}

func paneFromHerdr(out []byte) string {
	var wrap struct {
		Result struct {
			PaneID string `json:"pane_id"`
			Tab    struct {
				ActivePaneID string `json:"active_pane_id"`
			} `json:"tab"`
			Pane struct {
				PaneID string `json:"pane_id"`
			} `json:"pane"`
		} `json:"result"`
	}
	if json.Unmarshal(out, &wrap) != nil {
		return ""
	}
	if wrap.Result.PaneID != "" {
		return wrap.Result.PaneID
	}
	if wrap.Result.Pane.PaneID != "" {
		return wrap.Result.Pane.PaneID
	}
	return wrap.Result.Tab.ActivePaneID
}

func transcriptLines(sessionID string) ([]string, error) {
	cmd := exec.Command("ccrider", "view", sessionID)
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(out), "\n")
	if len(lines) > 400 {
		lines = lines[:400]
	}
	return lines, nil
}

func livePaneForSession(s *snapshot, sessionID string) string {
	if s == nil {
		return ""
	}
	for _, p := range s.Panes {
		if sessionMatchesPane(sessionID, p.AgentSession.Value) {
			return p.PaneID
		}
	}
	return ""
}

func txFilterLine(role, order string, width int) string {
	parts := []struct{ key, id, label string }{
		{"1", "typed", "Typed"},
		{"2", "any-user", "Any user"},
		{"3", "replies", "Replies"},
		{"4", "everything", "Everything"},
	}
	var b strings.Builder
	b.WriteString(" ")
	for _, p := range parts {
		chip := " " + p.key + " " + p.label + " "
		if p.id == role || (role == "" && p.id == "typed") || (role == "user" && p.id == "typed") {
			b.WriteString(keySelectBg + keySelectFg + keyBold + chip + keyReset)
		} else {
			b.WriteString(keyOverlay + chip + keyReset)
		}
		b.WriteString(" ")
	}
	b.WriteString(keyOverlay + " s " + txSortLabel(order) + keyReset)
	return clipANSI(b.String(), width)
}

func clipANSI(s string, n int) string {
	if n < 1 {
		return ""
	}
	return fitLine(s, n)
}

func txActionLine(hit txHit, live bool) string {
	open := "enter browse"
	if live {
		open = "enter open pane"
	}
	fork := "f fork Pi"
	if hit.Provider != "" && hit.Provider != "pi" {
		fork = "f Pi only"
	}
	return open + "    " + fork + "    b full thread"
}

// renderTranscriptSplit draws the hit list on the left and the nearby
// conversation on the right. Panes without a reminder are not involved.
func renderTranscriptSplit(width, height int, names []string, active int, fv vimline.View, status, role, order, query string, hits []txHit, snap *snapshot, sel, scrollOffset int, hit txHit, live bool, preview []string) string {
	if height < 8 {
		height = 8
	}
	maxLines := height
	if height > 6 {
		maxLines = height - 1
	}
	sc := newScreen(width, maxLines, 8)
	sc.header(names, active)
	sc.emit(renderField(fv, width))
	sc.emit(txFilterLine(role, order, width))
	sc.rule()
	left := width * 2 / 5
	if left < 28 {
		left = 28
	}
	if left > width-24 {
		left = width / 2
	}
	right := width - left - 1
	if right < 16 {
		right = 16
	}
	body := maxLines - len(sc.lines) - 3
	if body < 3 {
		body = 3
	}
	hitsVisible := body / 2
	if hitsVisible < 1 {
		hitsVisible = 1
	}
	start := scrollOffset
	if sel < start {
		start = sel
	}
	if sel >= start+hitsVisible {
		start = sel - hitsVisible + 1
	}
	if start < 0 {
		start = 0
	}
	now := time.Now()
	for i := 0; i < body; i++ {
		idx := start + i/2
		leftText := strings.Repeat(" ", left)
		if idx >= 0 && idx < len(hits) {
			meta := txLiveMeta(snap, hits[idx].SessionID)
			title, _ := txHitLines(hits[idx], query, meta, left-2, now)
			text := title
			color := keyText
			if i%2 == 1 {
				text = fitLine(highlightWords(hits[idx].Snippet, query, idx == sel), left-2)
				color = keySub
			}
			if idx == sel {
				leftText = keySelectBg + keySelectFg + pad(" "+text, left) + keyReset
			} else {
				leftText = color + pad(" "+text, left) + keyReset
			}
		}
		rightText := ""
		if i == 0 && hit.SessionID != "" {
			who := txTitle(hit, txLiveMeta(snap, hit.SessionID))
			rightText = keyAccentFg + clip(who+"  "+txActionLine(hit, live), right) + keyReset
		} else if i-1 >= 0 && i-1 < len(preview) {
			line := preview[i-1]
			color := keyText
			if strings.HasPrefix(strings.TrimSpace(line), ">") {
				color = keyAccentFg
			}
			rightText = color + clip(line, right) + keyReset
		}
		sc.emit(leftText + keyOverlay + "\u2502" + keyReset + rightText)
	}
	sc.rule()
	if status != "" {
		sc.emit(keySub + clip(status, width-1) + keyReset)
	}
	return sc.done()
}
