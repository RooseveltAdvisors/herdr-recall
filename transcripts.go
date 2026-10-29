package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// txHit is one ccrider search match the Transcripts tab can open or fork.
type txHit struct {
	SessionID string
	Provider  string
	CWD       string
	Summary   string
	Snippet   string
	When      string
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
	case "agent":
		return "assistant"
	case "all":
		return ""
	default:
		return "human"
	}
}

func nextTxRole(role string) string {
	switch role {
	case "user":
		return "agent"
	case "agent":
		return "all"
	default:
		return "user"
	}
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

func searchTranscripts(query, role string) ([]txHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	db := ccriderDB()
	if db == "" {
		return nil, fmt.Errorf("ccrider index is missing")
	}
	if _, err := os.Stat(db); err != nil {
		return nil, fmt.Errorf("ccrider index is not ready")
	}
	match := ftsQuery(query)
	if match == "" {
		return nil, nil
	}
	script := `
import json, sqlite3, sys
db, match, sender = sys.argv[1], sys.argv[2], sys.argv[3]
con = sqlite3.connect(f"file:{db}?mode=ro", uri=True)
sql = """
SELECT s.session_id, ifnull(s.provider,''), ifnull(s.cwd,''), ifnull(s.summary,''),
       substr(ifnull(m.text_content,''), 1, 180), ifnull(m.timestamp,'')
FROM messages_fts
JOIN messages m ON m.id = messages_fts.rowid
JOIN sessions s ON s.id = m.session_id
WHERE messages_fts MATCH ?
"""
args = [match]
if sender:
    sql += " AND m.sender = ?"
    args.append(sender)
sql += " ORDER BY m.timestamp DESC LIMIT 30"
rows = con.execute(sql, args).fetchall()
print(json.dumps(rows))
`
	cmd := exec.Command("python3", "-c", script, db, match, transcriptSender(role))
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("%s", msg)
	}
	var rows [][]string
	if err := json.Unmarshal(out, &rows); err != nil {
		return nil, err
	}
	hits := make([]txHit, 0, len(rows))
	for _, row := range rows {
		if len(row) < 6 {
			continue
		}
		hits = append(hits, txHit{
			SessionID: row[0],
			Provider:  row[1],
			CWD:       row[2],
			Summary:   row[3],
			Snippet:   strings.Join(strings.Fields(row[4]), " "),
			When:      row[5],
		})
	}
	return hits, nil
}

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
