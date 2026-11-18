package runner

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/manishsongirkar/aegisloop/internal/config"
	"github.com/manishsongirkar/aegisloop/internal/tokenizer"
)

// RuleInfo documents one built-in standards rule.
type RuleInfo struct {
	ID   string
	Lang string
	Desc string
}

// Rules lists every built-in rule (used by `aegis rules` and the prompt).
func Rules() []RuleInfo {
	return []RuleInfo{
		{"tabs", "php/js/css", "Indent with tabs, not spaces (strict_tabs)"},
		{"short-arrays", "php", "Use [] instead of array() (strict_short_arrays)"},
		{"escape-output", "php", "echo/print/<?= must output escaped values (esc_html, esc_attr, esc_url, wp_kses_post, absint…)"},
		{"db-unprepared", "php", "$wpdb queries built from variables must use $wpdb->prepare()"},
		{"db-uncached", "php", "Direct $wpdb reads must be cached (wp_cache_get/set or transients)"},
		{"unsanitized-input", "php", "Superglobals ($_GET, $_POST…) must be sanitized/validated before use"},
		{"forbidden-function", "php", "eval, create_function, extract, unserialize, shell_exec, exec, system, passthru, popen, proc_open, query_posts, assert"},
		{"debug-leftover", "php/js", "var_dump/print_r/error_log/console.log/debugger left in code"},
		{"no-eval", "js", "eval() and new Function() are forbidden"},
		{"xss-innerhtml", "js", "innerHTML/outerHTML/insertAdjacentHTML/document.write/dangerouslySetInnerHTML are XSS sinks"},
		{"hardcoded-secret", "all", "API keys, private keys and passwords must not be committed"},
	}
}

// StandardsText returns the enforced standards as imperative sentences for the
// system prompt, honouring the project configuration.
func StandardsText(cfg *config.Config) []string {
	if !cfg.Guard.Enabled {
		return nil
	}
	off := map[string]bool{}
	for _, d := range cfg.Guard.Disable {
		off[d] = true
	}
	var out []string
	add := func(id, text string) {
		if !off[id] {
			out = append(out, text)
		}
	}
	if cfg.StrictTabs {
		add("tabs", "Indent with tabs, never spaces.")
	}
	if cfg.StrictShortArrays {
		add("short-arrays", "Use short array syntax [] instead of array().")
	}
	for _, r := range Rules() {
		switch r.ID {
		case "tabs", "short-arrays":
		default:
			add(r.ID, r.Desc+".")
		}
	}
	return out
}

var (
	goodEscaper = regexp.MustCompile(`^\\?(?:esc_(?:html|attr|url|url_raw|js|textarea|sql)(?:__|_e|_x)?|wp_kses(?:_post|_data|_allowed_html)?|wp_json_encode|absint|intval|floatval|number_format(?:_i18n)?|count|sizeof|selected|checked|disabled|readonly|wp_nonce_field|wp_create_nonce|wp_referer_field|get_the_ID|the_ID|settings_fields|submit_button|wp_editor)\s*\(`)
	constantRe  = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	numberRe    = regexp.MustCompile(`^[+-]?\d+(?:\.\d+)?$`)
	castOKRe    = regexp.MustCompile(`^\(\s*(?:int|integer|float|double|bool|boolean)\s*\)`)
	echoStmtRe  = regexp.MustCompile(`(?i)\b(?:echo|print)\b`)
	wpdbRe      = regexp.MustCompile(`\$[A-Za-z_>-]*wpdb\s*->\s*(query|get_results|get_row|get_var|get_col)\s*\(`)
	superRe     = regexp.MustCompile(`\$_(?:GET|POST|REQUEST|COOKIE|SERVER|FILES)\s*\[`)
	superOKRe   = regexp.MustCompile(`(?:sanitize_[a-z_]+|absint|intval|floatval|esc_[a-z_]+|wp_kses[a-z_]*|wp_verify_nonce|check_admin_referer|check_ajax_referer|filter_var|filter_input|isset|empty|in_array|array_key_exists)\s*\(|\(\s*(?:int|float|bool)\s*\)`)
	forbiddenRe = regexp.MustCompile(`\b(eval|create_function|extract|unserialize|shell_exec|exec|system|passthru|popen|proc_open|query_posts|assert)\s*\(`)
	phpDebugRe  = regexp.MustCompile(`\b(var_dump|print_r|var_export|error_log|debug_zval_dump|xdebug_[a-z_]+|dd|dump)\s*\(`)
	jsEvalRe    = regexp.MustCompile(`\beval\s*\(|\bnew\s+Function\s*\(`)
	jsXSSRe     = regexp.MustCompile(`\.(?:innerHTML|outerHTML)\s*\+?=[^=]|\binsertAdjacentHTML\s*\(|\bdangerouslySetInnerHTML\b|\bdocument\.write(?:ln)?\s*\(`)
	jsDebugRe   = regexp.MustCompile(`\bconsole\.(?:log|debug|trace)\s*\(|^\s*debugger\s*;?\s*$`)
	cacheRe     = regexp.MustCompile(`wp_cache_(?:get|set|add|delete)|(?:get|set)_(?:site_)?transient|wpcom_vip_|vip_cache|wp_cache`)
	secretRes   = []*regexp.Regexp{
		regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
		regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA |PGP )?PRIVATE KEY-----`),
		regexp.MustCompile(`(?i)(?:api[_-]?key|secret|passw(?:or)?d|token|auth)['"]?\s*(?:=>|[:=])\s*['"][A-Za-z0-9_\-+/=]{16,}['"]`),
		regexp.MustCompile(`\b(?:sk|pk|rk)_(?:live|test)_[A-Za-z0-9]{16,}`),
		regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{30,}`),
	}
	secretSkip = regexp.MustCompile(`(?i)example|placeholder|your[_-]|xxxx|changeme|process\.env|getenv|\$_ENV|<[a-z_ -]+>|\*{4}`)
)

// CheckStandards runs the built-in guard on the added/changed lines in scope.
func CheckStandards(dir string, scope Scope, cfg *config.Config) ([]tokenizer.Failure, error) {
	if !cfg.Guard.Enabled {
		return nil, nil
	}
	off := map[string]bool{}
	for _, d := range cfg.Guard.Disable {
		off[d] = true
	}
	var out []tokenizer.Failure
	for _, rel := range scope.Files {
		lang := tokenizer.LangOf(rel)
		if lang == tokenizer.LangText {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		g := &guardFile{rel: rel, src: string(data), lang: lang, scope: scope, cfg: cfg, off: off}
		g.run()
		out = append(out, g.out...)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].File != out[j].File {
			return out[i].File < out[j].File
		}
		return out[i].Line < out[j].Line
	})
	return out, nil
}

type guardFile struct {
	rel     string
	src     string
	lang    tokenizer.Lang
	scope   Scope
	cfg     *config.Config
	off     map[string]bool
	lines   []string
	masked  []byte
	mlines  []string
	prot    map[int]bool
	offsets []int // start offset of each line
	out     []tokenizer.Failure
}

func (g *guardFile) run() {
	g.lines = strings.Split(strings.ReplaceAll(g.src, "\r\n", "\n"), "\n")
	g.masked, g.prot = tokenizer.MaskWithStrings(g.src, g.lang)
	g.mlines = strings.Split(strings.ReplaceAll(string(g.masked), "\r\n", "\n"), "\n")
	g.offsets = []int{0}
	for i := 0; i < len(g.src); i++ {
		if g.src[i] == '\n' {
			g.offsets = append(g.offsets, i+1)
		}
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(g.rel), "."))
	isPHP := g.lang == tokenizer.LangPHP
	isJS := g.lang == tokenizer.LangJS

	if g.on("tabs") && g.cfg.StrictTabs && (isPHP || isJS || g.lang == tokenizer.LangCSS) {
		for i, l := range g.lines {
			ln := i + 1
			if !g.scope.Has(g.rel, ln) || g.prot[ln] || g.ignored(ln) {
				continue
			}
			ws := len(l) - len(strings.TrimLeft(l, " \t"))
			if strings.TrimSpace(l) != "" && strings.Contains(l[:ws], "    ") {
				g.add(ln, 1, "tabs", "Spaces used for indentation; use tabs")
			}
		}
	}
	if isPHP {
		if g.on("short-arrays") && g.cfg.StrictShortArrays {
			for _, a := range FindLongArrays(g.src) {
				if g.scope.Has(g.rel, a.Line) && !g.ignored(a.Line) {
					g.add(a.Line, 0, "short-arrays", "array() must be written as []")
				}
			}
		}
		if g.on("escape-output") {
			g.escapeOutput()
		}
		g.database()
		for i, ml := range g.mlines {
			ln := i + 1
			if !g.scope.Has(g.rel, ln) || g.ignored(ln) {
				continue
			}
			if g.on("unsanitized-input") && superRe.MatchString(ml) && !superOKRe.MatchString(ml) {
				g.add(ln, 0, "unsanitized-input", "Superglobal used without sanitization/validation (sanitize_text_field(wp_unslash(…)), absint(), isset())")
			}
			if g.on("forbidden-function") {
				for _, m := range forbiddenRe.FindAllStringSubmatchIndex(ml, -1) {
					if !precededByMember(ml, m[0]) {
						g.add(ln, m[0]+1, "forbidden-function", fmt.Sprintf("%s() is forbidden", ml[m[2]:m[3]]))
					}
				}
			}
			if g.on("debug-leftover") {
				for _, m := range phpDebugRe.FindAllStringSubmatchIndex(ml, -1) {
					if !precededByMember(ml, m[0]) && !strings.Contains(g.lines[i], ", true") && !strings.Contains(g.lines[i], ",true") {
						g.add(ln, m[0]+1, "debug-leftover", fmt.Sprintf("Debug call %s() left in code", ml[m[2]:m[3]]))
					}
				}
			}
		}
	}
	if isJS {
		for i, ml := range g.mlines {
			ln := i + 1
			if !g.scope.Has(g.rel, ln) || g.ignored(ln) {
				continue
			}
			if g.on("no-eval") && jsEvalRe.MatchString(ml) {
				g.add(ln, 0, "no-eval", "eval()/new Function() is forbidden")
			}
			if g.on("xss-innerhtml") && jsXSSRe.MatchString(ml) {
				g.add(ln, 0, "xss-innerhtml", "XSS sink: build DOM nodes / use textContent, or sanitize with DOMPurify/wp.sanitize")
			}
			if g.on("debug-leftover") && jsDebugRe.MatchString(ml) {
				g.add(ln, 0, "debug-leftover", "Debug statement left in code")
			}
		}
	}
	if g.on("hardcoded-secret") && ext != "json" {
		for i, l := range g.lines {
			ln := i + 1
			if !g.scope.Has(g.rel, ln) || g.ignored(ln) || secretSkip.MatchString(l) {
				continue
			}
			for _, re := range secretRes {
				if re.MatchString(l) {
					g.add(ln, 0, "hardcoded-secret", "Possible hard-coded secret; load it from the environment or a secrets manager")
					break
				}
			}
		}
	}
}

func (g *guardFile) on(id string) bool { return !g.off[id] }

func (g *guardFile) add(line, col int, rule, msg string) {
	g.out = append(g.out, tokenizer.Failure{Step: "standards", File: g.rel, Line: line, Col: col, Severity: "error", Rule: rule, Message: fmt.Sprintf("[%s] %s", rule, msg)})
}

func (g *guardFile) ignored(line int) bool {
	for _, ln := range []int{line, line - 1} {
		if ln >= 1 && ln <= len(g.lines) {
			l := g.lines[ln-1]
			if strings.Contains(l, "aegis:ignore") || strings.Contains(l, "phpcs:ignore") || strings.Contains(l, "phpcs:disable") {
				return true
			}
		}
	}
	return false
}

func (g *guardFile) lineOf(off int) int {
	return sort.SearchInts(g.offsets, off+1)
}

func precededByMember(ml string, idx int) bool {
	t := strings.TrimRight(ml[:idx], " \t")
	return strings.HasSuffix(t, "->") || strings.HasSuffix(t, "::") || strings.HasSuffix(t, "function") || strings.HasSuffix(t, "&")
}

// ---- escape-output -----------------------------------------------------------

func (g *guardFile) escapeOutput() {
	m := string(g.masked)
	type stmt struct{ start, end int }
	var stmts []stmt
	for _, loc := range echoStmtRe.FindAllStringIndex(m, -1) {
		if loc[0] > 0 && (isIdent(m[loc[0]-1]) || m[loc[0]-1] == '$' || m[loc[0]-1] == '>' && loc[0] > 1 && m[loc[0]-2] == '-') {
			continue
		}
		end := loc[1]
		for end < len(m) && m[end] != ';' && !(m[end] == '?' && end+1 < len(m) && m[end+1] == '>') {
			end++
		}
		stmts = append(stmts, stmt{loc[1], end})
	}
	for i := 0; i+2 < len(m); i++ {
		if m[i] == '<' && m[i+1] == '?' && m[i+2] == '=' {
			end := i + 3
			for end+1 < len(m) && !(m[end] == '?' && m[end+1] == '>') && m[end] != ';' {
				end++
			}
			stmts = append(stmts, stmt{i + 3, min(end, len(m))})
		}
	}
	for _, st := range stmts {
		line := g.lineOf(st.start)
		if !g.scope.Has(g.rel, line) || g.ignored(line) {
			continue
		}
		for _, op := range splitOperands(m[st.start:st.end]) {
			a, b := st.start+op[0], st.start+op[1]
			orig := strings.TrimSpace(g.src[a:b])
			mask := strings.TrimSpace(m[a:b])
			if !operandSafe(orig, mask) {
				short := orig
				if len(short) > 60 {
					short = short[:57] + "…"
				}
				g.add(g.lineOf(a+strings.Index(m[a:b], mask)), 0, "escape-output", fmt.Sprintf("Output not escaped: `%s` — wrap in esc_html()/esc_attr()/esc_url()/wp_kses_post()", strings.Join(strings.Fields(short), " ")))
			}
		}
	}
}

// splitOperands splits an echo expression on top-level "," and "." (string
// concatenation) and returns [start,end) offsets relative to expr.
func splitOperands(expr string) [][2]int {
	var out [][2]int
	depth, start := 0, 0
	for i := 0; i < len(expr); i++ {
		switch c := expr[i]; c {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, [2]int{start, i})
				start = i + 1
			}
		case '.':
			if depth != 0 {
				continue
			}
			digitBefore := i > 0 && expr[i-1] >= '0' && expr[i-1] <= '9'
			digitAfter := i+1 < len(expr) && expr[i+1] >= '0' && expr[i+1] <= '9'
			if (digitBefore && digitAfter) || (i+1 < len(expr) && expr[i+1] == '.') || (i > 0 && expr[i-1] == '.') {
				continue
			}
			out = append(out, [2]int{start, i})
			start = i + 1
		}
	}
	return append(out, [2]int{start, len(expr)})
}

func operandSafe(orig, mask string) bool {
	if mask == "" {
		return true
	}
	if strings.HasPrefix(mask, "'") {
		return true
	}
	if strings.HasPrefix(mask, `"`) {
		return !strings.Contains(orig, "$")
	}
	if strings.HasPrefix(mask, "<<<") {
		return !strings.Contains(orig, "$")
	}
	if numberRe.MatchString(mask) || constantRe.MatchString(mask) || castOKRe.MatchString(mask) {
		return true
	}
	for strings.HasPrefix(mask, "(") && matchingParen(mask, 0) == len(mask)-1 {
		mask = strings.TrimSpace(mask[1 : len(mask)-1])
		orig = strings.TrimSpace(orig[1 : len(orig)-1])
		return operandSafe(orig, mask)
	}
	if loc := goodEscaper.FindStringIndex(mask); loc != nil {
		return matchingParen(mask, loc[1]-1) == len(mask)-1
	}
	return false
}

func matchingParen(s string, open int) int {
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// ---- database ----------------------------------------------------------------

func (g *guardFile) database() {
	m := string(g.masked)
	for _, loc := range wpdbRe.FindAllStringSubmatchIndex(m, -1) {
		method := m[loc[2]:loc[3]]
		openIdx := loc[1] - 1
		closeIdx := matchingParen(m, openIdx)
		if closeIdx < 0 {
			closeIdx = len(m)
		}
		line := g.lineOf(loc[0])
		if !g.scope.Has(g.rel, line) || g.ignored(line) {
			continue
		}
		argsMask := m[openIdx+1 : closeIdx]
		argsOrig := g.src[openIdx+1 : closeIdx]
		first := splitOperandsComma(argsMask)
		firstOrig := strings.TrimSpace(argsOrig[:first])
		firstMask := strings.TrimSpace(argsMask[:first])

		if g.on("db-unprepared") {
			hasPrepare := strings.Contains(firstMask, "->prepare(") || strings.Contains(firstMask, "->prepare (")
			interpolated := strings.Contains(firstOrig, "$")
			bareVar := regexp.MustCompile(`^\$\w+$`).MatchString(firstMask)
			switch {
			case hasPrepare || !interpolated:
			case bareVar && strings.Contains(m[:loc[0]], "->prepare("):
			default:
				g.add(line, 0, "db-unprepared", "SQL built from variables without $wpdb->prepare() (SQL injection risk)")
			}
		}
		if g.on("db-uncached") && method != "query" {
			if !cacheRe.MatchString(g.enclosing(line)) {
				g.add(line, 0, "db-uncached", fmt.Sprintf("Direct database query $wpdb->%s() without caching: wrap in wp_cache_get()/wp_cache_set() (or a transient)", method))
			}
		}
	}
}

func splitOperandsComma(args string) int {
	depth := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case '(', '[', '{':
			depth++
		case ')', ']', '}':
			depth--
		case ',':
			if depth == 0 {
				return i
			}
		}
	}
	return len(args)
}

// enclosing returns the source of the function containing line (or ±20 lines).
func (g *guardFile) enclosing(line int) string {
	sl := tokenizer.SliceFile(g.rel, g.src, []int{line}, tokenizer.SliceOptions{ContextLines: 20, MaxLines: 100000})
	if len(sl) == 0 {
		return g.src
	}
	lo, hi := sl[0].Start, sl[0].End
	if hi > len(g.lines) {
		hi = len(g.lines)
	}
	return strings.Join(g.lines[lo-1:hi], "\n")
}
