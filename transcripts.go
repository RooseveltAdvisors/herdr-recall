package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

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

func searchTranscripts(query, role string) ([]txHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	var resp txReply
	if err := txAsk(map[string]any{"op": "search", "match": match, "sender": transcriptSender(role)}, &resp); err != nil {
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
			h.Snippet = strings.Join(strings.Fields(body), " ")
			kept = append(kept, h)
			if len(kept) == 40 {
				break
			}
		}
		hits = kept
	} else if len(hits) > 40 {
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
		hits, err := searchTranscripts(job.Query, job.Role)
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
                   substr(ifnull(m.text_content,''), 1, 800), ifnull(m.timestamp,'')
            FROM messages_fts
            JOIN messages m ON m.id = messages_fts.rowid
            JOIN sessions s ON s.id = m.session_id
            WHERE messages_fts MATCH ?
            """
            args = [req.get("match") or ""]
            if sender:
                sql += " AND m.sender = ?"
                args.append(sender)
            sql += " ORDER BY rank LIMIT 120"
            hits = []
            for row in con.execute(sql, args):
                    hits.append({
                        "SessionID": row[0], "MsgID": row[1], "Provider": row[2], "CWD": row[3],
                        "Summary": row[4] or "", "Snippet": " ".join((row[5] or "").split()), "When": row[6] or "",
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
		label := h.Snippet
		if label == "" {
			label = h.Summary
		}
		if label == "" {
			label = h.SessionID
		}
		note := h.Provider
		if h.When != "" {
			if note != "" {
				note += " · "
			}
			note += h.When
		}
		out = append(out, row{
			PaneID: h.SessionID,
			Label:  label,
			Note:   note,
			CWD:    h.CWD,
		})
	}
	return out
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

func txFilterLine(role string, width int) string {
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
func renderTranscriptSplit(width, height int, names []string, active int, fv vimline.View, status, role string, rows []row, sel, scrollOffset int, hit txHit, live bool, preview []string) string {
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
	sc.emit(txFilterLine(role, width))
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
	start := scrollOffset
	if sel < start {
		start = sel
	}
	if sel >= start+body {
		start = sel - body + 1
	}
	if start < 0 {
		start = 0
	}
	for i := 0; i < body; i++ {
		idx := start + i
		leftText := ""
		if idx < len(rows) {
			label := rows[idx].Label
			if rows[idx].Note != "" {
				label = rows[idx].Note + "  " + label
			}
			leftText = clip(label, left-2)
			if idx == sel {
				leftText = keySelectBg + keySelectFg + pad(" "+leftText, left) + keyReset
			} else {
				leftText = pad(" "+leftText, left)
			}
		} else {
			leftText = strings.Repeat(" ", left)
		}
		rightText := ""
		if i == 0 && hit.SessionID != "" {
			who := hit.Provider
			if who == "" {
				who = "session"
			}
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
