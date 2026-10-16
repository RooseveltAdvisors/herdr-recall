package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
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
	Proxy    string             `json:"proxy,omitempty"`
	Scores   map[string]float64 `json:"scores,omitempty"`
	ScoreKey map[string]string  `json:"score_key,omitempty"`
	Weights  map[string]float64 `json:"weights,omitempty"`
	Examples int                `json:"examples,omitempty"`
}

func (s *store) ensureJev() {
	if s.Jev == nil {
		s.Jev = &jevState{}
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
			"tab":    r.Label,
			"status": statusWord(r.Status),
			"note":   jevClip(r.Note, 600),
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
func jevFeatures(r row, now int64) []string {
	f := []string{
		"status:" + statusWord(r.Status),
		jevAgeBucket(r, now),
	}
	if r.Note != "" {
		f = append(f, "note:yes")
	}
	if r.ManualBlocked {
		f = append(f, "manual:yes")
	}
	if r.RemindAt > 0 {
		f = append(f, "remind:yes")
	}
	if toks := strings.Fields(r.Label); len(toks) > 0 {
		f = append(f, "label:"+strings.ToLower(toks[0]))
	}
	return f
}

func (s *store) jevWeight(r row, now int64) float64 {
	if s.Jev == nil || len(s.Jev.Weights) == 0 {
		return 0
	}
	sum := 0.0
	for _, f := range jevFeatures(r, now) {
		sum += s.Jev.Weights[f]
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
	return p + s.jevWeight(r, now)
}

// jevLearn reinforces the chosen row and decays the rows skipped above it.
// Alpha is small so a single action never reorders the world.
func (s *store) jevLearn(chosen row, skipped []row) {
	if !s.jevOn() {
		return
	}
	s.ensureJev()
	now := time.Now().Unix()
	const alpha = 0.05
	bump := func(r row, sign float64) {
		for _, f := range jevFeatures(r, now) {
			w := s.Jev.Weights[f] + sign*alpha
			if w > 1 {
				w = 1
			}
			if w < -1 {
				w = -1
			}
			s.Jev.Weights[f] = w
		}
	}
	bump(chosen, 1)
	for _, r := range skipped {
		bump(r, -0.5)
	}
	s.Jev.Examples++
}

// jevOrderAscending sorts rows by descending priority, most important first.
func (s *store) jevSortDesc(list []row, now int64) {
	sort.SliceStable(list, func(i, j int) bool {
		return s.jevPriority(list[i], now) > s.jevPriority(list[j], now)
	})
}
