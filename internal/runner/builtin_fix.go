package runner

import (
	"strings"

	"aegisloop/internal/tokenizer"
)

// FixTabs converts leading runs of spaces to tabs (4 columns per tab) on the
// lines selected by in (nil = all lines). A remainder of 1–3 spaces is kept so
// docblock and alignment padding survive. It is idempotent.
//
// Lines that start inside a multi-line string, heredoc or template literal are
// left alone because their whitespace is data.
func FixTabs(src string, lang tokenizer.Lang, in func(line int) bool) string {
	var protected map[int]bool
	if lang != tokenizer.LangText {
		_, protected = tokenizer.MaskWithStrings(src, lang)
	}
	lines := strings.SplitAfter(src, "\n")
	for i, l := range lines {
		if (in != nil && !in(i+1)) || protected[i+1] {
			continue
		}
		body := strings.TrimRight(l, "\r\n")
		ending := l[len(body):]
		ws := len(body) - len(strings.TrimLeft(body, " \t"))
		if ws == 0 || !strings.Contains(body[:ws], " ") {
			continue
		}
		width := 0
		for _, c := range body[:ws] {
			if c == '\t' {
				width += 4 - width%4
			} else {
				width++
			}
		}
		lines[i] = strings.Repeat("\t", width/4) + strings.Repeat(" ", width%4) + body[ws:] + ending
	}
	return strings.Join(lines, "")
}

// LongArray marks a legacy array( … ) construct.
type LongArray struct {
	Start int // byte offset of the "array" keyword
	Open  int // byte offset of "("
	Close int // byte offset of the matching ")"
	Line  int // 1-based line of the keyword
}

// FindLongArrays locates array( … ) constructs in PHP source, ignoring
// strings, comments, inline HTML, casts, type hints and identifiers that merely
// contain the word (is_array, array_map, $this->array(), Foo::array()).
func FindLongArrays(src string) []LongArray {
	m := tokenizer.Mask(src, tokenizer.LangPHP)
	var out []LongArray
	n := len(m)
	line := 1
	for i := 0; i < n; i++ {
		if m[i] == '\n' {
			line++
			continue
		}
		if m[i] != 'a' && m[i] != 'A' || i+5 > n || !strings.EqualFold(string(m[i:i+5]), "array") {
			continue
		}
		if i > 0 && isIdent(m[i-1]) || i+5 < n && isIdent(m[i+5]) {
			continue
		}
		if p := prevIndex(m, i); p >= 0 {
			c := m[p]
			if c == '$' || c == '\\' || (c == '>' && p >= 1 && m[p-1] == '-') || (c == ':' && p >= 1 && m[p-1] == ':') {
				continue
			}
		}
		j := i + 5
		for j < n && (m[j] == ' ' || m[j] == '\t') {
			j++
		}
		if j >= n || m[j] != '(' {
			continue
		}
		depth := 0
		k := j
		for ; k < n; k++ {
			if m[k] == '(' {
				depth++
			} else if m[k] == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
		}
		if k >= n {
			continue
		}
		out = append(out, LongArray{Start: i, Open: j, Close: k, Line: line})
	}
	return out
}

func isIdent(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}

func prevIndex(m []byte, i int) int {
	j := i - 1
	for j >= 0 && (m[j] == ' ' || m[j] == '\t' || m[j] == '\n' || m[j] == '\r') {
		j--
	}
	return j
}

// FixShortArrays rewrites array( … ) to [ … ] for constructs whose keyword is
// on a selected line. Line count never changes.
func FixShortArrays(src string, in func(line int) bool) string {
	arrs := FindLongArrays(src)
	var sel []LongArray
	b := []byte(src)
	for _, a := range arrs {
		if in != nil && !in(a.Line) {
			continue
		}
		b[a.Open], b[a.Close] = '[', ']'
		sel = append(sel, a)
	}
	// Delete "array<spaces>" ranges back to front so earlier offsets stay valid.
	for i := len(sel) - 1; i >= 0; i-- {
		b = append(b[:sel[i].Start], b[sel[i].Open:]...)
	}
	return string(b)
}
