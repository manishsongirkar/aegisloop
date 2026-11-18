// Package runner orchestrates the verification pipeline: zero-token local
// fixers first, then the validation gates (lint, tests, build, security).
package runner

import (
	"context"
	"crypto/sha1"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/manishsongirkar/aegisloop/internal/config"
	"github.com/manishsongirkar/aegisloop/internal/tokenizer"
)

// Scope is the set of files (and, per file, line numbers) AegisLoop is
// responsible for. A file mapped to a nil set — or absent from Lines — means
// "every line" (new files).
type Scope struct {
	Files []string
	Lines map[string]map[int]bool
}

// Has reports whether line of file is in scope.
func (s Scope) Has(file string, line int) bool {
	set, ok := s.Lines[file]
	if !ok || set == nil {
		return true
	}
	return set[line]
}

func (s Scope) lineFn(file string) func(int) bool {
	set, ok := s.Lines[file]
	if !ok || set == nil {
		return nil
	}
	return func(n int) bool { return set[n] }
}

// Step statuses.
const (
	StatusPassed  = "passed"
	StatusFailed  = "failed"
	StatusSkipped = "skipped"
	StatusFixed   = "fixed" // fixer modified files
	StatusClean   = "clean" // fixer changed nothing
	StatusWarn    = "warn"  // fixer ran with a non-zero exit it could not resolve
)

// StepResult is the outcome of one fixer or check.
type StepResult struct {
	Name     string              `json:"name"`
	Phase    string              `json:"phase"` // fixer | check
	Kind     string              `json:"kind,omitempty"`
	Command  string              `json:"command"`
	Status   string              `json:"status"`
	Exit     int                 `json:"exit"`
	Duration time.Duration       `json:"duration_ns"`
	Note     string              `json:"note,omitempty"` // skip reason, timeout, missing tool
	Changed  []string            `json:"changed,omitempty"`
	Failures []tokenizer.Failure `json:"failures,omitempty"`
	Raw      string              `json:"-"` // combined stdout+stderr
}

// Report aggregates one verification pass.
type Report struct {
	Fixers     []StepResult
	Checks     []StepResult
	Failures   []tokenizer.Failure
	FixedFiles []string // files rewritten by zero-token fixers
	Passed     bool
	Duration   time.Duration
}

// FailedChecks returns the checks that failed.
func (r *Report) FailedChecks() []StepResult {
	var out []StepResult
	for _, c := range r.Checks {
		if c.Status == StatusFailed {
			out = append(out, c)
		}
	}
	return out
}

// Runner executes the configured steps in Dir (normally a sandbox worktree).
type Runner struct {
	Cfg *config.Config
	Dir string
	Log io.Writer // optional progress output
}

func (r *Runner) logf(format string, a ...any) {
	if r.Log != nil {
		fmt.Fprintf(r.Log, format+"\n", a...)
	}
}

// Run executes every fixer, then every check, against scope.
func (r *Runner) Run(ctx context.Context, scope Scope) (*Report, error) {
	start := time.Now()
	rep := &Report{}
	fixedSet := map[string]bool{}
	for _, st := range r.Cfg.Fixers {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		res, err := r.runFixer(ctx, st, scope)
		if err != nil {
			return rep, err
		}
		for _, f := range res.Changed {
			fixedSet[f] = true
		}
		rep.Fixers = append(rep.Fixers, res)
		r.logf("  fixer  %-14s %s", st.Name, summarize(res))
	}
	for f := range fixedSet {
		rep.FixedFiles = append(rep.FixedFiles, f)
	}
	sort.Strings(rep.FixedFiles)

	rep.Passed = true
	for _, st := range r.Cfg.Checks {
		if err := ctx.Err(); err != nil {
			return rep, err
		}
		res, err := r.runCheck(ctx, st, scope)
		if err != nil {
			return rep, err
		}
		rep.Checks = append(rep.Checks, res)
		r.logf("  check  %-14s %s", st.Name, summarize(res))
		if res.Status == StatusFailed {
			rep.Passed = false
			rep.Failures = append(rep.Failures, res.Failures...)
		}
	}
	rep.Duration = time.Since(start)
	return rep, nil
}

func summarize(s StepResult) string {
	out := s.Status
	if s.Status == StatusFailed && len(s.Failures) > 0 {
		out = fmt.Sprintf("failed (%d finding(s))", len(s.Failures))
	}
	if s.Status == StatusFixed {
		out = fmt.Sprintf("fixed %d file(s) locally", len(s.Changed))
	}
	if s.Note != "" {
		out += " — " + s.Note
	}
	return out
}

// matching returns the in-scope files that exist and match the step's extensions.
func (r *Runner) matching(st config.Step, scope Scope) []string {
	var out []string
	for _, f := range scope.Files {
		if len(st.Extensions) > 0 {
			ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(f), "."))
			ok := false
			for _, e := range st.Extensions {
				if e == ext {
					ok = true
					break
				}
			}
			if !ok {
				continue
			}
		}
		if _, err := os.Stat(filepath.Join(r.Dir, filepath.FromSlash(f))); err != nil {
			continue
		}
		out = append(out, f)
	}
	return out
}

func (r *Runner) timeout() time.Duration {
	return time.Duration(r.Cfg.TimeoutSeconds) * time.Second
}

// ---- fixers -----------------------------------------------------------------

func (r *Runner) runFixer(ctx context.Context, st config.Step, scope Scope) (StepResult, error) {
	res := StepResult{Name: st.Name, Phase: "fixer", Command: st.Run}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	files := r.matching(st, scope)
	if len(st.Extensions) > 0 && len(files) == 0 {
		res.Status, res.Note = StatusSkipped, "no matching changed files"
		return res, nil
	}
	switch st.Run {
	case "builtin:tabs", "builtin:short-arrays":
		res.Command = st.Run + " (in-process)"
		for _, f := range files {
			abs := filepath.Join(r.Dir, filepath.FromSlash(f))
			data, err := os.ReadFile(abs)
			if err != nil {
				return res, err
			}
			lang := tokenizer.LangOf(f)
			var out string
			if st.Run == "builtin:tabs" {
				out = FixTabs(string(data), lang, scope.lineFn(f))
			} else {
				if lang != tokenizer.LangPHP {
					continue
				}
				out = FixShortArrays(string(data), scope.lineFn(f))
			}
			if out != string(data) {
				mode := os.FileMode(0o644)
				if info, err := os.Stat(abs); err == nil {
					mode = info.Mode().Perm()
				}
				if err := os.WriteFile(abs, []byte(out), mode); err != nil {
					return res, err
				}
				res.Changed = append(res.Changed, f)
			}
		}
		res.Status = StatusClean
		if len(res.Changed) > 0 {
			res.Status = StatusFixed
		}
		return res, nil
	}

	before := r.hashFiles(files)
	argvs, note, err := r.expand(st, files)
	if err != nil {
		return res, fmt.Errorf("fixer %s: %w", st.Name, err)
	}
	if note != "" {
		res.Status, res.Note = StatusSkipped, note
		return res, nil
	}
	for _, argv := range argvs {
		er, err := execute(ctx, r.Dir, argv, buildEnv(r.Dir), r.timeout(), st.Shell)
		if err != nil {
			return res, fmt.Errorf("fixer %s: %w", st.Name, err)
		}
		res.Exit = er.Exit
		res.Raw += er.Stdout + er.Stderr
		if er.TimedOut {
			res.Note = "timed out"
		}
	}
	after := r.hashFiles(files)
	for _, f := range files {
		if before[f] != after[f] {
			res.Changed = append(res.Changed, f)
		}
	}
	switch {
	case len(res.Changed) > 0:
		res.Status = StatusFixed
	case res.Exit != 0 && !contains(st.OkExitCodes, res.Exit):
		res.Status = StatusWarn
	default:
		res.Status = StatusClean
	}
	return res, nil
}

func (r *Runner) hashFiles(files []string) map[string]string {
	out := make(map[string]string, len(files))
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(r.Dir, filepath.FromSlash(f)))
		if err != nil {
			continue
		}
		out[f] = fmt.Sprintf("%x", sha1.Sum(b))
	}
	return out
}

// ---- checks -----------------------------------------------------------------

func (r *Runner) runCheck(ctx context.Context, st config.Step, scope Scope) (StepResult, error) {
	res := StepResult{Name: st.Name, Phase: "check", Kind: st.Kind, Command: st.Run}
	start := time.Now()
	defer func() { res.Duration = time.Since(start) }()

	files := r.matching(st, scope)
	if len(st.Extensions) > 0 && len(files) == 0 {
		res.Status, res.Note = StatusSkipped, "no matching changed files"
		return res, nil
	}
	if st.Run == "builtin:standards" {
		res.Command = "builtin:standards (in-process)"
		sc := scope
		sc.Files = files
		fails, err := CheckStandards(r.Dir, sc, r.Cfg)
		if err != nil {
			return res, err
		}
		res.Failures = fails
		res.Status = StatusPassed
		if len(fails) > 0 {
			res.Status = StatusFailed
			res.Exit = 1
		}
		return res, nil
	}

	argvs, note, err := r.expand(st, files)
	if err != nil {
		return res, fmt.Errorf("check %s: %w", st.Name, err)
	}
	if note != "" {
		if strings.HasPrefix(note, "tool not found") && st.Required {
			res.Status, res.Note, res.Exit = StatusFailed, note, 127
			res.Failures = []tokenizer.Failure{{Step: st.Name, Severity: "error", Message: note + " (required)"}}
			return res, nil
		}
		res.Status, res.Note = StatusSkipped, note
		return res, nil
	}
	ok := st.OkExitCodes
	if len(ok) == 0 {
		ok = []int{0}
	}
	res.Status = StatusPassed
	var raw strings.Builder
	for _, argv := range argvs {
		er, err := execute(ctx, r.Dir, argv, buildEnv(r.Dir), r.timeout(), st.Shell)
		if err != nil {
			return res, fmt.Errorf("check %s: %w", st.Name, err)
		}
		raw.WriteString(er.Stdout)
		if !strings.HasSuffix(er.Stdout, "\n") && er.Stdout != "" {
			raw.WriteString("\n")
		}
		raw.WriteString(er.Stderr)
		if er.TimedOut {
			res.Status, res.Exit, res.Note = StatusFailed, -1, fmt.Sprintf("timed out after %s", r.timeout())
			res.Failures = append(res.Failures, tokenizer.Failure{Step: st.Name, Severity: "error", Message: res.Note})
			continue
		}
		if !contains(ok, er.Exit) {
			res.Status, res.Exit = StatusFailed, er.Exit
		}
	}
	res.Raw = raw.String()
	if res.Status == StatusFailed {
		located := tokenizer.ExtractFailures(st.Name, res.Raw, r.Dir)
		res.Failures = append(located, res.Failures...)
	}
	return res, nil
}

// expand turns a command template into argv lists (one per file for {file}).
// A non-empty note means the step cannot run (missing tool, nothing to do).
func (r *Runner) expand(st config.Step, files []string) (argvs [][]string, note string, err error) {
	tokens, err := SplitCommand(st.Run)
	if err != nil {
		return nil, "", err
	}
	if len(tokens) == 0 {
		return nil, "", fmt.Errorf("empty command")
	}
	hasFiles, hasFile := false, false
	for _, t := range tokens {
		switch t {
		case "{files}":
			hasFiles = true
		case "{file}":
			hasFile = true
		}
	}
	if (hasFiles || hasFile) && len(files) == 0 {
		return nil, "no matching changed files", nil
	}
	if !st.Shell {
		bin, lerr := LookTool(r.Dir, tokens[0])
		if lerr != nil {
			return nil, "tool not found: " + tokens[0], nil
		}
		tokens[0] = bin
	}
	build := func(file string) []string {
		var argv []string
		for _, t := range tokens {
			switch t {
			case "{files}":
				for _, f := range files {
					argv = append(argv, quoteArg(f, st.Shell))
				}
			case "{file}":
				argv = append(argv, quoteArg(file, st.Shell))
			default:
				argv = append(argv, t)
			}
		}
		return argv
	}
	if hasFile {
		for _, f := range files {
			argvs = append(argvs, build(f))
		}
		return argvs, "", nil
	}
	return [][]string{build("")}, "", nil
}

func contains(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func quoteArg(s string, shell bool) string {
	if !shell {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
