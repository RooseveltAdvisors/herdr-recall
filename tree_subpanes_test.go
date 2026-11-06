package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetectTabPaneLayoutsTopLeftIsMain(t *testing.T) {
	snap := &snapshot{
		Panes: []paneRec{
			{PaneID: "w1:pRight", TabID: "t1", WorkspaceID: "w1"},
			{PaneID: "w1:pTopLeft", TabID: "t1", WorkspaceID: "w1"},
			{PaneID: "w1:pBottom", TabID: "t1", WorkspaceID: "w1"},
		},
		Layouts: []layoutRec{
			{
				TabID: "t1",
				Panes: []layoutPane{
					{PaneID: "w1:pRight", Rect: rect{X: 80, Y: 0, Width: 80, Height: 20}},
					{PaneID: "w1:pBottom", Rect: rect{X: 0, Y: 20, Width: 80, Height: 20}},
					{PaneID: "w1:pTopLeft", Rect: rect{X: 0, Y: 0, Width: 80, Height: 20}},
				},
			},
		},
	}

	layouts := detectTabPaneLayouts(snap)
	info := layouts["t1"]
	if info == nil {
		t.Fatal("expected layout info for t1")
	}
	if info.MainPaneID != "w1:pTopLeft" {
		t.Fatalf("expected top-left w1:pTopLeft as main, got %q", info.MainPaneID)
	}
	if len(info.SubpaneIDs) != 2 {
		t.Fatalf("expected 2 subpanes, got %d", len(info.SubpaneIDs))
	}
	if info.Positions["w1:pRight"] != "right" {
		t.Fatalf("expected right position, got %q", info.Positions["w1:pRight"])
	}
	if info.Positions["w1:pBottom"] != "bottom" {
		t.Fatalf("expected bottom position, got %q", info.Positions["w1:pBottom"])
	}
}

func TestSubpaneDisplayLabelFormatting(t *testing.T) {
	// 1. Explicit pane label takes precedence over pane ID
	pNamed := paneRec{
		PaneID: "w1:p16T",
		Label:  "beads-qa-checks",
		Agent:  "pi",
	}
	gotNamed := subpaneDisplayLabel(nil, pNamed, "", "right")
	if gotNamed != "beads-qa-checks · pi · (right)" {
		t.Fatalf("expected pane name beads-qa-checks without pane ID, got: %q", gotNamed)
	}

	// 2. Custom terminal title used instead of pane ID
	pTitle := paneRec{
		PaneID:             "w1:p116",
		TerminalTitleStrip: "worker-build",
		Agent:              "pi",
	}
	gotTitle := subpaneDisplayLabel(nil, pTitle, "", "right")
	if gotTitle != "worker-build · pi · (right)" {
		t.Fatalf("expected title worker-build without pane ID, got: %q", gotTitle)
	}

	// 3. Generic title falls back to short pane ID
	pGeneric := paneRec{
		PaneID:             "w1:p117",
		TerminalTitleStrip: "π - wiseman",
		Agent:              "pi",
	}
	gotGeneric := subpaneDisplayLabel(nil, pGeneric, "", "bottom")
	if gotGeneric != "p117 · pi · (bottom)" {
		t.Fatalf("unnamed pane should fall back to short pane ID, got: %q", gotGeneric)
	}
}

func TestTreeAssemblyAndExpandCollapse(t *testing.T) {
	st := &store{Version: 2, Panes: map[string]*paneStat{}, ExpandedPanes: map[string]bool{}}
	snap := &snapshot{
		Panes: []paneRec{
			{PaneID: "w1:pMain", TabID: "t1", WorkspaceID: "w1", AgentStatus: "idle"},
			{PaneID: "w1:pSub1", TabID: "t1", WorkspaceID: "w1", AgentStatus: "working"},
			{PaneID: "w1:pSub2", TabID: "t1", WorkspaceID: "w1", AgentStatus: "done"},
			{PaneID: "w1:pSingle", TabID: "t2", WorkspaceID: "w1", AgentStatus: "idle"},
		},
		Tabs: []tabRec{
			{TabID: "t1", Label: "multi-tab"},
			{TabID: "t2", Label: "single-tab"},
		},
		Layouts: []layoutRec{
			{
				TabID: "t1",
				Panes: []layoutPane{
					{PaneID: "w1:pMain", Rect: rect{X: 0, Y: 0, Width: 80, Height: 40}},
					{PaneID: "w1:pSub1", Rect: rect{X: 80, Y: 0, Width: 80, Height: 20}},
					{PaneID: "w1:pSub2", Rect: rect{X: 80, Y: 20, Width: 80, Height: 20}},
				},
			},
		},
	}

	all := collectRows(snap, st)
	if len(all) != 4 {
		t.Fatalf("expected 4 collected rows, got %d", len(all))
	}

	// Main pane aggregates subpane status (working > idle)
	var mainRow row
	for _, r := range all {
		if r.PaneID == "w1:pMain" {
			mainRow = r
			break
		}
	}
	if mainRow.SubpaneCount != 2 {
		t.Fatalf("expected 2 subpanes on main, got %d", mainRow.SubpaneCount)
	}
	if statusWord(mainRow.Status) != "working" {
		t.Fatalf("collapsed main should inherit working status from sub1, got %q", mainRow.Status)
	}

	// Collapsed by default: assembleTreeRows returns only root rows (multi-tab and single-tab)
	roots := []row{}
	for _, r := range all {
		if !r.IsSubpane {
			roots = append(roots, r)
		}
	}
	treeCollapsed := assembleTreeRows(roots, all, st, "")
	if len(treeCollapsed) != 2 {
		t.Fatalf("collapsed tree should show 2 rows, got %d", len(treeCollapsed))
	}
	if treeCollapsed[0].PaneID != "w1:pMain" || treeCollapsed[1].PaneID != "w1:pSingle" {
		t.Fatalf("unexpected collapsed rows: %+v", treeCollapsed)
	}
	if treeCollapsed[0].Expanded {
		t.Fatal("expected main row to be collapsed")
	}

	// Render frame check for collapsed: contains ▸ and badge
	frameCollapsed := formatLabelWithTree(treeCollapsed[0], "", 70, false, false)
	if !strings.Contains(frameCollapsed, "▸") || !strings.Contains(frameCollapsed, "2 subpanes") {
		t.Fatalf("collapsed label missing chevron or badge:\n%s", frameCollapsed)
	}

	// Expand the main pane
	st.toggleExpanded("w1:pMain")
	if !st.isExpanded("w1:pMain") {
		t.Fatal("expected main pane to be expanded")
	}

	treeExpanded := assembleTreeRows(roots, all, st, "")
	if len(treeExpanded) != 4 {
		t.Fatalf("expanded tree should show 4 rows (1 main + 2 subs + 1 single), got %d", len(treeExpanded))
	}
	if treeExpanded[0].PaneID != "w1:pMain" || treeExpanded[1].PaneID != "w1:pSub1" || treeExpanded[2].PaneID != "w1:pSub2" {
		t.Fatalf("unexpected expanded order: %+v", treeExpanded)
	}
	if treeExpanded[1].TreeBranch != "├─ " {
		t.Fatalf("expected ├─ on first subpane, got %q", treeExpanded[1].TreeBranch)
	}
	if treeExpanded[2].TreeBranch != "└─ " {
		t.Fatalf("expected └─ on last subpane, got %q", treeExpanded[2].TreeBranch)
	}

	// Render frame check for expanded main row: contains ▾
	frameExpanded := formatLabelWithTree(treeExpanded[0], "", 70, false, false)
	if !strings.Contains(frameExpanded, "▾") {
		t.Fatalf("expanded label missing ▾:\n%s", frameExpanded)
	}

	// Render frame check for subpane: contains branch
	frameSub := formatLabelWithTree(treeExpanded[1], "", 70, false, false)
	if !strings.Contains(frameSub, "├─") {
		t.Fatalf("subpane label missing ├─:\n%s", frameSub)
	}

	// Search auto-expansion: collapse st, but search for "Sub2"
	st.toggleExpanded("w1:pMain") // back to collapsed
	treeSearched := assembleTreeRows(roots, all, st, "Sub2")
	if len(treeSearched) != 4 {
		t.Fatalf("search matching subpane should auto-expand, got %d rows", len(treeSearched))
	}
	filtered := filterRows(treeSearched, "Sub2")
	if len(filtered) != 2 {
		t.Fatalf("filterRows should keep parent main and matching subpane, got %d rows", len(filtered))
	}
	if filtered[0].PaneID != "w1:pMain" || filtered[1].PaneID != "w1:pSub2" {
		t.Fatalf("unexpected filtered rows: %+v", filtered)
	}
}

func TestTreeSubpanesPTYNavigation(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "herdr-recall")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v: %s", err, out)
	}
	useTempConfig(t)
	cfg := os.Getenv("HERDR_PLUGIN_CONFIG_DIR")
	seed := mustLoad(t)
	seed.TabsInit = true
	seed.ActiveTabSet = true
	seed.TabsInit = true
	seed.Tabs = defaultTabs()
	seed.ActiveTabSet = true
	seed.ActiveTab = 5 // All tab (Fav=0, Inbox=1, Later=2, Recent=3, Most used=4, All=5)
	if err := saveStore(seed); err != nil {
		t.Fatal(err)
	}

	snapshot := `{"result":{"snapshot":{"focused_pane_id":"w1:pMain","focused_tab_id":"t1","focused_workspace_id":"w1","panes":[{"pane_id":"w1:pMain","tab_id":"t1","workspace_id":"w1","agent_status":"idle","terminal_title_stripped":"main","cwd":"/work","focused":true},{"pane_id":"w1:pSub1","tab_id":"t1","workspace_id":"w1","agent_status":"working","terminal_title_stripped":"worker1","cwd":"/work","focused":false}],"tabs":[{"tab_id":"t1","workspace_id":"w1","label":"multi-tab","focused":true}],"workspaces":[{"workspace_id":"w1","label":"Work","active_tab_id":"t1","focused":true}],"layouts":[{"tab_id":"t1","workspace_id":"w1","focused_pane_id":"w1:pMain","panes":[{"pane_id":"w1:pMain","rect":{"x":0,"y":0,"width":80,"height":40}},{"pane_id":"w1:pSub1","rect":{"x":80,"y":0,"width":80,"height":40}}]}]}}}`

	p := startPicker(t, bin, cfg, "HERDR_RECALL_TRANSCRIPTS=0", "PATH="+herdrShim(t, snapshot))
	frame := 1
	p.frames(frame)

	// Jump to All tab
	p.press("g")
	frame++
	p.frames(frame)
	p.press("a")
	frame++
	out := stripANSI(p.frames(frame))

	// Collapsed by default: shows ▸ and subpane badge
	if !strings.Contains(out, "▸") || !strings.Contains(out, "1 subpane") {
		t.Fatalf("expected collapsed multi-tab with badge in frame:\n%s", out)
	}
	if strings.Contains(out, "worker1") {
		t.Fatalf("subpane should be hidden when collapsed:\n%s", out)
	}

	// Press space to expand!
	p.press(" ")
	frame++
	out = stripANSI(lastPaintedFrame(p.frames(frame)))
	if !strings.Contains(out, "▾") {
		t.Fatalf("expected ▾ after space in frame:\n%s", out)
	}
	if !strings.Contains(out, "worker1") || !strings.Contains(out, "└─") {
		t.Fatalf("expected subpane visible with branch after space in frame:\n%s", out)
	}

	// Press space again to collapse!
	p.press(" ")
	frame++
	out = stripANSI(lastPaintedFrame(p.frames(frame)))
	if !strings.Contains(out, "▸") || strings.Contains(out, "worker1") {
		t.Fatalf("expected collapsed again after second space in frame:\n%s", out)
	}
}
