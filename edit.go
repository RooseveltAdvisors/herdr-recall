package main

import (
	"strconv"
	"strings"
	"unicode"
)

// edit.go is the shared vim-style line editor. The search prompt, the sticky
// note prompt, and the custom tab name field all drive this one type, so the
// editing feel is identical wherever text is entered.
//
// The key inventory follows GNU readline vi-editing-mode and zsh bindkey -v:
// motions, operators with counts, one unnamed register, undo and redo, and
// replace mode. j/k and the arrows move the result list, because a single
// line has no vertical motion to spend them on.

type editMode int

const (
	modeInsert editMode = iota
	modeNormal
	modeReplace
)

// editAction is what the caller must do with a key the editor did not keep.
type editAction int

const (
	actStay editAction = iota
	actAccept
	actOut
	actCancel
	actUp
	actDown
	actFirst
	actLast
	actPageUp
	actPageDown
)

type editSnap struct {
	text   []rune
	cursor int
}

type editor struct {
	text   []rune
	cursor int
	mode   editMode

	pending   rune // 'd', 'c', 'y', 'r', 'f', 'F', 't', 'T'
	pendingOp rune // operator held while a find motion waits for its char
	count     int  // numeric prefix

	reg []rune

	undo []editSnap
	redo []editSnap

	findCh  rune
	findDir int

	// change tracks one insert session so '.' can replay it.
	inChange    bool
	changeM     rune
	changeCount int
	changeFrom  int

	lastEdit func(*editor)
}

func newEditor(s string) *editor {
	rs := []rune(s)
	return &editor{text: rs, cursor: len(rs), mode: modeInsert}
}

func (e *editor) String() string { return string(e.text) }

func (e *editor) pushUndo() {
	e.undo = append(e.undo, editSnap{text: append([]rune(nil), e.text...), cursor: e.cursor})
	e.redo = e.redo[:0]
}

func (e *editor) takeCount() int {
	c := e.count
	if c <= 0 {
		c = 1
	}
	e.count = 0
	return c
}

func (e *editor) clamp() {
	if e.cursor > len(e.text) {
		e.cursor = len(e.text)
	}
	if e.cursor < 0 {
		e.cursor = 0
	}
}

// ---------------------------------------------------------------------------
// motions
// ---------------------------------------------------------------------------

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func (e *editor) wordClass(r rune, big bool) int {
	if r == ' ' {
		return 0
	}
	if big {
		return 1
	}
	if isWordRune(r) {
		return 1
	}
	return 2
}

func (e *editor) wordForward(p int, big bool) int {
	n := len(e.text)
	if p >= n {
		return n
	}
	start := e.wordClass(e.text[p], big)
	i := p
	for i < n && e.wordClass(e.text[i], big) == start {
		i++
	}
	for i < n && e.text[i] == ' ' {
		i++
	}
	return i
}

func (e *editor) wordBack(p int, big bool) int {
	if p <= 0 {
		return 0
	}
	i := p - 1
	for i > 0 && e.text[i] == ' ' {
		i--
	}
	cls := e.wordClass(e.text[i], big)
	for i > 0 && e.wordClass(e.text[i-1], big) == cls {
		i--
	}
	return i
}

func (e *editor) wordEnd(p int, big bool) int {
	n := len(e.text)
	if n == 0 {
		return 0
	}
	i := p + 1
	for i < n && e.text[i] == ' ' {
		i++
	}
	if i >= n {
		return n - 1
	}
	cls := e.wordClass(e.text[i], big)
	for i+1 < n && e.wordClass(e.text[i+1], big) == cls {
		i++
	}
	return i
}

// findChar runs f, F, t or T. findCh must already be set.
func (e *editor) findChar(m rune, count int) (int, bool) {
	n := len(e.text)
	if e.findCh == 0 {
		return e.cursor, false
	}
	step := 1
	if m == 'F' || m == 'T' {
		step = -1
	}
	p := e.cursor
	for i := 0; i < count; i++ {
		q := p + step
		for q >= 0 && q < n && e.text[q] != e.findCh {
			q += step
		}
		if q < 0 || q >= n {
			return e.cursor, false
		}
		p = q
	}
	if m == 't' {
		p--
	}
	if m == 'T' {
		p++
	}
	if p < 0 || p >= n {
		return e.cursor, false
	}
	return p, true
}

// move applies one motion and returns the target cursor. ok is false when a
// character search finds nothing, which in vim aborts the command.
func (e *editor) move(m rune, count int) (int, bool) {
	if count < 1 {
		count = 1
	}
	p := e.cursor
	n := len(e.text)
	switch m {
	case 'h':
		p -= count
		if p < 0 {
			p = 0
		}
	case 'l':
		p += count
		if p > n {
			p = n
		}
	case '0':
		p = 0
	case '^':
		p = 0
		for p < n && e.text[p] == ' ' {
			p++
		}
	case '$':
		p = n
	case 'w', 'W':
		for i := 0; i < count; i++ {
			p = e.wordForward(p, m == 'W')
		}
	case 'b', 'B':
		for i := 0; i < count; i++ {
			p = e.wordBack(p, m == 'B')
		}
	case 'e', 'E':
		for i := 0; i < count; i++ {
			p = e.wordEnd(p, m == 'E')
		}
	case 'f', 'F', 't', 'T':
		return e.findChar(m, count)
	case ';':
		if e.findCh == 0 {
			return p, false
		}
		if e.findDir < 0 {
			return e.findChar('F', count)
		}
		return e.findChar('f', count)
	case ',':
		if e.findCh == 0 {
			return p, false
		}
		if e.findDir < 0 {
			return e.findChar('f', count)
		}
		return e.findChar('F', count)
	}
	return p, true
}

// rangeFor maps an operator motion onto a half-open [start,end) span.
func (e *editor) rangeFor(m rune, count int) (int, int, bool) {
	start := e.cursor
	target, ok := e.move(m, count)
	if !ok {
		return 0, 0, false
	}
	switch m {
	case 'b', 'B', '0', '^':
		return target, e.cursor, true
	case '$':
		return e.cursor, len(e.text), true
	}
	if target < start {
		return target, start, true
	}
	// Forward motions are inclusive of the target character, except w and W
	// which stop at the start of the next word.
	if m == 'w' || m == 'W' {
		return start, target, true
	}
	end := target + 1
	if end > len(e.text) {
		end = len(e.text)
	}
	return start, end, true
}

// ---------------------------------------------------------------------------
// edits
// ---------------------------------------------------------------------------

func (e *editor) insertRunes(rs []rune) {
	e.pushUndo()
	out := make([]rune, 0, len(e.text)+len(rs))
	out = append(out, e.text[:e.cursor]...)
	out = append(out, rs...)
	out = append(out, e.text[e.cursor:]...)
	e.text = out
	e.cursor += len(rs)
	e.clamp()
}

func (e *editor) deleteRange(a, b int) {
	if a > b {
		a, b = b, a
	}
	if a < 0 {
		a = 0
	}
	if b > len(e.text) {
		b = len(e.text)
	}
	if a == b {
		return
	}
	e.pushUndo()
	e.reg = append([]rune(nil), e.text[a:b]...)
	out := make([]rune, 0, len(e.text)-(b-a))
	out = append(out, e.text[:a]...)
	out = append(out, e.text[b:]...)
	e.text = out
	e.cursor = a
	e.clamp()
}

// beginChange arms '.' capture for one insert session.
func (e *editor) beginChange(m rune, count int) {
	e.inChange = true
	e.changeM = m
	e.changeCount = count
	e.changeFrom = e.cursor
}

// endChange records the finished insert session as the '.' repeat target.
func (e *editor) endChange() {
	if !e.inChange {
		return
	}
	e.inChange = false
	kind := e.changeM
	count := e.changeCount
	ins := append([]rune(nil), e.text[e.changeFrom:e.cursor]...)
	e.lastEdit = func(ed *editor) {
		switch kind {
		case 'i', 'I':
			ed.insertRunes(ins)
		case 'a', 'A':
			if ed.cursor < len(ed.text) {
				ed.cursor++
			}
			ed.insertRunes(ins)
		case 'C', '$':
			ed.deleteRange(ed.cursor, len(ed.text))
			ed.insertRunes(ins)
		case 'L':
			ed.deleteRange(0, len(ed.text))
			ed.cursor = 0
			ed.insertRunes(ins)
		default:
			if s, en, ok := ed.rangeFor(kind, count); ok {
				ed.deleteRange(s, en)
				ed.insertRunes(ins)
			}
		}
	}
}

func (e *editor) operate(op, m rune, count int) {
	if m == op {
		switch op {
		case 'y':
			e.reg = append([]rune(nil), e.text...)
		case 'd':
			e.deleteRange(0, len(e.text))
			e.lastEdit = func(ed *editor) { ed.deleteRange(0, len(ed.text)) }
		case 'c':
			e.deleteRange(0, len(e.text))
			e.beginChange('L', 1)
			e.mode = modeInsert
		}
		return
	}
	// Vim special case: cw and cW change to the end of the word, not to the
	// start of the next one. They behave exactly like ce and cE.
	if op == 'c' {
		if m == 'w' {
			m = 'e'
		} else if m == 'W' {
			m = 'E'
		}
	}
	start, end, ok := e.rangeFor(m, count)
	if !ok {
		return
	}
	switch op {
	case 'y':
		e.reg = append([]rune(nil), e.text[start:end]...)
	case 'd':
		e.deleteRange(start, end)
		mc := m
		cc := count
		e.lastEdit = func(ed *editor) {
			if s, en, ok := ed.rangeFor(mc, cc); ok {
				ed.deleteRange(s, en)
			}
		}
	case 'c':
		e.deleteRange(start, end)
		e.beginChange(m, count)
		e.mode = modeInsert
	}
}

func (e *editor) paste(before bool) {
	if len(e.reg) == 0 {
		return
	}
	e.pushUndo()
	at := e.cursor
	if !before && at < len(e.text) {
		at++
	}
	out := make([]rune, 0, len(e.text)+len(e.reg))
	out = append(out, e.text[:at]...)
	out = append(out, e.reg...)
	out = append(out, e.text[at:]...)
	e.text = out
	e.cursor = at + len(e.reg) - 1
	e.clamp()
	e.lastEdit = func(ed *editor) { ed.paste(before) }
}

func (e *editor) deleteChar(before bool) {
	if before {
		if e.cursor == 0 {
			return
		}
		e.deleteRange(e.cursor-1, e.cursor)
		return
	}
	if e.cursor >= len(e.text) {
		return
	}
	e.deleteRange(e.cursor, e.cursor+1)
}

func (e *editor) replaceChar(r rune) {
	if e.cursor >= len(e.text) {
		return
	}
	e.pushUndo()
	e.text[e.cursor] = r
}

func (e *editor) toggleCase() {
	if e.cursor >= len(e.text) {
		return
	}
	e.pushUndo()
	r := e.text[e.cursor]
	if unicode.IsUpper(r) {
		r = unicode.ToLower(r)
	} else {
		r = unicode.ToUpper(r)
	}
	e.text[e.cursor] = r
	e.cursor++
	e.clamp()
}

func (e *editor) undoEdit() {
	if len(e.undo) == 0 {
		return
	}
	snap := e.undo[len(e.undo)-1]
	e.undo = e.undo[:len(e.undo)-1]
	e.redo = append(e.redo, editSnap{text: append([]rune(nil), e.text...), cursor: e.cursor})
	e.text = snap.text
	e.cursor = snap.cursor
	e.clamp()
}

func (e *editor) redoEdit() {
	if len(e.redo) == 0 {
		return
	}
	snap := e.redo[len(e.redo)-1]
	e.redo = e.redo[:len(e.redo)-1]
	e.undo = append(e.undo, editSnap{text: append([]rune(nil), e.text...), cursor: e.cursor})
	e.text = snap.text
	e.cursor = snap.cursor
	e.clamp()
}

// ---------------------------------------------------------------------------
// feed
// ---------------------------------------------------------------------------

func (e *editor) printable(key string) (rune, bool) {
	if len(key) != 1 {
		return 0, false
	}
	r := rune(key[0])
	if r < 0x20 || r >= 0x7f {
		return 0, false
	}
	return r, true
}

func (e *editor) feed(key string) editAction {
	// A pending r, f, F, t or T is waiting for one literal character.
	if e.pending == 'r' {
		e.pending = 0
		if r, ok := e.printable(key); ok {
			e.replaceChar(r)
			rr := r
			e.lastEdit = func(ed *editor) { ed.replaceChar(rr) }
		}
		return actStay
	}
	if e.pending == 'f' || e.pending == 'F' || e.pending == 't' || e.pending == 'T' {
		m := e.pending
		e.pending = 0
		if r, ok := e.printable(key); ok {
			e.findCh = r
			if m == 'f' || m == 't' {
				e.findDir = 1
			} else {
				e.findDir = -1
			}
			if p, ok := e.move(m, e.takeCount()); ok {
				e.cursor = p
			}
		}
		return actStay
	}

	// An operator is waiting for its motion.
	if e.pending == 'd' || e.pending == 'c' || e.pending == 'y' {
		op := e.pending
		e.pending = 0
		if key == "esc" {
			return actStay
		}
		if key == string(op) {
			e.operate(op, op, 1)
			return actStay
		}
		if m, ok := e.printable(key); ok {
			if m == 'f' || m == 'F' || m == 't' || m == 'T' {
				e.pending = m
				e.pendingOp = op
				return actStay
			}
			e.operate(op, m, e.takeCount())
		}
		return actStay
	}

	// Numeric prefix. 0 is a motion unless a count is already running.
	if e.mode == modeNormal {
		if r, ok := e.printable(key); ok {
			if (r >= '1' && r <= '9') || (r == '0' && e.count > 0) {
				e.count = e.count*10 + int(r-'0')
				return actStay
			}
		}
	}

	switch e.mode {
	case modeInsert, modeReplace:
		return e.feedInsert(key)
	default:
		return e.feedNormal(key)
	}
}

func (e *editor) feedInsert(key string) editAction {
	switch key {
	case "esc":
		e.endChange()
		e.mode = modeNormal
		if e.cursor > 0 {
			e.cursor--
		}
		e.pending = 0
		e.clamp()
		return actStay
	case "enter":
		return actAccept
	case "ctrl-c":
		return actCancel
	case "bs":
		e.deleteChar(true)
		return actStay
	case "ctrl-u":
		if e.cursor > 0 {
			e.deleteRange(0, e.cursor)
		}
		return actStay
	case "ctrl-w":
		p, _ := e.move('b', 1)
		if p < e.cursor {
			e.deleteRange(p, e.cursor)
		}
		return actStay
	case "ctrl-k":
		if e.cursor < len(e.text) {
			e.deleteRange(e.cursor, len(e.text))
		}
		return actStay
	case "ctrl-y":
		e.insertRunes(e.reg)
		return actStay
	case "ctrl-d":
		e.deleteChar(false)
		return actStay
	case "ctrl-n", "down":
		return actDown
	case "ctrl-p", "up":
		return actUp
	case "left":
		if e.cursor > 0 {
			e.cursor--
		}
		return actStay
	case "right":
		if e.cursor < len(e.text) {
			e.cursor++
		}
		return actStay
	}
	if r, ok := e.printable(key); ok {
		if e.mode == modeReplace && e.cursor < len(e.text) {
			e.pushUndo()
			e.text[e.cursor] = r
			e.cursor++
			e.clamp()
			return actStay
		}
		e.insertRunes([]rune{r})
	}
	return actStay
}

func (e *editor) feedNormal(key string) editAction {
	if r, ok := e.printable(key); ok {
		switch r {
		case 'h':
			p, _ := e.move('h', e.takeCount())
			e.cursor = p
			return actStay
		case 'l':
			p, _ := e.move('l', e.takeCount())
			e.cursor = p
			return actStay
		case 'w', 'W', 'b', 'B', 'e', 'E', '0', '^', '$':
			p, _ := e.move(r, e.takeCount())
			e.cursor = p
			return actStay
		case 'f', 'F', 't', 'T':
			e.pending = r
			return actStay
		case ';', ',':
			if p, ok := e.move(r, e.takeCount()); ok {
				e.cursor = p
			}
			return actStay
		case 'i':
			e.mode = modeInsert
			e.beginChange('i', 1)
			return actStay
		case 'I':
			e.mode = modeInsert
			e.cursor, _ = e.move('^', 1)
			e.beginChange('I', 1)
			return actStay
		case 'a':
			e.mode = modeInsert
			if e.cursor < len(e.text) {
				e.cursor++
			}
			e.beginChange('a', 1)
			return actStay
		case 'A':
			e.mode = modeInsert
			e.cursor = len(e.text)
			e.beginChange('A', 1)
			return actStay
		case 'R':
			e.mode = modeReplace
			return actStay
		case 'x':
			n := e.takeCount()
			for i := 0; i < n; i++ {
				e.deleteChar(false)
			}
			e.lastEdit = func(ed *editor) {
				for i := 0; i < n; i++ {
					ed.deleteChar(false)
				}
			}
			return actStay
		case 'X':
			n := e.takeCount()
			for i := 0; i < n; i++ {
				e.deleteChar(true)
			}
			e.lastEdit = func(ed *editor) {
				for i := 0; i < n; i++ {
					ed.deleteChar(true)
				}
			}
			return actStay
		case 'r':
			e.pending = 'r'
			return actStay
		case 's':
			n := e.takeCount()
			end := e.cursor + n
			if end > len(e.text) {
				end = len(e.text)
			}
			e.deleteRange(e.cursor, end)
			e.beginChange('s', n)
			e.mode = modeInsert
			return actStay
		case 'd', 'c', 'y':
			e.pending = r
			return actStay
		case 'D':
			e.deleteRange(e.cursor, len(e.text))
			e.lastEdit = func(ed *editor) { ed.deleteRange(ed.cursor, len(ed.text)) }
			return actStay
		case 'C':
			e.deleteRange(e.cursor, len(e.text))
			e.beginChange('C', 1)
			e.mode = modeInsert
			return actStay
		case 'Y':
			e.reg = append([]rune(nil), e.text...)
			return actStay
		case 'p':
			e.paste(false)
			return actStay
		case 'P':
			e.paste(true)
			return actStay
		case 'u':
			e.undoEdit()
			return actStay
		case '~':
			n := e.takeCount()
			for i := 0; i < n; i++ {
				e.toggleCase()
			}
			e.lastEdit = func(ed *editor) {
				for i := 0; i < n; i++ {
					ed.toggleCase()
				}
			}
			return actStay
		case 'g':
			return actFirst
		case 'G':
			return actLast
		case '.':
			if e.lastEdit != nil {
				e.pushUndo()
				e.lastEdit(e)
			}
			return actStay
		}
	}

	switch key {
	case "esc":
		if e.pending != 0 {
			e.pending = 0
			e.pendingOp = 0
			e.count = 0
			return actStay
		}
		return actOut
	case "enter":
		return actAccept
	case "ctrl-c":
		return actCancel
	case "ctrl-r":
		e.redoEdit()
		return actStay
	case "ctrl-n", "down", "j":
		return actDown
	case "ctrl-p", "up", "k":
		return actUp
	case "ctrl-u":
		return actPageUp
	case "ctrl-d":
		return actPageDown
	case "left":
		p, _ := e.move('h', e.takeCount())
		e.cursor = p
		return actStay
	case "right":
		p, _ := e.move('l', e.takeCount())
		e.cursor = p
		return actStay
	}
	return actStay
}

// ---------------------------------------------------------------------------
// rendering
// ---------------------------------------------------------------------------

type fieldView struct {
	Label   string
	Text    string
	Cursor  int
	Mode    editMode
	Pending rune
	Count   int
	Focused bool
}

func (e *editor) view(label string, focused bool) fieldView {
	return fieldView{
		Label:   label,
		Text:    e.String(),
		Cursor:  e.cursor,
		Mode:    e.mode,
		Pending: e.pending,
		Count:   e.count,
		Focused: focused,
	}
}

// modeChip is the small badge left of the prompt. It carries the same
// information as vim's -- INSERT -- bar, in one bracketed cell.
func modeChip(m editMode, pending rune, count int) string {
	label := "N"
	color := keyAccentFg
	switch {
	case pending == 'd' || pending == 'c' || pending == 'y':
		label = strings.ToUpper(string(pending))
		color = keyStBad
	case pending == 'f' || pending == 'F' || pending == 't' || pending == 'T':
		label = string(pending)
		color = keyStBusy
	case pending == 'r':
		label = "r"
		color = keyStBusy
	case m == modeInsert:
		label = "I"
		color = keyStFree
	case m == modeReplace:
		label = "R"
		color = keyStBusy
	}
	if count > 0 {
		label += " " + strconv.Itoa(count)
	}
	return keySurface + "[" + keyReset + color + keyBold + label + keyReset + keySurface + "]" + keyReset
}

// renderField paints one editing line with a visible cursor cell. The real
// terminal cursor is hidden while the picker runs, so the painted cell is the
// only cursor and it cannot drift or wrap.
func renderField(fv fieldView, width int) string {
	if !fv.Focused {
		if fv.Text == "" {
			return keyOverlay + "  " + fv.Label + " " + keyReset + keyOverlay + "search panes" + keyReset
		}
		return keyOverlay + "  " + fv.Label + " " + keyReset + keyText + fv.Text + keyReset
	}
	var b strings.Builder
	b.WriteString(modeChip(fv.Mode, fv.Pending, fv.Count))
	b.WriteString(keyAccentFg + keyBold + "  " + fv.Label + " " + keyReset)
	runes := []rune(fv.Text)
	cur := fv.Cursor
	if cur > len(runes) {
		cur = len(runes)
	}
	for i := 0; i <= len(runes); i++ {
		ch := ' '
		if i < len(runes) {
			ch = runes[i]
		}
		if i != cur {
			b.WriteRune(ch)
			continue
		}
		switch fv.Mode {
		case modeNormal:
			b.WriteString(keySelectBg + keySelectFg + string(ch) + keyReset)
		case modeReplace:
			b.WriteString(keyStBusy + "\x1b[4m" + string(ch) + keyReset)
		default:
			b.WriteString(keyAccentFg + "\x1b[4m" + string(ch) + keyReset)
		}
	}
	if fv.Pending != 0 {
		b.WriteString(keyOverlay + "  " + string(fv.Pending) + keyReset)
	}
	return b.String()
}
