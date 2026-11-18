// Package tokenizer turns raw tool output and source files into the smallest
// payload an LLM needs to repair a failure: noise-stripped traces, parsed
// failures, AST/heuristic code slices and the prompt built from them.
package tokenizer

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Lang identifies how a file is scanned.
type Lang string

const (
	LangPHP  Lang = "php"
	LangGo   Lang = "go"
	LangJS   Lang = "js" // js, jsx, ts, tsx, mjs, cjs
	LangCSS  Lang = "css"
	LangText Lang = "text"
)

// LangOf maps a file name to a Lang.
func LangOf(path string) Lang {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) {
	case "php", "phtml", "inc":
		return LangPHP
	case "go":
		return LangGo
	case "js", "jsx", "ts", "tsx", "mjs", "cjs":
		return LangJS
	case "css", "scss", "less":
		return LangCSS
	}
	return LangText
}

// Mask returns a copy of src, byte-for-byte the same length, in which the
// contents of comments and string literals (and, for PHP, inline HTML and
// heredocs) are replaced by spaces. Newlines are preserved, so line and column
// positions map 1:1 onto src. String delimiters are kept, which lets callers
// still recognise "a string literal sits here".
func Mask(src string, lang Lang) []byte {
	out, _ := MaskWithStrings(src, lang)
	return out
}

// MaskWithStrings is Mask that also reports the 1-based numbers of lines that
// begin inside a multi-line string literal, heredoc or template literal. The
// whitespace on such lines is data, so formatters must not touch it.
func MaskWithStrings(src string, lang Lang) ([]byte, map[int]bool) {
	out := []byte(src)
	inString := map[int]bool{}
	lineAt := make([]int, 0, 64) // offsets of every '\n'
	for i := 0; i < len(src); i++ {
		if src[i] == '\n' {
			lineAt = append(lineAt, i)
		}
	}
	blankStr := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] == '\n' {
				k := sort.SearchInts(lineAt, i)
				inString[k+2] = true
			}
		}
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' && out[i] != '\r' {
				out[i] = ' '
			}
		}
	}
	blank := func(from, to int) {
		for i := from; i < to && i < len(out); i++ {
			if out[i] != '\n' && out[i] != '\r' {
				out[i] = ' '
			}
		}
	}
	n := len(src)
	i := 0
	inPHP := lang != LangPHP // for non-PHP everything is code
	for i < n {
		if lang == LangPHP && !inPHP {
			j := indexPHPOpen(src, i)
			if j < 0 {
				blank(i, n)
				return out, inString
			}
			blank(i, j)
			i = j
			if strings.HasPrefix(src[i:], "<?php") {
				i += 5
			} else if strings.HasPrefix(src[i:], "<?=") {
				i += 3
			} else {
				i += 2
			}
			inPHP = true
			continue
		}
		c := src[i]
		switch {
		case lang == LangPHP && c == '?' && i+1 < n && src[i+1] == '>':
			i += 2
			inPHP = false
		case c == '/' && i+1 < n && src[i+1] == '*':
			end := strings.Index(src[i+2:], "*/")
			stop := n
			if end >= 0 {
				stop = i + 2 + end + 2
			}
			blank(i, stop)
			i = stop
		case (c == '/' && i+1 < n && src[i+1] == '/') || (c == '#' && lang == LangPHP && !(i+1 < n && src[i+1] == '[')):
			j := i
			for j < n && src[j] != '\n' && !(lang == LangPHP && src[j] == '?' && j+1 < n && src[j+1] == '>') {
				j++
			}
			blank(i, j)
			i = j
		case c == '<' && lang == LangPHP && strings.HasPrefix(src[i:], "<<<"):
			if m := heredocRe.FindStringSubmatch(src[i:]); m != nil {
				bodyStart := i + len(m[0])
				endRe := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(m[1]) + `\b`)
				stop := n
				if loc := endRe.FindStringIndex(src[bodyStart:]); loc != nil {
					stop = bodyStart + loc[0]
				}
				blankStr(bodyStart-1, stop)
				i = stop
				continue
			}
			i++
		case c == '"' || c == '\'' || (c == '`' && lang != LangCSS && lang != LangGo) || (c == '`' && lang == LangGo):
			j := i + 1
			for j < n {
				if src[j] == '\\' && c != '`' || (src[j] == '\\' && lang == LangJS && c == '`') {
					j += 2
					continue
				}
				if src[j] == c {
					break
				}
				if src[j] == '\n' && c != '`' && lang != LangPHP {
					break // unterminated single-line string; stop at EOL
				}
				j++
			}
			if j > n {
				j = n
			}
			blankStr(i+1, j)
			i = j + 1
		default:
			i++
		}
	}
	return out, inString
}

var heredocRe = regexp.MustCompile(`^<<<[ \t]*['"]?([A-Za-z_][A-Za-z0-9_]*)['"]?\r?\n`)

func indexPHPOpen(src string, from int) int {
	for i := from; i+1 < len(src); i++ {
		if src[i] == '<' && src[i+1] == '?' {
			if strings.HasPrefix(src[i:], "<?php") || strings.HasPrefix(src[i:], "<?=") {
				return i
			}
		}
	}
	return -1
}
