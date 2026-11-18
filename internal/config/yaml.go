package config

import (
	"fmt"
	"strconv"
	"strings"
)

// This file contains a deliberately small YAML-subset reader so that AegisLoop
// ships as a single dependency-free binary. It supports exactly what
// .aegis.yml needs: nested block maps, block lists (of scalars or maps),
// inline lists ([a, b]), quoted/unquoted scalars, booleans, integers and
// full-line / trailing comments.

type yline struct {
	indent int
	text   string
	no     int
}

// ParseYAML parses the supported YAML subset into map[string]any / []any / scalars.
func ParseYAML(src string) (map[string]any, error) {
	var lines []yline
	for i, raw := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if strings.Contains(raw, "\t") && strings.TrimLeft(raw, " ") != strings.TrimLeft(raw, " \t") {
			return nil, fmt.Errorf("line %d: tabs are not allowed for indentation in YAML", i+1)
		}
		text := strings.TrimRight(stripComment(raw), " \t")
		if strings.TrimSpace(text) == "" || strings.TrimSpace(text) == "---" {
			continue
		}
		indent := len(text) - len(strings.TrimLeft(text, " "))
		lines = append(lines, yline{indent: indent, text: strings.TrimLeft(text, " "), no: i + 1})
	}
	if len(lines) == 0 {
		return map[string]any{}, nil
	}
	idx := 0
	node, err := parseNode(lines, &idx, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if idx < len(lines) {
		return nil, fmt.Errorf("line %d: unexpected indentation", lines[idx].no)
	}
	m, ok := node.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("top-level YAML value must be a mapping")
	}
	return m, nil
}

func stripComment(s string) string {
	var quote rune
	for i, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
		case r == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t'):
			return s[:i]
		}
	}
	return s
}

func isListItem(t string) bool { return t == "-" || strings.HasPrefix(t, "- ") }

func parseNode(ls []yline, i *int, indent int) (any, error) {
	if isListItem(ls[*i].text) {
		return parseList(ls, i, indent)
	}
	return parseMap(ls, i, indent)
}

func splitKey(t string) (key, rest string, ok bool) {
	if len(t) > 0 && (t[0] == '"' || t[0] == '\'') {
		q := t[0]
		end := strings.IndexByte(t[1:], q)
		if end < 0 {
			return "", "", false
		}
		after := t[end+2:]
		if strings.HasPrefix(after, ":") && (len(after) == 1 || after[1] == ' ') {
			return t[1 : end+1], strings.TrimSpace(after[1:]), true
		}
		return "", "", false
	}
	var quote byte
	for i := 0; i < len(t); i++ {
		c := t[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ':' && (i == len(t)-1 || t[i+1] == ' '):
			return strings.TrimSpace(t[:i]), strings.TrimSpace(t[i+1:]), true
		}
	}
	return "", "", false
}

func parseMap(ls []yline, i *int, indent int) (any, error) {
	m := map[string]any{}
	for *i < len(ls) {
		l := ls[*i]
		if l.indent < indent {
			break
		}
		if l.indent > indent {
			return nil, fmt.Errorf("line %d: unexpected indentation", l.no)
		}
		if isListItem(l.text) {
			break
		}
		key, rest, ok := splitKey(l.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected 'key: value', got %q", l.no, l.text)
		}
		if _, dup := m[key]; dup {
			return nil, fmt.Errorf("line %d: duplicate key %q", l.no, key)
		}
		*i++
		if rest != "" {
			v, err := parseScalar(rest, l.no)
			if err != nil {
				return nil, err
			}
			m[key] = v
			continue
		}
		if *i < len(ls) && (ls[*i].indent > indent || (ls[*i].indent == indent && isListItem(ls[*i].text))) {
			child, err := parseNode(ls, i, ls[*i].indent)
			if err != nil {
				return nil, err
			}
			m[key] = child
		} else {
			m[key] = nil
		}
	}
	return m, nil
}

func parseList(ls []yline, i *int, indent int) (any, error) {
	var out []any
	for *i < len(ls) {
		l := ls[*i]
		if l.indent != indent || !isListItem(l.text) {
			break
		}
		rest := strings.TrimLeft(strings.TrimPrefix(l.text, "-"), " ")
		if rest == "" {
			*i++
			if *i < len(ls) && ls[*i].indent > indent {
				child, err := parseNode(ls, i, ls[*i].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, child)
			} else {
				out = append(out, nil)
			}
			continue
		}
		if _, _, isKV := splitKey(rest); isKV && !isQuotedScalar(rest) {
			childIndent := indent + (len(l.text) - len(rest))
			ls[*i] = yline{indent: childIndent, text: rest, no: l.no}
			child, err := parseMap(ls, i, childIndent)
			if err != nil {
				return nil, err
			}
			out = append(out, child)
			continue
		}
		v, err := parseScalar(rest, l.no)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		*i++
	}
	return out, nil
}

func isQuotedScalar(s string) bool {
	if len(s) < 2 || (s[0] != '"' && s[0] != '\'') {
		return false
	}
	end := strings.IndexByte(s[1:], s[0])
	return end == len(s)-2
}

func parseScalar(s string, no int) (any, error) {
	s = strings.TrimSpace(s)
	switch {
	case strings.HasPrefix(s, "["):
		if !strings.HasSuffix(s, "]") {
			return nil, fmt.Errorf("line %d: unterminated inline list", no)
		}
		inner := strings.TrimSpace(s[1 : len(s)-1])
		out := []any{}
		if inner == "" {
			return out, nil
		}
		for _, part := range splitInline(inner) {
			v, err := parseScalar(part, no)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case strings.HasPrefix(s, `"`):
		if !strings.HasSuffix(s, `"`) || len(s) < 2 {
			return nil, fmt.Errorf("line %d: unterminated string", no)
		}
		u, err := strconv.Unquote(s)
		if err != nil {
			return nil, fmt.Errorf("line %d: bad double-quoted string: %v", no, err)
		}
		return u, nil
	case strings.HasPrefix(s, "'"):
		if !strings.HasSuffix(s, "'") || len(s) < 2 {
			return nil, fmt.Errorf("line %d: unterminated string", no)
		}
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'"), nil
	case s == "true" || s == "True":
		return true, nil
	case s == "false" || s == "False":
		return false, nil
	case s == "null" || s == "~":
		return nil, nil
	}
	if n, err := strconv.Atoi(s); err == nil {
		return n, nil
	}
	return s, nil
}

func splitInline(s string) []string {
	var parts []string
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ',':
			parts = append(parts, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}
