package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

// jev.go is the optional triage-priority module.
//
// Jev is the house reflex layer. When its decision endpoint is reachable
// through the Infisical proxy, each new Inbox arrival is asked one question:
// should the captain triage this thread before the others right now. The
// answer orders new arrivals, most important first.
//
// Preference learning is local and needs no endpoint. Every time the captain
// acts on an Inbox row, its features are reinforced and the features of the
// rows skipped above it are decayed, so the ordering drifts toward what this
// captain actually handles first.
//
// The module is optional by contract. With no proxy on the box and no
// HERDR_RECALL_JEV override, every function here is a no-op and the picker
// behaves exactly as it did before Jev existed.

const jevDefaultProxy = "/opt/ra/infisical/bin/jev-proxy.sh"

type jevState struct {
	Proxy     string             `json:"proxy,omitempty"`
	Scores    map[string]float64 `json:"scores,omitempty"`
	ScoreKey  map[string]string  `json:"score_key,omitempty"`
	Weights   map[string]float64 `json:"weights,omitempty"`
	Rules     []jevRule          `json:"rules,omitempty"`
	DocFreq   map[string]int     `json:"doc_freq,omitempty"`
	DocCount  int                `json:"doc_count,omitempty"`
	Examples  int                `json:"examples,omitempty"`
	Teachings []jevTeaching      `json:"teachings,omitempty"`
}

// jevRule is a conditional preference. It fires only when the row matches
// When, so "demote idle until afternoon" does not bury idle threads all day.
type jevRule struct {
	ID       string   `json:"id"`
	Text     string   `json:"text,omitempty"`
	When     []string `json:"when,omitempty"`
	WhenMode string   `json:"when_mode,omitempty"` // all or any
	Target   []string `json:"target,omitempty"`
	Sign     float64  `json:"sign"`
	Delta    float64  `json:"delta"`
}

func (s *store) ensureJev() {
	if s.Jev == nil {
		s.Jev = &jevState{}
	}
	// Every map must exist before any write. applyJev used to seed Scores
	// without Weights, so the first jevLearn on Inbox wrote to a nil map,
	// panicked, and killed the picker before the Enter jump ran.
	if s.Jev.Weights == nil {
		s.Jev.Weights = map[string]float64{}
	}
	if s.Jev.Scores == nil {
		s.Jev.Scores = map[string]float64{}
	}
	if s.Jev.ScoreKey == nil {
		s.Jev.ScoreKey = map[string]string{}
	}
}

// jevProxyBin resolves the decision proxy, or "" when Jev is absent.
func jevProxyBin() string {
	switch strings.TrimSpace(strings.ToLower(os.Getenv("HERDR_RECALL_JEV"))) {
	case "off", "0", "no", "false":
		return ""
	}
	if v := strings.TrimSpace(os.Getenv("HERDR_RECALL_JEV")); v != "" && v != "on" {
		return v
	}
	if v := strings.TrimSpace(os.Getenv("FM_JEV_PROXY_BIN")); v != "" {
		return v
	}
	if fi, err := os.Stat(jevDefaultProxy); err == nil && !fi.IsDir() && fi.Mode()&0111 != 0 {
		return jevDefaultProxy
	}
	return ""
}

func jevEnabled() bool { return jevProxyBin() != "" }

// jevOn reports whether the module is active for this store.
func (s *store) jevOn() bool { return s.Jev != nil && jevEnabled() }

type jevScore struct {
	noul float64
	key  string
}

// jevScoreRows asks the endpoint about every row, concurrently, inside a hard
// budget. It touches nothing but its arguments, so it is safe to run while the
// picker loop is idle.
func jevScoreRows(rows []row, budget time.Duration) map[string]jevScore {
	proxy := jevProxyBin()
	if proxy == "" || len(rows) == 0 {
		return nil
	}
	base := strings.TrimSpace(os.Getenv("FM_JEV_TS_BASE"))
	if base == "" {
		base = "https://api.typesafe.ai"
	}
	model := strings.TrimSpace(os.Getenv("FM_JEV_TS_MODEL"))
	if model == "" {
		model = "jev-latest"
	}
	if len(rows) > 8 {
		rows = rows[:8]
	}
	deadline := time.Now().Add(budget)
	type res struct {
		id   string
		noul float64
		key  string
	}
	ch := make(chan res, len(rows))
	var wg sync.WaitGroup
	for _, r := range rows {
		wg.Add(1)
		go func(r row) {
			defer wg.Done()
			if time.Now().After(deadline) {
				return
			}
			n := jevAsk(proxy, base, model, r)
			ch <- res{r.PaneID, n, jevRowKey(r)}
		}(r)
	}
	go func() { wg.Wait(); close(ch) }()
	out := make(map[string]jevScore, len(rows))
	for x := range ch {
		out[x.id] = jevScore{noul: x.noul, key: x.key}
	}
	return out
}

func jevAsk(proxy, base, model string, r row) float64 {
	payload := map[string]any{
		"model": model,
		"state": map[string]string{
			"tab":      r.Label,
			"status":   statusWord(r.Status),
			"note":     jevClip(r.Note, 600),
			"agent":    r.Agent,
			"cwd":      r.CWD,
			"features": strings.Join(jevFeatures(r, time.Now().Unix()), " "),
		},
		"questions": map[string]any{
			"triage": map[string]any{
				"type":         "noul",
				"instructions": "Should the captain triage this thread before the others right now?",
			},
		},
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return 0
	}
	cmd := exec.Command(proxy, "-fsS", "--max-time", "2", "-X", "POST",
		strings.TrimRight(base, "/")+"/v1/systemone",
		"-H", "Content-Type: application/json", "--data-binary", "@-")
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	var resp struct {
		Answers struct {
			Triage struct {
				Noul float64 `json:"noul"`
			} `json:"triage"`
		} `json:"answers"`
	}
	if json.Unmarshal(out, &resp) != nil {
		return 0
	}
	return resp.Answers.Triage.Noul
}

func jevClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// jevRowKey identifies the content a score was computed for, so a thread that
// changed since scoring is scored again instead of using a stale answer.
func jevRowKey(r row) string {
	h := sha1.Sum([]byte(r.Label + "\x00" + statusWord(r.Status) + "\x00" + r.Note))
	return hex.EncodeToString(h[:8])
}

// ---------------------------------------------------------------------------
// local preference learning
// ---------------------------------------------------------------------------

func jevAgeBucket(r row, now int64) string {
	if r.Last == 0 {
		return "age:none"
	}
	d := now - r.Last
	switch {
	case d < 10*60:
		return "age:fresh"
	case d < time.Hour.Milliseconds()/1000:
		return "age:hour"
	case d < 24*3600:
		return "age:day"
	default:
		return "age:old"
	}
}

// jevFeatures names the dimensions preference is learned over.
// Topic words come from the pane itself. There is no topic dictionary.
func jevFeatures(r row, now int64) []string {
	var f []string
	if sw := statusWord(r.Status); sw != "" {
		f = append(f, "status:"+sw)
	}
	if a := strings.ToLower(strings.TrimSpace(r.Agent)); a != "" && a != "unknown" && a != "none" {
		f = append(f, "agent:"+a)
	}
	f = append(f, jevAgeBucket(r, now))
	f = append(f, jevVisitFeature(r.Visits))
	f = append(f, jevCadence(now))
	if r.Note != "" {
		f = append(f, "note:yes")
	}
	if r.ManualBlocked {
		f = append(f, "manual:yes")
	}
	if r.RemindAt > 0 {
		f = append(f, "remind:yes")
		if r.RemindAt <= now {
			f = append(f, "remind:due")
		}
	}
	if r.Fav {
		f = append(f, "favorite:yes")
	}
	if repo := jevRepoFeature(r.CWD); repo != "" {
		f = append(f, repo)
	}
	sw := statusWord(r.Status)
	if (sw == "blocked" || sw == "done") && r.Last > 0 && now-r.Last > 30*60 {
		f = append(f, "attention:waiting_on_human")
	}
	if r.Last > 0 && now-r.Last < 5*60 {
		f = append(f, "attention:hot")
	}
	f = append(f, jevContentFeatures(r.Label, r.Note, r.Workspace)...)
	if lab := jevLabelFeature(r.Label); lab != "" {
		f = append(f, lab)
	}
	sort.Strings(f)
	out := f[:0]
	prev := ""
	for _, x := range f {
		if x == prev || x == "" {
			continue
		}
		prev = x
		out = append(out, x)
	}
	return out
}

// jevSignalFeatures is the subset implicit actions may train. Time of day,
// visit count, and "just touched" are context, not a reason to bury a project.
// Common tokens and path-only tokens wait for an explicit chip or sentence.
func (s *store) jevSignalFeatures(r row, now int64) []string {
	var out []string
	for _, f := range jevFeatures(r, now) {
		kind, val, _ := strings.Cut(f, ":")
		switch kind {
		case "cadence", "visit", "age":
			continue
		case "attention":
			if val == "hot" {
				continue
			}
		case "tok":
			if s.jevCommon(f) {
				continue
			}
			if !jevTextHasToken(r.Label, val) && !jevTextHasToken(r.Note, val) && !jevTextHasToken(r.Workspace, val) {
				continue
			}
		}
		out = append(out, f)
	}
	return out
}

func (s *store) jevWeight(r row, now int64) float64 {
	if s.Jev == nil || len(s.Jev.Weights) == 0 {
		return 0
	}
	feats := jevFeatures(r, now)
	have := map[string]bool{}
	sum := 0.0
	for _, f := range feats {
		have[f] = true
		sum += s.Jev.Weights[f]
	}
	values := jevValueSet(r, feats)
	for f, w := range s.Jev.Weights {
		if w == 0 || have[f] {
			continue
		}
		if jevAliasHit(f, values) {
			sum += w
		}
	}
	return sum
}

// jevPriority combines the endpoint answer with the learned preference.
func (s *store) jevPriority(r row, now int64) float64 {
	if s.Jev == nil {
		return 0
	}
	p := s.Jev.Scores[r.PaneID]
	if s.Jev.ScoreKey[r.PaneID] != jevRowKey(r) {
		p = 0
	}
	return p + s.jevWeight(r, now) + s.jevRuleBonus(r, now) + jevUrgency(r, now)
}

// jevUrgency is a small cold-start prior. Learned weights can override it.
func jevUrgency(r row, now int64) float64 {
	if r.RemindAt > 0 && r.RemindAt <= now {
		return 0.2
	}
	switch statusWord(r.Status) {
	case "blocked":
		return 0.15
	case "done":
		return 0.08
	case "working":
		return 0.02
	}
	return 0
}

// jevLearn reinforces the chosen row and decays the rows skipped above it.
// Alpha is small so a single action never reorders the world.
// Learning is local. A nil Jev state stays nil so a caller that did not opt in
// cannot start a weight map by accident.
func (s *store) jevLearn(chosen row, skipped []row) {
	s.jevLearnScaled(chosen, skipped, 0.05)
}

func (s *store) jevLearnScaled(chosen row, skipped []row, alpha float64) {
	if s.Jev == nil {
		return
	}
	s.ensureJev()
	now := time.Now().Unix()
	bump := func(r row, sign float64) {
		for _, f := range s.jevSignalFeatures(r, now) {
			s.Jev.Weights[f] = clampWeight(s.Jev.Weights[f] + sign*alpha)
		}
	}
	bump(chosen, 1)
	for _, r := range skipped {
		bump(r, -0.5)
	}
	s.Jev.Examples++
}

// jevPrefer records explicit feedback on one row. Plus bumps its features,
// minus decays them. This is local weight math, so it works without the
// decision proxy; only the arrival scoring needs the endpoint.
func (s *store) jevPrefer(r row, up bool) {
	s.jevPreferSelected(jevFeatures(r, time.Now().Unix()), up)
}

// jevPreferSelected applies a plus or minus only to the features the captain
// checked. Unchecked features are not credited for this teaching.
func (s *store) jevPreferSelected(features []string, up bool) {
	if len(features) == 0 {
		return
	}
	s.ensureJev()
	sign := -1.0
	if up {
		sign = 1
	}
	for _, f := range features {
		s.Jev.Weights[f] = clampWeight(s.Jev.Weights[f] + sign*0.1)
	}
	s.Jev.Examples++
}

// jevSortDesc sorts rows by descending priority, most important first.
func (s *store) jevSortDesc(list []row, now int64) {
	sort.SliceStable(list, func(i, j int) bool {
		pi, pj := s.jevPriority(list[i], now), s.jevPriority(list[j], now)
		if pi != pj {
			return pi > pj
		}
		if list[i].Last != list[j].Last {
			return list[i].Last > list[j].Last
		}
		return list[i].PaneID < list[j].PaneID
	})
}

// jevTeaching captures full real-time feedback context from the captain.
type jevTeaching struct {
	ID          string            `json:"id"`
	Timestamp   int64             `json:"timestamp"`
	TimeRFC3339 string            `json:"time_rfc3339"`
	Feedback    string            `json:"feedback"`
	Intent      string            `json:"intent"` // "prefer", "demote", "directive", "feedback"
	Pane        jevPaneContext    `json:"pane"`
	InboxRank   int               `json:"inbox_rank"`
	InboxTotal  int               `json:"inbox_total"`
	Queue       []jevQueueContext `json:"queue,omitempty"`
	Features    []string          `json:"features"`
	Selected    []string          `json:"selected,omitempty"`
	RuleTexts   []string          `json:"rule_texts,omitempty"`
	Session     jevSessionContext `json:"session"`
}

type jevPaneContext struct {
	PaneID        string  `json:"pane_id"`
	Label         string  `json:"label"`
	Status        string  `json:"status"`
	CWD           string  `json:"cwd,omitempty"`
	Note          string  `json:"note,omitempty"`
	ManualBlocked bool    `json:"manual_blocked"`
	RemindAt      int64   `json:"remind_at,omitempty"`
	SnoozeMode    string  `json:"snooze_mode,omitempty"`
	SnoozeState   string  `json:"snooze_state,omitempty"`
	Done          bool    `json:"done"`
	DoneAt        int64   `json:"done_at,omitempty"`
	DoneState     string  `json:"done_state,omitempty"`
	Visits        int64   `json:"visits"`
	Last          int64   `json:"last"`
	JevScore      float64 `json:"jev_score"`
	JevPriority   float64 `json:"jev_priority"`
}

type jevQueueContext struct {
	PaneID   string  `json:"pane_id"`
	Label    string  `json:"label"`
	Status   string  `json:"status"`
	Priority float64 `json:"priority"`
}

type jevSessionContext struct {
	ActiveTab    string `json:"active_tab"`
	ActiveFilter string `json:"active_filter,omitempty"`
	ActiveQuery  string `json:"active_query,omitempty"`
	SocketPath   string `json:"socket_path,omitempty"`
	FocusedPane  string `json:"focused_pane,omitempty"`
}

func (s *store) JevScore(paneID string) float64 {
	if s.Jev == nil || s.Jev.Scores == nil {
		return 0
	}
	return s.Jev.Scores[paneID]
}

func recordJevTeaching(st *store, t jevTeaching) error {
	st.ensureJev()
	st.Jev.Teachings = append(st.Jev.Teachings, t)
	if len(st.Jev.Teachings) > 200 {
		st.Jev.Teachings = st.Jev.Teachings[len(st.Jev.Teachings)-200:]
	}

	logPath := filepath.Join(configDir(), "jev-teachings.jsonl")
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func clampWeight(w float64) float64 {
	if w > 1 {
		return 1
	}
	if w < -1 {
		return -1
	}
	return w
}

func jevVisitFeature(v int64) string {
	switch {
	case v > 50:
		return "visit:heavy"
	case v >= 10:
		return "visit:frequent"
	case v > 0:
		return "visit:low"
	default:
		return "visit:zero"
	}
}

func jevCadence(now int64) string {
	hr := time.Unix(now, 0).Local().Hour()
	switch {
	case hr >= 6 && hr < 12:
		return "cadence:morning"
	case hr >= 12 && hr < 18:
		return "cadence:afternoon"
	case hr >= 18:
		return "cadence:evening"
	default:
		return "cadence:night"
	}
}

func jevRepoFeature(cwd string) string {
	if cwd == "" {
		return ""
	}
	base := strings.ToLower(filepath.Base(cwd))
	if base == "" || base == "." || base == "/" {
		return ""
	}
	return "repo:" + base
}

func jevLabelFeature(label string) string {
	toks := strings.Fields(label)
	if len(toks) == 0 {
		return ""
	}
	tok := strings.ToLower(toks[0])
	if tok == "" {
		return ""
	}
	return "label:" + tok
}

// jevContentFeatures turns the pane's own words into features.
// A new project name appears here with no code change.
func jevContentFeatures(parts ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		for _, w := range jevSplitWords(part) {
			if len(w) < 2 || jevGrammarWord(w) || seen[w] {
				continue
			}
			seen[w] = true
			out = append(out, "tok:"+w)
			if len(out) >= 24 {
				sort.Strings(out)
				return out
			}
		}
	}
	sort.Strings(out)
	return out
}

func jevTextHasToken(text, word string) bool {
	for _, w := range jevSplitWords(text) {
		if w == word {
			return true
		}
	}
	return false
}

func jevValueSet(r row, feats []string) map[string]bool {
	values := map[string]bool{}
	for _, f := range feats {
		_, val, ok := strings.Cut(f, ":")
		if ok && val != "" && val != "yes" {
			values[val] = true
		}
	}
	if a := strings.ToLower(strings.TrimSpace(r.Agent)); a != "" && a != "unknown" && a != "none" {
		values[a] = true
	}
	if sw := statusWord(r.Status); sw != "" {
		values[sw] = true
	}
	if repo := jevRepoFeature(r.CWD); repo != "" {
		_, val, _ := strings.Cut(repo, ":")
		values[val] = true
	}
	return values
}

// jevAliasHit lets an old domain: weight or a tok: weight match a row that
// carries the same word as an agent, a repo, or a term.
func jevAliasHit(feature string, values map[string]bool) bool {
	kind, val, ok := strings.Cut(feature, ":")
	if !ok {
		return false
	}
	switch kind {
	case "tok", "label", "domain":
		return values[val]
	}
	return false
}

// observeRows rebuilds how common each feature is in the current panes.
// Common terms stay visible but start unchecked, so path noise is not trained
// by accident.
func (s *store) observeRows(rows []row, now int64) {
	s.ensureJev()
	freq := map[string]int{}
	docs := 0
	for _, r := range rows {
		docs++
		seen := map[string]bool{}
		for _, f := range jevFeatures(r, now) {
			if seen[f] {
				continue
			}
			seen[f] = true
			freq[f]++
		}
	}
	s.Jev.DocFreq = freq
	s.Jev.DocCount = docs
}

func (s *store) jevCommon(feature string) bool {
	if s == nil || s.Jev == nil || s.Jev.DocCount < 4 || s.Jev.DocFreq == nil {
		return false
	}
	return float64(s.Jev.DocFreq[feature])/float64(s.Jev.DocCount) >= 0.5
}

func jevSplitWords(s string) []string {
	return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// jevSchemaToken maps the picker's own status and time words.
// It is not a topic list. Any other word becomes tok:<word>.
func jevSchemaToken(word string) (string, bool) {
	switch word {
	case "blocked", "working", "done", "idle":
		return "status:" + word, true
	case "fresh", "recent":
		return "age:fresh", true
	case "stale", "old":
		return "age:old", true
	case "morning", "afternoon", "evening", "night":
		return "cadence:" + word, true
	case "note", "notes":
		return "note:yes", true
	case "reminder", "remind":
		return "remind:yes", true
	case "overdue":
		return "remind:due", true
	case "favorite", "starred":
		return "favorite:yes", true
	}
	return "", false
}

func jevGrammarWord(w string) bool {
	return jevStopWord(w) || jevPreferWord(w) || jevDemoteWord(w) || w == "never"
}

func jevStopWord(w string) bool {
	switch w {
	case "a", "an", "the", "this", "that", "these", "those", "thread", "threads", "pane", "panes",
		"agent", "agents", "and", "or", "to", "for", "in", "on", "with", "of", "is", "are", "be",
		"it", "my", "me", "than", "then", "just", "please", "because", "so", "from", "over",
		"instead", "before", "ahead", "until", "when", "if", "during", "not", "now", "right",
		"time", "show", "inbox", "always", "at", "by":
		return true
	}
	return false
}

func jevPreferWord(w string) bool {
	switch w {
	case "prefer", "prioritize", "priority", "boost", "urgent", "important", "first", "surface", "sooner", "higher":
		return true
	}
	return false
}

func jevDemoteWord(w string) bool {
	switch w {
	case "demote", "bury", "later", "ignore", "park", "lower", "downrank", "defer", "less":
		return true
	}
	return false
}

func resolveJevToken(word string, rowFeats []string) string {
	word = strings.ToLower(strings.TrimSpace(word))
	if word == "" || jevGrammarWord(word) || len(word) < 2 {
		return ""
	}
	if f := matchRowFeature(word, rowFeats); f != "" {
		return f
	}
	if f, ok := jevSchemaToken(word); ok {
		return f
	}
	return "tok:" + word
}

func matchRowFeature(word string, rowFeats []string) string {
	tok := ""
	for _, rf := range rowFeats {
		kind, val, ok := strings.Cut(rf, ":")
		if !ok || val != word {
			continue
		}
		switch kind {
		case "status", "agent", "repo", "note", "remind", "favorite", "manual":
			return rf
		case "tok", "label", "domain":
			if tok == "" {
				tok = rf
			}
		}
	}
	return tok
}

func resolveJevWords(words []string, rowFeats []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range words {
		f := resolveJevToken(w, rowFeats)
		if f == "" || seen[f] {
			continue
		}
		seen[f] = true
		out = append(out, f)
	}
	return out
}

func normalizeJevText(text string) string {
	raw := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == ' ' || r == '-' {
			return unicode.ToLower(r)
		}
		return ' '
	}, text)
	return strings.Join(strings.Fields(raw), " ")
}

func jevTextHasPolarity(text string) bool {
	for _, w := range strings.Fields(normalizeJevText(text)) {
		if jevPreferWord(w) || jevDemoteWord(w) || w == "never" {
			return true
		}
	}
	return false
}

func jevPolarity(raw string) float64 {
	preferN, demoteN := 0, 0
	for _, w := range strings.Fields(raw) {
		if jevPreferWord(w) {
			preferN++
		}
		if jevDemoteWord(w) || w == "never" {
			demoteN++
		}
	}
	if preferN > demoteN {
		return 1
	}
	if demoteN > preferN {
		return -1
	}
	return 0
}

func jevUntilWhen(word string) []string {
	switch word {
	case "afternoon":
		return []string{"cadence:morning"}
	case "evening":
		return []string{"cadence:morning", "cadence:afternoon"}
	case "night", "tonight":
		return []string{"cadence:morning", "cadence:afternoon", "cadence:evening"}
	case "morning":
		return []string{"cadence:night"}
	default:
		return nil
	}
}

func splitJevContext(raw string) (body, cond string, ok bool) {
	for _, p := range []string{"when ", "if ", "during "} {
		if strings.HasPrefix(raw, p) {
			fields := strings.Fields(raw[len(p):])
			var condF, bodyF []string
			seenPol := false
			for _, f := range fields {
				if !seenPol && (jevPreferWord(f) || jevDemoteWord(f) || f == "never") {
					seenPol = true
					bodyF = append(bodyF, f)
					continue
				}
				if !seenPol {
					condF = append(condF, f)
				} else {
					bodyF = append(bodyF, f)
				}
			}
			if len(condF) == 0 || len(bodyF) == 0 {
				return "", "", false
			}
			return strings.Join(bodyF, " "), strings.Join(condF, " "), true
		}
	}
	for _, sep := range []string{" when ", " if ", " during "} {
		if i := strings.Index(raw, sep); i >= 0 {
			return raw[:i], raw[i+len(sep):], true
		}
	}
	return "", "", false
}

// parseJevFeedback turns free text into weight deltas and conditional rules.
// Unknown words are ignored unless they match a feature already on the row.
func parseJevFeedback(text string, rowFeats []string) (map[string]float64, []jevRule) {
	raw := normalizeJevText(text)
	if raw == "" {
		return nil, nil
	}
	sign := jevPolarity(raw)

	if i := strings.Index(raw, " until "); i >= 0 {
		subject := resolveJevWords(strings.Fields(raw[:i]), rowFeats)
		rest := strings.Fields(raw[i+len(" until "):])
		var when []string
		if len(rest) > 0 {
			when = jevUntilWhen(rest[0])
			if len(when) == 0 {
				when = resolveJevWords(rest[:1], rowFeats)
			}
		}
		if sign == 0 {
			sign = -1
		}
		if len(subject) > 0 && len(when) > 0 {
			mode := "all"
			if len(when) > 1 {
				mode = "any"
			}
			return nil, []jevRule{{
				Text:     strings.TrimSpace(text),
				When:     when,
				WhenMode: mode,
				Target:   subject,
				Sign:     sign,
				Delta:    0.25,
			}}
		}
	}

	if body, cond, ok := splitJevContext(raw); ok {
		when := resolveJevWords(strings.Fields(cond), rowFeats)
		target := resolveJevWords(strings.Fields(body), rowFeats)
		if sign == 0 {
			sign = 1
		}
		if len(when) > 0 && len(target) > 0 {
			return nil, []jevRule{{
				Text:     strings.TrimSpace(text),
				When:     when,
				WhenMode: "all",
				Target:   target,
				Sign:     sign,
				Delta:    0.25,
			}}
		}
	}

	for _, sep := range []string{" over ", " instead of ", " ahead of ", " before "} {
		if i := strings.Index(raw, sep); i >= 0 {
			left := resolveJevWords(strings.Fields(raw[:i]), rowFeats)
			right := resolveJevWords(strings.Fields(raw[i+len(sep):]), rowFeats)
			adj := map[string]float64{}
			for _, f := range left {
				adj[f] += 0.2
			}
			for _, f := range right {
				adj[f] -= 0.2
			}
			if len(adj) > 0 {
				return adj, nil
			}
		}
	}

	if sign == 0 {
		return nil, nil
	}
	feats := resolveJevWords(strings.Fields(raw), rowFeats)
	if len(feats) == 0 {
		return nil, nil
	}
	adj := map[string]float64{}
	for _, f := range feats {
		adj[f] = sign * 0.2
	}
	return adj, nil
}

func (s *store) jevRuleBonus(r row, now int64) float64 {
	if s.Jev == nil || len(s.Jev.Rules) == 0 {
		return 0
	}
	feats := jevFeatures(r, now)
	have := map[string]bool{}
	for _, f := range feats {
		have[f] = true
	}
	values := jevValueSet(r, feats)
	covers := func(feature string) bool {
		return have[feature] || jevAliasHit(feature, values)
	}
	sum := 0.0
	for _, rule := range s.Jev.Rules {
		if !jevRuleMatches(rule, have) {
			continue
		}
		if len(rule.Target) > 0 {
			hit := false
			for _, t := range rule.Target {
				if covers(t) {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
		}
		sum += rule.Sign * rule.Delta
	}
	return sum
}

func jevRuleMatches(rule jevRule, have map[string]bool) bool {
	if len(rule.When) == 0 {
		return true
	}
	if rule.WhenMode == "any" {
		for _, w := range rule.When {
			if have[w] {
				return true
			}
		}
		return false
	}
	for _, w := range rule.When {
		if !have[w] {
			return false
		}
	}
	return true
}

func jevRuleID(rule jevRule) string {
	w := append([]string{}, rule.When...)
	t := append([]string{}, rule.Target...)
	sort.Strings(w)
	sort.Strings(t)
	mode := rule.WhenMode
	if mode == "" {
		mode = "all"
	}
	sum := sha1.Sum([]byte(mode + "|" + strings.Join(w, ",") + "|" + strings.Join(t, ",") + "|" + fmt.Sprintf("%g", rule.Sign)))
	return hex.EncodeToString(sum[:6])
}

func (s *store) addJevRule(rule jevRule) {
	s.ensureJev()
	if rule.WhenMode == "" {
		rule.WhenMode = "all"
	}
	if rule.Delta == 0 {
		rule.Delta = 0.25
	}
	if rule.Sign == 0 {
		rule.Sign = 1
	}
	rule.ID = jevRuleID(rule)
	for i := range s.Jev.Rules {
		if s.Jev.Rules[i].ID == rule.ID {
			d := s.Jev.Rules[i].Delta + rule.Delta
			if d > 0.8 {
				d = 0.8
			}
			s.Jev.Rules[i].Delta = d
			if rule.Text != "" {
				s.Jev.Rules[i].Text = rule.Text
			}
			return
		}
	}
	if len(s.Jev.Rules) >= 40 {
		s.Jev.Rules = s.Jev.Rules[1:]
	}
	s.Jev.Rules = append(s.Jev.Rules, rule)
}

func (s *store) removeJevRule(id string) {
	if s.Jev == nil {
		return
	}
	out := s.Jev.Rules[:0]
	for _, rule := range s.Jev.Rules {
		if rule.ID != id {
			out = append(out, rule)
		}
	}
	s.Jev.Rules = out
}

func (s *store) applyJevDirective(adj map[string]float64, rules []jevRule) {
	if len(adj) == 0 && len(rules) == 0 {
		return
	}
	s.ensureJev()
	for f, d := range adj {
		s.Jev.Weights[f] = clampWeight(s.Jev.Weights[f] + d)
	}
	for _, rule := range rules {
		s.addJevRule(rule)
	}
	s.Jev.Examples++
}
