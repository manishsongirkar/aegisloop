package healer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/manishsongirkar/aegisloop/internal/config"
	"github.com/manishsongirkar/aegisloop/internal/runner"
	"github.com/manishsongirkar/aegisloop/internal/tokenizer"
)

// Workspace is the sandbox as seen by the healer.
type Workspace interface {
	Dir() string
	Read(rel string) (string, error)
	Write(rel, content string) error
	ApplyPatch(ctx context.Context, patch string) error
	Scope(ctx context.Context) (runner.Scope, error)
}

// Verifier runs the fixers and checks (implemented by *runner.Runner).
type Verifier interface {
	Run(ctx context.Context, scope runner.Scope) (*runner.Report, error)
}

// Result statuses.
const (
	StatusPassed   = "passed"
	StatusFailed   = "failed"
	StatusNeedsFix = "needs_fix" // no model configured: compact fix request returned to the caller
	// StatusNothing means there was no change to verify (clean tree, empty patch). It is not a failure.
	StatusNothing = "nothing_to_verify"
)

// Attempt records one heal iteration.
type Attempt struct {
	N                int      `json:"n"`
	Failures         int      `json:"failures_before"`
	Fingerprint      string   `json:"fingerprint"`
	PromptTokens     int      `json:"prompt_tokens"`
	CompletionTokens int      `json:"completion_tokens"`
	BaselineTokens   int      `json:"baseline_tokens"`
	EditsApplied     int      `json:"edits_applied"`
	ChangedFiles     []string `json:"changed_files,omitempty"`
	Problems         []string `json:"problems,omitempty"`
}

// TokenStats summarises token use against a naive "send whole files and raw
// output" retry loop.
type TokenStats struct {
	Sent         int     `json:"sent"`
	Received     int     `json:"received"`
	Baseline     int     `json:"baseline"`
	SavedPercent float64 `json:"saved_percent"`
	BudgetAlert  bool    `json:"budget_alert,omitempty"`
}

// StepSummary is a compact view of a runner.StepResult.
type StepSummary struct {
	Name     string `json:"name"`
	Phase    string `json:"phase"`
	Status   string `json:"status"`
	Note     string `json:"note,omitempty"`
	Findings int    `json:"findings,omitempty"`
}

// Result is the outcome of Healer.Run.
type Result struct {
	Status       string              `json:"status"`
	Reason       string              `json:"reason,omitempty"`
	Attempts     []Attempt           `json:"attempts"`
	Steps        []StepSummary       `json:"steps"`
	LocallyFixed []string            `json:"locally_fixed,omitempty"`
	Remaining    []tokenizer.Failure `json:"remaining_failures,omitempty"`
	Payload      *tokenizer.Payload  `json:"fix_request,omitempty"`
	Tokens       TokenStats          `json:"tokens"`
	Report       *runner.Report      `json:"-"`
}

// Healer drives the bounded verify → prompt → edit loop.
type Healer struct {
	Cfg    *config.Config
	WS     Workspace
	Verify Verifier
	LLM    Provider // nil = agent mode (status needs_fix)
	Task   string
	Log    io.Writer
}

func (h *Healer) logf(format string, a ...any) {
	if h.Log != nil {
		fmt.Fprintf(h.Log, format+"\n", a...)
	}
}

// Run verifies the workspace and heals it until every gate passes or the
// circuit breaker trips.
func (h *Healer) Run(ctx context.Context) (*Result, error) {
	res := &Result{}
	brk := NewBreaker(h.Cfg.MaxRetries)
	fixed := map[string]bool{}
	var problems []string
	skipObserve := false

	verify := func() (*runner.Report, error) {
		scope, err := h.WS.Scope(ctx)
		if err != nil {
			return nil, err
		}
		rep, err := h.Verify.Run(ctx, scope)
		if rep != nil {
			for _, f := range rep.FixedFiles {
				fixed[f] = true
			}
		}
		return rep, err
	}
	finish := func(status, reason string, rep *runner.Report) (*Result, error) {
		res.Status, res.Reason, res.Report = status, reason, rep
		res.LocallyFixed = sortedKeys(fixed)
		res.Steps = summarize(rep)
		if rep != nil && !rep.Passed {
			res.Remaining = rep.Failures
		}
		if res.Tokens.Baseline > 0 {
			res.Tokens.SavedPercent = 100 * (1 - float64(res.Tokens.Sent)/float64(res.Tokens.Baseline))
		}
		res.Tokens.BudgetAlert = h.Cfg.TokenBudgetAlert > 0 && res.Tokens.Sent > h.Cfg.TokenBudgetAlert
		if res.Tokens.BudgetAlert {
			h.logf("⚠ token budget alert: ~%d tokens sent this run (threshold %d)", res.Tokens.Sent, h.Cfg.TokenBudgetAlert)
		}
		return res, nil
	}

	h.logf("verifying (zero-token fixers first)…")
	rep, err := verify()
	if err != nil {
		return interrupted(res, rep, err, finish)
	}
	for {
		if rep.Passed {
			return finish(StatusPassed, "", rep)
		}
		fails := rep.Failures
		fp := tokenizer.Fingerprint(fails, unlocatedText(rep))
		if !skipObserve {
			if reason := brk.Observe(fp, len(fails)); reason != "" {
				h.logf("circuit breaker: %s", reason)
				return finish(StatusFailed, reason, rep)
			}
		}
		skipObserve = false

		payload, allowed, baseline := h.buildPayload(ctx, rep, brk.Attempts()+1, problems)
		res.Payload = &payload
		if h.LLM == nil {
			return finish(StatusNeedsFix, "no_llm_configured", rep)
		}
		if !brk.Allow() {
			h.logf("circuit breaker: %s after %d attempt(s)", HaltMaxRetries, brk.Attempts())
			return finish(StatusFailed, HaltMaxRetries, rep)
		}
		payload.Attempt = brk.Attempts()
		system := tokenizer.SystemPrompt(runner.StandardsText(h.Cfg))
		user := payload.Markdown()
		h.logf("heal attempt %d/%d: %d failure(s), ~%d prompt tokens via %s", brk.Attempts(), h.Cfg.MaxRetries, len(fails), tokenizer.EstimateTokens(system+user), h.LLM.Name())

		reply, usage, err := h.LLM.Complete(ctx, system, user)
		if err != nil {
			if ctx.Err() != nil {
				return interrupted(res, rep, ctx.Err(), finish)
			}
			return finish(StatusFailed, "llm_error: "+err.Error(), rep)
		}
		att := Attempt{N: brk.Attempts(), Failures: len(fails), Fingerprint: fp, BaselineTokens: baseline}
		att.PromptTokens = usage.InputTokens
		if att.PromptTokens == 0 {
			att.PromptTokens = tokenizer.EstimateTokens(system + user)
		}
		att.CompletionTokens = usage.OutputTokens
		if att.CompletionTokens == 0 {
			att.CompletionTokens = tokenizer.EstimateTokens(reply)
		}
		res.Tokens.Sent += att.PromptTokens
		res.Tokens.Received += att.CompletionTokens
		res.Tokens.Baseline += baseline + tokenizer.EstimateTokens(system) // a naive loop needs the instructions too

		parsed := ParseReply(reply)
		ap := h.apply(ctx, parsed, allowed)
		att.EditsApplied, att.ChangedFiles, att.Problems = ap.Applied, ap.Changed, ap.Problems
		res.Attempts = append(res.Attempts, att)
		problems = ap.Problems
		if ap.Applied == 0 {
			h.logf("  reply contained no applicable edit%s", suffixProblems(ap.Problems))
			if reason := brk.Unusable(); reason != "" {
				return finish(StatusFailed, reason, rep)
			}
			skipObserve = true // give the model one more chance with the problems listed
			continue
		}
		brk.Usable()
		h.logf("  applied %d edit(s) to %s", ap.Applied, strings.Join(ap.Changed, ", "))

		rep, err = verify()
		if err != nil {
			return interrupted(res, rep, err, finish)
		}
	}
}

func interrupted(res *Result, rep *runner.Report, err error, finish func(string, string, *runner.Report) (*Result, error)) (*Result, error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		r, _ := finish(StatusFailed, HaltInterrupted, rep)
		return r, err
	}
	r, _ := finish(StatusFailed, "error: "+err.Error(), rep)
	return r, err
}

func suffixProblems(p []string) string {
	if len(p) == 0 {
		return ""
	}
	return ": " + strings.Join(p, "; ")
}

func (h *Healer) apply(ctx context.Context, r Reply, allowed Allowed) ApplyResult {
	if r.Empty() {
		return ApplyResult{Problems: []string{"reply contained no aegis-replace blocks, SEARCH/REPLACE blocks or unified diff"}}
	}
	res := ApplyReply(h.WS, r, allowed)
	if strings.TrimSpace(r.Diff) != "" {
		if err := h.WS.ApplyPatch(ctx, r.Diff); err != nil {
			res.Problems = append(res.Problems, "diff: "+err.Error())
		} else {
			res.Applied++
		}
	}
	return res
}

// buildPayload creates the minimal fix request for the failing report and the
// ranges the model is allowed to edit.
func (h *Healer) buildPayload(ctx context.Context, rep *runner.Report, attempt int, problems []string) (tokenizer.Payload, Allowed, int) {
	p := tokenizer.Payload{Task: h.Task, Attempt: attempt, MaxAttempts: h.Cfg.MaxRetries}
	fails := append([]tokenizer.Failure{}, rep.Failures...)
	sort.SliceStable(fails, func(i, j int) bool {
		if fails[i].File != fails[j].File {
			return fails[i].File < fails[j].File
		}
		return fails[i].Line < fails[j].Line
	})
	if len(fails) > 40 {
		fails = fails[:40]
	}
	p.Failures = fails

	byFile := map[string][]int{}
	for _, f := range fails {
		if f.File != "" && f.Line > 0 {
			byFile[f.File] = append(byFile[f.File], f.Line)
		}
	}
	files := make([]string, 0, len(byFile))
	for f := range byFile {
		files = append(files, f)
	}
	sort.Strings(files)

	allowed := Allowed{}
	whole := map[string]string{}
	for _, f := range files {
		content, err := h.WS.Read(f)
		if err != nil {
			continue
		}
		whole[f] = content
		for _, s := range tokenizer.SliceFile(f, content, byFile[f], tokenizer.SliceOptions{ContextLines: h.Cfg.ContextLines, MaxLines: h.Cfg.MaxSliceLines}) {
			p.Slices = append(p.Slices, s)
			allowed[f] = append(allowed[f], Range{s.Start, s.End})
		}
	}

	h.addRelated(ctx, rep, &p, allowed)

	var rawAll, outBuf strings.Builder
	for _, c := range rep.Checks {
		if c.Status != runner.StatusFailed {
			continue
		}
		rawAll.WriteString(c.Raw)
		located := 0
		for _, f := range c.Failures {
			if f.File != "" {
				located++
			}
		}
		if c.Kind == "test" || c.Kind == "build" || located == 0 {
			st := tokenizer.StripNoise(c.Raw, tokenizer.StripOptions{Root: h.WS.Dir()})
			if strings.TrimSpace(st.Text) != "" {
				fmt.Fprintf(&outBuf, "[%s]\n%s\n", c.Name, st.Text)
			}
		}
	}
	p.Output = strings.TrimSpace(outBuf.String())
	if len(problems) > 0 {
		p.Task = strings.TrimSpace(p.Task + "\n\nYour previous reply could not be applied: " + strings.Join(problems, "; ") + ". Use only line ranges shown below.")
	}
	return p, allowed, tokenizer.Baseline(whole, rawAll.String())
}

// maxRelated bounds how many extra "code under test" slices one prompt carries.
const maxRelated = 6

// addRelated connects failing tests/builds to the code they exercise. A test
// failure is reported at the assertion, so the slice alone never contains the
// bug; the functions the failing code calls, taken from the files the patch
// changed, are added (and made editable) so the model can fix the real cause.
func (h *Healer) addRelated(ctx context.Context, rep *runner.Report, p *tokenizer.Payload, allowed Allowed) {
	failing := map[string]bool{}
	for _, c := range rep.Checks {
		if c.Status != runner.StatusFailed || (c.Kind != "test" && c.Kind != "build") {
			continue
		}
		for _, f := range c.Failures {
			if f.File != "" && f.Line > 0 {
				failing[f.File] = true
			}
		}
	}
	if len(failing) == 0 {
		return
	}
	var seed strings.Builder
	for _, s := range p.Slices {
		if failing[s.Path] {
			seed.WriteString(s.Text + "\n")
		}
	}
	names := tokenizer.CalledNames(seed.String())
	scope, err := h.WS.Scope(ctx)
	if err != nil || len(names) == 0 {
		return
	}
	opts := tokenizer.SliceOptions{ContextLines: h.Cfg.ContextLines, MaxLines: h.Cfg.MaxSliceLines}
	left := maxRelated
	for _, f := range scope.Files {
		if left <= 0 {
			break
		}
		content, err := h.WS.Read(f)
		if err != nil {
			continue
		}
		skip := func(start, end int) bool {
			for _, r := range allowed[f] {
				if start <= r.End && end >= r.Start {
					return true
				}
			}
			return false
		}
		for _, s := range tokenizer.RelatedSlices(f, content, names, skip, opts, left) {
			p.Slices = append(p.Slices, s)
			allowed[f] = append(allowed[f], Range{s.Start, s.End})
			left--
		}
	}
}

// Generate asks the model to implement Task in the given files (full content
// is sent, line-numbered) and applies the reply in the workspace. It is the
// first step of `aegis fix "<prompt>"`; verification and healing follow.
func (h *Healer) Generate(ctx context.Context, files []string) (*Attempt, error) {
	if h.LLM == nil {
		return nil, errors.New("`aegis fix` needs an LLM: set llm.provider in .aegis.yml (anthropic, openai, ollama or command)")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "## Task\n%s\n\n## Files\n", strings.TrimSpace(h.Task))
	allowed := Allowed{}
	whole := map[string]string{}
	for _, f := range files {
		content, err := h.WS.Read(f)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f, err)
		}
		whole[f] = content
		n := strings.Count(strings.TrimSuffix(content, "\n"), "\n") + 1
		sl := tokenizer.Slice{Path: f, Start: 1, End: n, Text: strings.TrimSuffix(content, "\n")}
		fmt.Fprintf(&b, "\n### %s (%d lines)\n```\n%s```\n", f, n, sl.Numbered())
		allowed[f] = []Range{{1, n}}
	}
	b.WriteString("\n## Reply format\n" + tokenizer.ReplyFormat + "\n\nTo add a new file use:\n```aegis-create path/to/new-file.php\n<full file content>\n```\n")
	system := tokenizer.SystemPrompt(runner.StandardsText(h.Cfg))
	user := b.String()
	h.logf("generating: ~%d prompt tokens via %s", tokenizer.EstimateTokens(system+user), h.LLM.Name())
	reply, usage, err := h.LLM.Complete(ctx, system, user)
	if err != nil {
		return nil, err
	}
	att := &Attempt{N: 0, PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, BaselineTokens: tokenizer.Baseline(whole, "")}
	if att.PromptTokens == 0 {
		att.PromptTokens = tokenizer.EstimateTokens(system + user)
	}
	if att.CompletionTokens == 0 {
		att.CompletionTokens = tokenizer.EstimateTokens(reply)
	}
	ap := h.apply(ctx, ParseReply(reply), allowed)
	att.EditsApplied, att.ChangedFiles, att.Problems = ap.Applied, ap.Changed, ap.Problems
	if ap.Applied == 0 {
		return att, fmt.Errorf("the model's reply contained no applicable edit%s", suffixProblems(ap.Problems))
	}
	return att, nil
}

func unlocatedText(rep *runner.Report) string {
	var b strings.Builder
	for _, c := range rep.Checks {
		if c.Status == runner.StatusFailed && len(c.Failures) == 0 {
			b.WriteString(c.Name + ":" + c.Raw)
		}
	}
	return b.String()
}

func summarize(rep *runner.Report) []StepSummary {
	if rep == nil {
		return nil
	}
	var out []StepSummary
	for _, s := range rep.Fixers {
		out = append(out, StepSummary{Name: s.Name, Phase: "fixer", Status: s.Status, Note: s.Note, Findings: len(s.Changed)})
	}
	for _, s := range rep.Checks {
		out = append(out, StepSummary{Name: s.Name, Phase: "check", Status: s.Status, Note: s.Note, Findings: len(s.Failures)})
	}
	return out
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
