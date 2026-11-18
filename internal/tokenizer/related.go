package tokenizer

import (
	"regexp"
	"strings"
)

var callRe = regexp.MustCompile(`([A-Za-z_$][\w$]*)\s*\(`)

// notCallee lists keywords and ubiquitous builtins that look like calls but never name project code.
var notCallee = map[string]bool{
	"if": true, "for": true, "foreach": true, "while": true, "switch": true, "catch": true, "elseif": true,
	"func": true, "function": true, "return": true, "defer": true, "go": true, "array": true, "isset": true,
	"empty": true, "echo": true, "print": true, "list": true, "match": true, "fn": true, "new": true,
	"len": true, "make": true, "append": true, "panic": true, "string": true, "int": true, "error": true,
	"describe": true, "it": true, "test": true, "expect": true, "require": true, "import": true,
}

// CalledNames returns, in first-seen order, the identifiers that are called in text.
func CalledNames(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range callRe.FindAllStringSubmatchIndex(text, -1) {
		n := text[m[2]:m[3]]
		if notCallee[n] || seen[n] || declaredHere(text[:m[2]]) {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// declaredHere reports whether the name that follows before is being declared
// (`func Name(`, `function name(`) rather than called.
func declaredHere(before string) bool {
	b := strings.TrimRight(before, " \t")
	return strings.HasSuffix(b, "func") || strings.HasSuffix(b, "function")
}

// RelatedSlices returns the functions of src whose declared name is one of
// names. It is how a failing test (or build error) is connected to the code
// under test: the caller passes the names called by the failing slice and the
// source of a file the patch changed. Blocks for which skip(start, end) is
// true (already shown) are left out. At most max slices are returned.
func RelatedSlices(path, src string, names []string, skip func(start, end int) bool, opts SliceOptions, max int) []Slice {
	opts.defaults()
	lines := splitLines(src)
	lang := LangOf(path)
	var blocks []block
	switch lang {
	case LangGo:
		blocks = goBlocks(src)
	case LangPHP, LangJS, LangCSS:
		blocks = braceBlocks(src, lang)
	}
	if len(lines) == 0 || len(blocks) == 0 || len(names) == 0 {
		return nil
	}
	var res []*regexp.Regexp
	for _, n := range names {
		res = append(res, regexp.MustCompile(`(?:^|[^\w$])`+regexp.QuoteMeta(n)+`\s*[(=]`))
	}
	var out []Slice
	for i := range blocks {
		b := blocks[i]
		if b.Kind != "function" || (skip != nil && skip(b.Start, b.End)) {
			continue
		}
		hit := false
		for _, re := range res {
			if re.MatchString(b.Signature) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		var s Slice
		if b.End-b.Start+1 <= opts.MaxLines {
			s = Slice{Path: path, Start: b.Start, End: b.End, Kind: b.Kind, Signature: b.Signature,
				Text: strings.Join(lines[b.Start-1:b.End], "\n")}
		} else {
			el := b.Open + 1
			if el > b.End {
				el = b.End
			}
			s = pickSlice(path, lines, blocks, el, opts, lang)
		}
		s.Related = true
		out = append(out, s)
		if len(out) >= max {
			break
		}
	}
	return out
}
