package tokenizer

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"regexp"
	"sort"
	"strings"
)

// Slice is the smallest region of a file that contains a failure: the
// enclosing function/class/rule (or a ±N-line window) with its signature.
type Slice struct {
	Path      string `json:"path"`
	Start     int    `json:"start"` // 1-based, inclusive
	End       int    `json:"end"`   // 1-based, inclusive
	Kind      string `json:"kind"`  // function | class | decl | rule | window | file
	Signature string `json:"signature,omitempty"`
	ErrLines  []int  `json:"error_lines,omitempty"`
	Text      string `json:"-"` // raw source of [Start, End]
	// Elided is set when the enclosing block was too long: Text then holds the
	// signature lines, a marker and a window around the error.
	Elided bool `json:"elided,omitempty"`
}

// Numbered renders the slice with file line numbers ("  12| code").
func (s Slice) Numbered() string {
	var b strings.Builder
	n := s.Start
	for _, l := range strings.Split(strings.TrimSuffix(s.Text, "\n"), "\n") {
		if strings.HasPrefix(l, elideMarker) {
			b.WriteString("     | " + l + "\n")
			continue
		}
		fmt.Fprintf(&b, "%5d| %s\n", n, l)
		n++
	}
	return b.String()
}

const elideMarker = "/* … aegis: lines omitted"

// SliceOptions tunes slicing.
type SliceOptions struct {
	ContextLines int // window radius when no enclosing block is found (default 10)
	MaxLines     int // longest block that is sent whole (default 120)
}

func (o *SliceOptions) defaults() {
	if o.ContextLines <= 0 {
		o.ContextLines = 10
	}
	if o.MaxLines <= 0 {
		o.MaxLines = 120
	}
}

// SliceFile returns merged slices of src covering errLines.
func SliceFile(path, src string, errLines []int, opts SliceOptions) []Slice {
	opts.defaults()
	lines := splitLines(src)
	if len(lines) == 0 {
		return nil
	}
	lang := LangOf(path)
	var blocks []block
	switch lang {
	case LangGo:
		blocks = goBlocks(src)
	case LangPHP, LangJS, LangCSS:
		blocks = braceBlocks(src, lang)
	}

	sort.Ints(errLines)
	var out []Slice
	for _, el := range uniqueInts(errLines) {
		if el < 1 {
			el = 1
		}
		if el > len(lines) {
			el = len(lines)
		}
		s := pickSlice(path, lines, blocks, el, opts, lang)
		out = append(out, s)
	}
	return mergeSlices(path, lines, out)
}

type block struct {
	Start, End int // header start line .. closing line (1-based)
	Open       int // line holding the opening brace
	Kind       string
	Signature  string
}

func pickSlice(path string, lines []string, blocks []block, el int, opts SliceOptions, lang Lang) Slice {
	var best *block
	for i := range blocks {
		b := &blocks[i]
		if el < b.Start || el > b.End {
			continue
		}
		if b.Kind == "control" {
			continue
		}
		if best == nil || (b.End-b.Start) < (best.End-best.Start) {
			// prefer the innermost function over an enclosing class
			best = b
		}
	}
	// If the innermost block is a class but a function exists inside it, the
	// innermost rule above already picked the function. If only a class matches
	// (e.g. a property line) fall back to a window plus the class signature.
	if best == nil || best.Kind == "class" {
		w := window(path, lines, el, opts.ContextLines)
		if best != nil {
			w.Signature = best.Signature
		}
		return w
	}
	size := best.End - best.Start + 1
	if size <= opts.MaxLines {
		return Slice{Path: path, Start: best.Start, End: best.End, Kind: best.Kind, Signature: best.Signature,
			ErrLines: []int{el}, Text: strings.Join(lines[best.Start-1:best.End], "\n")}
	}
	// Too long: keep the signature (header up to the opening line) + window.
	headEnd := best.Open
	if headEnd < best.Start {
		headEnd = best.Start
	}
	lo, hi := el-opts.ContextLines, el+opts.ContextLines
	if lo <= headEnd {
		lo = headEnd + 1
	}
	if hi >= best.End {
		hi = best.End - 1
	}
	if hi < lo {
		hi = lo
	}
	var parts []string
	parts = append(parts, lines[best.Start-1:headEnd]...)
	if lo > headEnd+1 {
		parts = append(parts, fmt.Sprintf("%s %d-%d */", elideMarker, headEnd+1, lo-1))
	}
	parts = append(parts, lines[lo-1:hi]...)
	if hi < best.End-1 {
		parts = append(parts, fmt.Sprintf("%s %d-%d */", elideMarker, hi+1, best.End-1))
	}
	parts = append(parts, lines[best.End-1])
	return Slice{Path: path, Start: best.Start, End: best.End, Kind: best.Kind, Signature: best.Signature,
		ErrLines: []int{el}, Text: strings.Join(parts, "\n"), Elided: true}
}

func window(path string, lines []string, el, radius int) Slice {
	lo, hi := el-radius, el+radius
	if lo < 1 {
		lo = 1
	}
	if hi > len(lines) {
		hi = len(lines)
	}
	return Slice{Path: path, Start: lo, End: hi, Kind: "window", ErrLines: []int{el}, Text: strings.Join(lines[lo-1:hi], "\n")}
}

func mergeSlices(path string, lines []string, in []Slice) []Slice {
	if len(in) <= 1 {
		return in
	}
	sort.SliceStable(in, func(i, j int) bool { return in[i].Start < in[j].Start })
	out := []Slice{in[0]}
	for _, s := range in[1:] {
		last := &out[len(out)-1]
		if s.Start <= last.End+1 {
			if last.Elided || s.Elided {
				// keep both separate when either is elided: merging would re-inflate
				if s.Start == last.Start && s.End == last.End {
					last.ErrLines = append(last.ErrLines, s.ErrLines...)
					continue
				}
				out = append(out, s)
				continue
			}
			if s.End > last.End {
				last.End = s.End
			}
			last.ErrLines = append(last.ErrLines, s.ErrLines...)
			if last.Kind == "window" && s.Kind != "window" {
				last.Kind, last.Signature = s.Kind, s.Signature
			}
			last.Text = strings.Join(lines[last.Start-1:last.End], "\n")
			continue
		}
		out = append(out, s)
	}
	return out
}

// ---- Go: real AST -----------------------------------------------------------

func goBlocks(src string) []block {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.ParseComments)
	if f == nil {
		return nil
	}
	_ = err // a partially parsed file is still useful; syntax errors are what we are fixing
	var out []block
	line := func(p token.Pos) int { return fset.Position(p).Line }
	for _, d := range f.Decls {
		switch t := d.(type) {
		case *ast.FuncDecl:
			start := line(t.Pos())
			if t.Doc != nil {
				start = line(t.Doc.Pos())
			}
			open := start
			if t.Body != nil {
				open = line(t.Body.Lbrace)
			}
			out = append(out, block{Start: start, End: line(t.End()), Open: open, Kind: "function", Signature: oneLine(srcLines(src, line(t.Pos()), open))})
		case *ast.GenDecl:
			start := line(t.Pos())
			if t.Doc != nil {
				start = line(t.Doc.Pos())
			}
			out = append(out, block{Start: start, End: line(t.End()), Open: line(t.Pos()), Kind: "decl", Signature: oneLine(srcLines(src, line(t.Pos()), line(t.Pos())))})
		}
	}
	return out
}

func srcLines(src string, from, to int) string {
	ls := splitLines(src)
	if from < 1 {
		from = 1
	}
	if to > len(ls) {
		to = len(ls)
	}
	if from > to {
		return ""
	}
	return strings.Join(ls[from-1:to], "\n")
}

// ---- PHP / JS / CSS: masked brace matching ---------------------------------

var (
	phpFunc    = regexp.MustCompile(`\bfunction\b`)
	phpClass   = regexp.MustCompile(`\b(?:class|interface|trait|enum)\s+\w+`)
	jsFunc     = regexp.MustCompile(`\bfunction\b|=>|^\s*(?:(?:public|private|protected|static|async|get|set|readonly)\s+)*\*?[\w$#]+\s*(?:<[^>]*>)?\s*\([^)]*\)\s*(?::\s*[^{]+)?$`)
	jsClass    = regexp.MustCompile(`\bclass\s+[\w$]+`)
	controlKw  = regexp.MustCompile(`^\s*(?:\}\s*)?(?:if|else|elseif|else\s+if|for|foreach|while|do|switch|try|catch|finally|with|match)\b`)
	docLineRe  = regexp.MustCompile(`^\s*(?:/\*\*?|\*|\*/|//|#)`)
	jsCtrlCall = regexp.MustCompile(`^\s*(?:if|for|while|switch|catch|with)\s*\(`)
)

func braceBlocks(src string, lang Lang) []block {
	masked := Mask(src, lang)
	lines := splitLines(src)
	mlines := splitLines(string(masked))
	type open struct{ line int }
	var stack []open
	var out []block
	for ln, ml := range mlines {
		for i := 0; i < len(ml); i++ {
			switch ml[i] {
			case '{':
				stack = append(stack, open{line: ln + 1})
			case '}':
				if len(stack) == 0 {
					continue
				}
				o := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				hdrStart, hdr := headerOf(lines, mlines, o.line, i2col(mlines, o.line))
				kind := classify(lang, hdr)
				if kind == "" {
					continue
				}
				start := hdrStart
				// pull in the doc comment above
				for start > 1 && docLineRe.MatchString(lines[start-2]) {
					start--
				}
				out = append(out, block{Start: start, End: ln + 1, Open: o.line, Kind: kind, Signature: oneLine(hdr)})
			}
		}
	}
	return out
}

func i2col(mlines []string, line int) int {
	if line-1 < len(mlines) {
		return strings.Index(mlines[line-1], "{")
	}
	return 0
}

// headerOf returns the first line and text of the statement that precedes the
// "{" on openLine (joining up to 6 previous lines of a wrapped signature).
func headerOf(lines, mlines []string, openLine, col int) (int, string) {
	if col < 0 {
		col = len(mlines[openLine-1])
	}
	first := mlines[openLine-1][:min(col, len(mlines[openLine-1]))]
	hdr := first
	start := openLine
	for strings.TrimSpace(hdr) == "" || !endsStatement(hdr) {
		if start <= 1 || openLine-start >= 6 {
			break
		}
		prev := strings.TrimSpace(mlines[start-2])
		if prev == "" || strings.HasSuffix(prev, ";") || strings.HasSuffix(prev, "{") || strings.HasSuffix(prev, "}") || strings.HasSuffix(prev, "*/") {
			break
		}
		start--
		hdr = mlines[start-1] + " " + hdr
		if strings.Contains(hdr, "(") && strings.Count(hdr, "(") == strings.Count(hdr, ")") {
			break
		}
	}
	// recover original text (unmasked) for the signature
	var orig []string
	for i := start; i <= openLine; i++ {
		l := lines[i-1]
		if i == openLine && col <= len(l) {
			l = l[:col]
		}
		orig = append(orig, strings.TrimSpace(l))
	}
	return start, strings.TrimSpace(strings.Join(orig, " "))
}

func endsStatement(h string) bool {
	t := strings.TrimSpace(h)
	return t != "" && (strings.Count(t, "(") == strings.Count(t, ")"))
}

func classify(lang Lang, hdr string) string {
	h := strings.TrimSpace(hdr)
	if h == "" {
		return "" // bare block / object literal
	}
	switch lang {
	case LangCSS:
		if strings.HasPrefix(h, "@media") || strings.HasPrefix(h, "@supports") {
			return "rule"
		}
		return "rule"
	case LangPHP:
		if controlKw.MatchString(h) {
			return "control"
		}
		if phpFunc.MatchString(h) {
			return "function"
		}
		if phpClass.MatchString(h) {
			return "class"
		}
		return "control"
	case LangJS:
		if controlKw.MatchString(h) || jsCtrlCall.MatchString(h) {
			return "control"
		}
		if jsClass.MatchString(h) {
			return "class"
		}
		if jsFunc.MatchString(h) {
			return "function"
		}
		return "control"
	}
	return ""
}

func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	s = strings.ReplaceAll(s, "\r\n", "\n")
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

func uniqueInts(in []int) []int {
	var out []int
	for i, v := range in {
		if i == 0 || v != in[i-1] {
			out = append(out, v)
		}
	}
	return out
}
