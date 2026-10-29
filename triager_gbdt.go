package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// gbdtModel is a small boosted-tree ranker scored in process.
// Training happens offline. The picker never calls Jev to use it.
type gbdtModel struct {
	Features []string `json:"features"`
	Init     float64  `json:"init"`
	Trees    []gbNode `json:"trees"`
}

type gbNode struct {
	Leaf  *float64 `json:"leaf,omitempty"`
	Feat  int      `json:"feat,omitempty"`
	Thr   float64  `json:"thr,omitempty"`
	Left  *gbNode  `json:"left,omitempty"`
	Right *gbNode  `json:"right,omitempty"`
}

var (
	gbdtOnce sync.Once
	gbdtLive *gbdtModel
)

func triagerModelPath() string {
	return filepath.Join(configDir(), "triager-gbdt.json")
}

func triagerTrainPath() string {
	return filepath.Join(configDir(), "triager-train.json")
}

func writeTriagerExamples(st *store, rows []row, now int64) (int, error) {
	if st == nil || st.Jev == nil {
		return 0, nil
	}
	type ex struct {
		Y     float64            `json:"y"`
		Feats map[string]float64 `json:"feats"`
	}
	var out []ex
	for _, r := range rows {
		if st.Jev.ScoreKey[r.PaneID] != jevRowKey(r) {
			continue
		}
		y, ok := st.Jev.Scores[r.PaneID]
		if !ok {
			continue
		}
		feats := map[string]float64{}
		for _, f := range jevFeatures(r, now) {
			feats[f] = 1
		}
		out = append(out, ex{Y: y + teachShift(st, r, now), Feats: feats})
	}
	if len(out) < 8 {
		return len(out), nil
	}
	b, err := json.Marshal(map[string]any{"rows": out})
	if err != nil {
		return 0, err
	}
	return len(out), os.WriteFile(triagerTrainPath(), b, 0o600)
}

func loadGBDT() *gbdtModel {
	gbdtOnce.Do(func() {
		b, err := os.ReadFile(triagerModelPath())
		if err != nil {
			return
		}
		var m gbdtModel
		if json.Unmarshal(b, &m) != nil || len(m.Trees) == 0 || len(m.Features) == 0 {
			return
		}
		gbdtLive = &m
	})
	return gbdtLive
}

func (m *gbdtModel) score(feats map[string]float64) float64 {
	if m == nil {
		return 0
	}
	sum := m.Init
	for i := range m.Trees {
		sum += walkGBDT(&m.Trees[i], m.Features, feats)
	}
	return sum
}

func walkGBDT(n *gbNode, names []string, feats map[string]float64) float64 {
	if n == nil {
		return 0
	}
	if n.Leaf != nil {
		return *n.Leaf
	}
	v := 0.0
	if n.Feat >= 0 && n.Feat < len(names) {
		v = feats[names[n.Feat]]
	}
	if v <= n.Thr {
		return walkGBDT(n.Left, names, feats)
	}
	return walkGBDT(n.Right, names, feats)
}

func (m *gbdtModel) scoreRow(r row, now int64) float64 {
	feats := map[string]float64{}
	for _, f := range jevFeatures(r, now) {
		feats[f] = 1
	}
	return m.score(feats)
}
