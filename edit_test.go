package main

import "testing"

// typeKeys feeds a string of single characters plus named keys like <esc>.
func typeKeys(e *editor, seq string) editAction {
	last := actStay
	for _, k := range splitKeys(seq) {
		last = e.feed(k)
	}
	return last
}

func splitKeys(seq string) []string {
	var out []string
	for i := 0; i < len(seq); i++ {
		if seq[i] == '<' {
			if j := indexByte(seq[i:], '>'); j > 0 {
				out = append(out, seq[i+1:i+j])
				i += j
				continue
			}
		}
		out = append(out, string(seq[i]))
	}
	return out
}

func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func newNormal(text string) *editor {
	e := newEditor(text)
	e.mode = modeNormal
	e.cursor = 0
	return e
}

func want(t *testing.T, e *editor, text, where string) {
	t.Helper()
	if e.String() != text {
		t.Fatalf("%s: text = %q, want %q", where, e.String(), text)
	}
}

func TestMotions(t *testing.T) {
	e := newNormal("alpha beta gamma")
	typeKeys(e, "w")
	if e.cursor != 6 {
		t.Fatalf("w cursor = %d, want 6", e.cursor)
	}
	typeKeys(e, "e")
	if e.cursor != 9 {
		t.Fatalf("e cursor = %d, want 9", e.cursor)
	}
	typeKeys(e, "b")
	if e.cursor != 6 {
		t.Fatalf("b cursor = %d, want 6", e.cursor)
	}
	typeKeys(e, "$")
	if e.cursor != 16 {
		t.Fatalf("$ cursor = %d, want 16", e.cursor)
	}
	typeKeys(e, "0")
	if e.cursor != 0 {
		t.Fatalf("0 cursor = %d, want 0", e.cursor)
	}

	// f and t land on and before a character, and ; repeats the search.
	e = newNormal("one two two")
	typeKeys(e, "ft")
	if e.cursor != 4 {
		t.Fatalf("ft cursor = %d, want 4", e.cursor)
	}
	typeKeys(e, ";")
	if e.cursor != 8 {
		t.Fatalf("; cursor = %d, want 8", e.cursor)
	}
	e = newNormal("one two two")
	typeKeys(e, "tw")
	if e.cursor != 4 {
		t.Fatalf("tw cursor = %d, want 4", e.cursor)
	}
}

func TestOperatorsAndCounts(t *testing.T) {
	e := newNormal("alpha beta gamma")
	typeKeys(e, "dw")
	want(t, e, "beta gamma", "dw")

	e = newNormal("alpha beta gamma")
	typeKeys(e, "2dw")
	want(t, e, "gamma", "2dw")

	e = newNormal("alpha beta gamma")
	typeKeys(e, "cwX<esc>")
	if e.mode != modeNormal {
		t.Fatalf("cw left mode = %v, want normal", e.mode)
	}
	want(t, e, "X beta gamma", "cw")

	e = newNormal("abcd")
	e.cursor = 2
	typeKeys(e, "D")
	want(t, e, "ab", "D")

	e = newNormal("alpha beta")
	typeKeys(e, "0dd")
	want(t, e, "", "dd")
}

func TestCharsReplaceAndCase(t *testing.T) {
	e := newNormal("cat")
	typeKeys(e, "x")
	want(t, e, "at", "x")

	e = newNormal("cat")
	typeKeys(e, "llX")
	want(t, e, "ct", "X")

	e = newNormal("cat")
	typeKeys(e, "rz")
	want(t, e, "zat", "rz")

	e = newNormal("abc")
	typeKeys(e, "~")
	want(t, e, "Abc", "~")
}

func TestUndoRedoAndRepeat(t *testing.T) {
	e := newNormal("alpha beta")
	typeKeys(e, "dw")
	want(t, e, "beta", "dw")
	typeKeys(e, "u")
	want(t, e, "alpha beta", "u")
	typeKeys(e, "<ctrl-r>")
	want(t, e, "beta", "ctrl-r")

	e = newNormal("aaa")
	typeKeys(e, "x")
	want(t, e, "aa", "x")
	typeKeys(e, ".")
	want(t, e, "a", ".")
}

func TestInsertEntryAndEscLadder(t *testing.T) {
	e := newNormal("abc")
	if a := typeKeys(e, "iX"); a != actStay {
		t.Fatalf("i action = %v", a)
	}
	want(t, e, "Xabc", "i")
	if e.mode != modeInsert {
		t.Fatalf("mode after i = %v", e.mode)
	}

	// esc leaves insert for normal, and only then leaves the field.
	if a := typeKeys(e, "<esc>"); a != actStay {
		t.Fatalf("esc from insert action = %v, want stay", a)
	}
	if e.mode != modeNormal {
		t.Fatalf("mode after esc = %v, want normal", e.mode)
	}
	if a := typeKeys(e, "<esc>"); a != actOut {
		t.Fatalf("esc from normal action = %v, want out", a)
	}
}

func TestNormalPassesResultsToCaller(t *testing.T) {
	e := newNormal("abc")
	for _, tc := range []struct {
		key  string
		want editAction
	}{
		{"j", actDown}, {"k", actUp}, {"down", actDown}, {"up", actUp},
		{"ctrl-n", actDown}, {"ctrl-p", actUp},
		{"g", actFirst}, {"G", actLast},
		{"ctrl-u", actPageUp}, {"ctrl-d", actPageDown},
		{"enter", actAccept}, {"ctrl-c", actCancel},
	} {
		if got := e.feed(tc.key); got != tc.want {
			t.Fatalf("key %q action = %v, want %v", tc.key, got, tc.want)
		}
	}
}

func TestInsertTypesLetters(t *testing.T) {
	e := newEditor("")
	typeKeys(e, "hello")
	if e.String() != "hello" {
		t.Fatalf("insert text = %q", e.String())
	}
	// j and k are ordinary characters while typing.
	typeKeys(e, "jk")
	if e.String() != "hellojk" {
		t.Fatalf("typed jk = %q", e.String())
	}
}

func TestJJExitsInsert(t *testing.T) {
	e := newEditor("")
	typeKeys(e, "ab")
	if a := typeKeys(e, "j"); a != actStay {
		t.Fatalf("first j action = %v", a)
	}
	if e.String() != "ab" {
		t.Fatalf("held j leaked into text: %q", e.String())
	}
	if !e.PendingJ() {
		t.Fatal("j was not held")
	}
	if a := typeKeys(e, "j"); a != actStay {
		t.Fatalf("second j action = %v", a)
	}
	if e.mode != modeNormal {
		t.Fatalf("mode after jj = %v, want normal", e.mode)
	}
	if e.String() != "ab" {
		t.Fatalf("jj leaked into text: %q", e.String())
	}
}

func TestFlushJInsertsOneJ(t *testing.T) {
	e := newEditor("")
	typeKeys(e, "j")
	e.FlushJ()
	if e.String() != "j" || e.PendingJ() {
		t.Fatalf("flush produced %q, held %v", e.String(), e.PendingJ())
	}
}

func TestJThenOtherKeyInsertsBoth(t *testing.T) {
	e := newEditor("")
	typeKeys(e, "jk")
	if e.String() != "jk" {
		t.Fatalf("text = %q, want jk", e.String())
	}
	if e.mode != modeInsert {
		t.Fatalf("mode = %v, want insert", e.mode)
	}
}
