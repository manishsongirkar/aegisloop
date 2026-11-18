package tokenizer

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Failure is one located problem reported by a tool or the standards guard.
type Failure struct {
	Step     string `json:"step"`
	File     string `json:"file,omitempty"` // relative to the project root when possible
	Line     int    `json:"line,omitempty"`
	Col      int    `json:"col,omitempty"`
	Severity string `json:"severity,omitempty"` // error | warning
	Rule     string `json:"rule,omitempty"`
	Message  string `json:"message"`
}

func (f Failure) String() string {
	loc := f.File
	if f.Line > 0 {
		loc = fmt.Sprintf("%s:%d", f.File, f.Line)
	}
	if loc == "" {
		return fmt.Sprintf("[%s] %s", f.Step, f.Message)
	}
	return fmt.Sprintf("[%s] %s — %s", f.Step, loc, f.Message)
}

const codeExt = `(?:php|phtml|inc|js|jsx|ts|tsx|mjs|cjs|vue|svelte|css|scss|sass|less|go|json|html|twig|py|rb|java|kt|rs|c|h|cpp|xml|ya?ml|md|txt|sh)`

var (
	reLoc      = regexp.MustCompile(`^\s*(?:vet:\s*)?(?P<file>[^\s:()]*\.` + codeExt + `):(?P<line>\d+):(?:(?P<col>\d+):)?\s*(?P<msg>\S.*)$`)
	reTSC      = regexp.MustCompile(`^(?P<file>[^\s()]+\.` + codeExt + `)\((?P<line>\d+),(?P<col>\d+)\):\s*(?P<msg>(?:error|warning)\s.*)$`)
	rePHPError = regexp.MustCompile(`(?i)^(?:PHP\s+)?(?P<sev>Parse error|Fatal error|Warning|Notice|Deprecated|Error)(?:: |:\s+)(?P<msg>.*?) in (?P<file>\S+?\.` + codeExt + `) on line (?P<line>\d+)`)
	reFrame    = regexp.MustCompile(`^#\d+\s+(?P<file>\S+\.php)\((?P<line>\d+)\):\s*(?P<msg>.*)$`)
	reBareLoc  = regexp.MustCompile(`^(?P<file>/?[^\s:]+\.php):(?P<line>\d+)$`)
	reJestAt   = regexp.MustCompile(`at (?:.* \()?(?P<file>[^\s()]+\.` + codeExt + `):(?P<line>\d+):(?P<col>\d+)\)?$`)
	reStylish  = regexp.MustCompile(`^\s+(?P<line>\d+):(?P<col>\d+)\s+(?P<sev>error|warning)\s+(?P<msg>.+?)\s{2,}(?P<rule>\S+)\s*$`)
	reFileHdr  = regexp.MustCompile(`^(?P<file>/?[^\s:]+\.` + codeExt + `)$`)
	rePHPUnit  = regexp.MustCompile(`^\d+\) (.+)$`)
	reJestHdr  = regexp.MustCompile(`^\s*●\s+(.+)$`)
	reRuleTail = regexp.MustCompile(`\s+[\[(](?P<rule>[A-Za-z0-9_./-]+(?:\.[A-Za-z0-9_.]+)+|[A-Za-z]+/[A-Za-z0-9_-]+)[\])]\s*$`)
	reSevMsg   = regexp.MustCompile(`^(?i)(error|warning)\b\s*[-:]?\s*(.*)$`)
)

// ExtractFailures parses tool output into located failures. root is stripped
// from absolute paths. Frames inside vendor/ and node_modules/ are ignored.
func ExtractFailures(step, output, root string) []Failure {
	output = StripANSI(strings.ReplaceAll(output, "\r\n", "\n"))
	var out []Failure
	seen := map[string]bool{}
	add := func(f Failure) {
		f.Step = step
		f.File = relPath(f.File, root)
		if isNoisePath(f.File) {
			return
		}
		f.Message = strings.TrimSpace(f.Message)
		if m := reSevMsg.FindStringSubmatch(f.Message); m != nil && f.Severity == "" {
			f.Severity = strings.ToLower(m[1])
			if strings.TrimSpace(m[2]) != "" {
				f.Message = strings.TrimSpace(m[2])
			}
		}
		if m := reRuleTail.FindStringSubmatch(f.Message); m != nil && f.Rule == "" {
			f.Rule = m[1]
		}
		if f.Severity == "" {
			f.Severity = "error"
		}
		key := fmt.Sprintf("%s|%d|%s", f.File, f.Line, f.Message)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, f)
	}

	var header, curFile string
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimRight(raw, " \t")
		if line == "" {
			continue
		}
		if m := rePHPUnit.FindStringSubmatch(line); m != nil {
			header = m[1]
			continue
		}
		if m := reJestHdr.FindStringSubmatch(line); m != nil {
			header = m[1]
			continue
		}
		switch {
		case matchTo(reTSC, line, func(g map[string]string) { add(failureFrom(g)) }):
		case matchTo(rePHPError, line, func(g map[string]string) {
			sev := "error"
			if s := strings.ToLower(g["sev"]); s == "warning" || s == "notice" || s == "deprecated" {
				sev = "warning"
			}
			add(Failure{File: g["file"], Line: atoi(g["line"]), Severity: sev, Message: g["sev"] + ": " + g["msg"]})
		}):
		case matchTo(reFrame, line, func(g map[string]string) {
			msg := header
			if msg == "" {
				msg = g["msg"]
			}
			add(Failure{File: g["file"], Line: atoi(g["line"]), Message: msg})
		}):
		case matchTo(reLoc, line, func(g map[string]string) { add(failureFrom(g)) }):
		case matchTo(reBareLoc, line, func(g map[string]string) {
			msg := header
			if msg == "" {
				msg = "test failure"
			}
			add(Failure{File: g["file"], Line: atoi(g["line"]), Message: msg})
		}):
		case matchTo(reJestAt, line, func(g map[string]string) {
			msg := header
			if msg == "" {
				msg = "test failure"
			}
			add(Failure{File: g["file"], Line: atoi(g["line"]), Col: atoi(g["col"]), Message: msg})
		}):
		case matchTo(reFileHdr, line, func(g map[string]string) { curFile = g["file"] }):
		case curFile != "" && matchTo(reStylish, line, func(g map[string]string) {
			add(Failure{File: curFile, Line: atoi(g["line"]), Col: atoi(g["col"]), Severity: g["sev"], Rule: g["rule"], Message: g["msg"]})
		}):
		}
	}
	return out
}

func failureFrom(g map[string]string) Failure {
	return Failure{File: g["file"], Line: atoi(g["line"]), Col: atoi(g["col"]), Message: g["msg"]}
}

func matchTo(re *regexp.Regexp, line string, fn func(map[string]string)) bool {
	m := re.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	g := map[string]string{}
	for i, n := range re.SubexpNames() {
		if n != "" {
			g[n] = m[i]
		}
	}
	fn(g)
	return true
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func relPath(p, root string) string {
	p = filepath.ToSlash(p)
	if root != "" {
		r := filepath.ToSlash(root)
		if strings.HasPrefix(p, r+"/") {
			return p[len(r)+1:]
		}
	}
	return strings.TrimPrefix(p, "./")
}

func isNoisePath(p string) bool {
	return strings.Contains(p, "/vendor/") || strings.HasPrefix(p, "vendor/") ||
		strings.Contains(p, "/node_modules/") || strings.HasPrefix(p, "node_modules/") ||
		strings.Contains(p, "/.aegis-sandbox/")
}

// Fingerprint is a stable hash of a failure set. Line numbers and digits in
// messages are ignored so that harmless line shifts do not look like progress.
func Fingerprint(fs []Failure, unlocated string) string {
	keys := make([]string, 0, len(fs))
	for _, f := range fs {
		keys = append(keys, f.Step+"|"+f.File+"|"+digitsRe.ReplaceAllString(f.Message, "#"))
	}
	sort.Strings(keys)
	h := sha1.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
	}
	if len(fs) == 0 {
		h.Write([]byte(digitsRe.ReplaceAllString(unlocated, "#")))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

var digitsRe = regexp.MustCompile(`\d+`)
