package healer

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LineEdit replaces an inclusive range of lines of a file.
type LineEdit struct {
	Path       string
	Start, End int
	Text       string
}

// SearchReplace replaces one occurrence of Search with Replace.
type SearchReplace struct {
	Path            string
	Search, Replace string
}

// Create writes a new file.
type Create struct {
	Path, Text string
}

// Reply is a parsed model reply.
type Reply struct {
	Lines    []LineEdit
	Searches []SearchReplace
	Creates  []Create
	Diff     string // unified diff, if the model answered with one
}

// Empty reports whether the reply contains nothing applicable.
func (r Reply) Empty() bool {
	return len(r.Lines) == 0 && len(r.Searches) == 0 && len(r.Creates) == 0 && strings.TrimSpace(r.Diff) == ""
}

var (
	gutterRe  = regexp.MustCompile(`^\s*\d+\| ?`)
	rangeRe   = regexp.MustCompile(`^(\d+)(?:-(\d+))?$`)
	diffHdrRe = regexp.MustCompile(`(?m)^(diff --git |--- (?:a/|/dev/null)\S*\n\+\+\+ )`)
)

// ParseReply extracts aegis-replace / aegis-create blocks, SEARCH/REPLACE
// blocks or a unified diff from a model reply (in that order of preference).
func ParseReply(resp string) Reply {
	resp = strings.ReplaceAll(resp, "\r\n", "\n")
	var r Reply
	lines := strings.Split(resp, "\n")
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		switch {
		case strings.HasPrefix(l, "```aegis-replace"):
			f := strings.Fields(strings.TrimPrefix(l, "```aegis-replace"))
			body, next := collectFence(lines, i+1)
			i = next
			if len(f) != 2 {
				continue
			}
			m := rangeRe.FindStringSubmatch(f[1])
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			end := start
			if m[2] != "" {
				end, _ = strconv.Atoi(m[2])
			}
			r.Lines = append(r.Lines, LineEdit{Path: cleanPath(f[0]), Start: start, End: end, Text: stripGutter(body)})
		case strings.HasPrefix(l, "```aegis-create"):
			f := strings.Fields(strings.TrimPrefix(l, "```aegis-create"))
			body, next := collectFence(lines, i+1)
			i = next
			if len(f) == 1 {
				r.Creates = append(r.Creates, Create{Path: cleanPath(f[0]), Text: stripGutter(body)})
			}
		}
	}
	if !r.Empty() {
		return r
	}

	// SEARCH/REPLACE blocks
	for i := 0; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) != "<<<<<<< SEARCH" {
			continue
		}
		path := ""
		for j := i - 1; j >= 0; j-- {
			t := strings.Trim(strings.TrimSpace(lines[j]), "`*: ")
			if t == "" || strings.HasPrefix(t, "```") {
				continue
			}
			path = cleanPath(t)
			break
		}
		var search, repl []string
		k := i + 1
		for ; k < len(lines) && strings.TrimSpace(lines[k]) != "======="; k++ {
			search = append(search, lines[k])
		}
		k++
		for ; k < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[k]), ">>>>>>> REPLACE"); k++ {
			repl = append(repl, lines[k])
		}
		if path != "" && k < len(lines) {
			r.Searches = append(r.Searches, SearchReplace{Path: path, Search: strings.Join(search, "\n"), Replace: strings.Join(repl, "\n")})
		}
		i = k
	}
	if !r.Empty() {
		return r
	}

	// unified diff
	if loc := diffHdrRe.FindStringIndex(resp); loc != nil {
		d := resp[loc[0]:]
		if end := strings.Index(d, "\n```"); end >= 0 {
			d = d[:end]
		}
		r.Diff = strings.TrimRight(d, "\n") + "\n"
	}
	return r
}

func collectFence(lines []string, from int) (body string, last int) {
	var b []string
	i := from
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "```" {
			break
		}
		b = append(b, lines[i])
	}
	return strings.Join(b, "\n"), i
}

func stripGutter(body string) string {
	ls := strings.Split(body, "\n")
	nonEmpty, gut := 0, 0
	for _, l := range ls {
		if strings.TrimSpace(l) == "" {
			continue
		}
		nonEmpty++
		if gutterRe.MatchString(l) {
			gut++
		}
	}
	if nonEmpty == 0 || gut != nonEmpty {
		return body
	}
	for i, l := range ls {
		ls[i] = gutterRe.ReplaceAllString(l, "")
	}
	return strings.Join(ls, "\n")
}

func cleanPath(p string) string {
	p = strings.Trim(p, "`'\" ")
	p = strings.TrimPrefix(filepath.ToSlash(p), "./")
	return p
}

// Range is an inclusive line range that edits may touch.
type Range struct{ Start, End int }

// Allowed maps file path → ranges that were shown to the model.
type Allowed map[string][]Range

func (a Allowed) covers(path string, start, end int) bool {
	for _, r := range a[path] {
		if start >= r.Start && end <= r.End {
			return true
		}
	}
	return false
}

// ApplyResult summarises ApplyReply.
type ApplyResult struct {
	Changed  []string
	Problems []string
	Applied  int
}

// ApplyReply applies line edits, search/replace blocks and creates to ws.
// Edits outside the ranges shown to the model are rejected. Unified diffs are
// not handled here (the caller passes them to Workspace.ApplyPatch).
func ApplyReply(ws Workspace, r Reply, allowed Allowed) ApplyResult {
	var res ApplyResult
	changed := map[string]bool{}
	problem := func(f string, a ...any) { res.Problems = append(res.Problems, fmt.Sprintf(f, a...)) }

	byFile := map[string][]LineEdit{}
	for _, e := range r.Lines {
		byFile[e.Path] = append(byFile[e.Path], e)
	}
	paths := make([]string, 0, len(byFile))
	for p := range byFile {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		edits := byFile[p]
		content, err := ws.Read(p)
		if err != nil {
			problem("%s: cannot read file (%v)", p, err)
			continue
		}
		crlf := strings.Contains(content, "\r\n")
		trailing := strings.HasSuffix(content, "\n")
		text := strings.ReplaceAll(content, "\r\n", "\n")
		lines := strings.Split(strings.TrimSuffix(text, "\n"), "\n")
		sort.SliceStable(edits, func(i, j int) bool { return edits[i].Start > edits[j].Start })
		lastStart := len(lines) + 2
		modified := false
		for _, e := range edits {
			switch {
			case e.Start < 1 || e.End < e.Start || e.End > len(lines):
				problem("%s:%d-%d: line range is outside the file (%d lines)", p, e.Start, e.End, len(lines))
				continue
			case !allowed.covers(p, e.Start, e.End):
				problem("%s:%d-%d: range was not part of the code shown to the model", p, e.Start, e.End)
				continue
			case e.End >= lastStart:
				problem("%s:%d-%d: overlaps another edit", p, e.Start, e.End)
				continue
			}
			var repl []string
			if e.Text != "" {
				repl = strings.Split(strings.TrimSuffix(e.Text, "\n"), "\n")
			}
			lines = append(lines[:e.Start-1], append(repl, lines[e.End:]...)...)
			lastStart = e.Start
			modified = true
			res.Applied++
		}
		if modified {
			out := strings.Join(lines, "\n")
			if trailing {
				out += "\n"
			}
			if crlf {
				out = strings.ReplaceAll(out, "\n", "\r\n")
			}
			if err := ws.Write(p, out); err != nil {
				problem("%s: write failed (%v)", p, err)
				res.Applied--
				continue
			}
			changed[p] = true
		}
	}

	for _, s := range r.Searches {
		content, err := ws.Read(s.Path)
		if err != nil {
			problem("%s: cannot read file (%v)", s.Path, err)
			continue
		}
		out, line, perr := searchReplace(content, s)
		if perr != nil {
			problem("%s: %v", s.Path, perr)
			continue
		}
		if !allowed.covers(s.Path, line, line) {
			problem("%s:%d: match is outside the code shown to the model", s.Path, line)
			continue
		}
		if err := ws.Write(s.Path, out); err != nil {
			problem("%s: write failed (%v)", s.Path, err)
			continue
		}
		changed[s.Path] = true
		res.Applied++
	}

	for _, c := range r.Creates {
		if _, err := ws.Read(c.Path); err == nil {
			problem("%s: file already exists; use aegis-replace to modify it", c.Path)
			continue
		}
		text := c.Text
		if !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if err := ws.Write(c.Path, text); err != nil {
			problem("%s: cannot create (%v)", c.Path, err)
			continue
		}
		changed[c.Path] = true
		res.Applied++
	}

	for p := range changed {
		res.Changed = append(res.Changed, p)
	}
	sort.Strings(res.Changed)
	return res
}

// searchReplace replaces the unique occurrence of s.Search. Exact match first;
// then a whitespace-insensitive line-by-line match. It returns the new content
// and the 1-based line where the match starts.
func searchReplace(content string, s SearchReplace) (string, int, error) {
	if strings.TrimSpace(s.Search) == "" {
		return "", 0, fmt.Errorf("empty SEARCH block")
	}
	if n := strings.Count(content, s.Search); n == 1 {
		idx := strings.Index(content, s.Search)
		return content[:idx] + s.Replace + content[idx+len(s.Search):], strings.Count(content[:idx], "\n") + 1, nil
	} else if n > 1 {
		return "", 0, fmt.Errorf("SEARCH block matches %d places; include more context", n)
	}
	crlf := strings.Contains(content, "\r\n")
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	cl := strings.Split(norm, "\n")
	sl := strings.Split(strings.Trim(s.Search, "\n"), "\n")
	match := -1
	for i := 0; i+len(sl) <= len(cl); i++ {
		ok := true
		for j := range sl {
			if normLine(cl[i+j]) != normLine(sl[j]) {
				ok = false
				break
			}
		}
		if ok {
			if match >= 0 {
				return "", 0, fmt.Errorf("SEARCH block matches several places; include more context")
			}
			match = i
		}
	}
	if match < 0 {
		return "", 0, fmt.Errorf("SEARCH block not found")
	}
	var repl []string
	if s.Replace != "" {
		repl = strings.Split(strings.Trim(s.Replace, "\n"), "\n")
	}
	out := append(append(append([]string{}, cl[:match]...), repl...), cl[match+len(sl):]...)
	res := strings.Join(out, "\n")
	if crlf {
		res = strings.ReplaceAll(res, "\n", "\r\n")
	}
	return res, match + 1, nil
}

// normLine collapses all whitespace so that indentation or spacing differences
// between the model's SEARCH text and the file do not prevent a match.
func normLine(s string) string { return strings.Join(strings.Fields(s), " ") }
