package healer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aegisloop/internal/config"
	"aegisloop/internal/runner"
	"aegisloop/internal/tokenizer"
)

// ---- fakes ------------------------------------------------------------------------

type memWS struct {
	files   map[string]string
	patches []string
}

func (m *memWS) Dir() string { return "/ws" }
func (m *memWS) Read(rel string) (string, error) {
	c, ok := m.files[rel]
	if !ok {
		return "", fmt.Errorf("%s: no such file", rel)
	}
	return c, nil
}
func (m *memWS) Write(rel, content string) error { m.files[rel] = content; return nil }
func (m *memWS) ApplyPatch(_ context.Context, p string) error {
	m.patches = append(m.patches, p)
	return nil
}
func (m *memWS) Scope(context.Context) (runner.Scope, error) {
	var sc runner.Scope
	for f := range m.files {
		sc.Files = append(sc.Files, f)
	}
	return sc, nil
}

type verifyFunc func(ws *memWS, call int) *runner.Report

type fakeVerifier struct {
	ws    *memWS
	fn    verifyFunc
	calls int
}

func (v *fakeVerifier) Run(context.Context, runner.Scope) (*runner.Report, error) {
	v.calls++
	return v.fn(v.ws, v.calls), nil
}

func failing(msg string, line int) *runner.Report {
	f := tokenizer.Failure{Step: "lint", File: "a.php", Line: line, Message: msg, Severity: "error"}
	return &runner.Report{Failures: []tokenizer.Failure{f}, Checks: []runner.StepResult{{Name: "lint", Phase: "check", Kind: "lint", Status: runner.StatusFailed, Failures: []tokenizer.Failure{f}}}}
}

func passing() *runner.Report {
	return &runner.Report{Passed: true, Checks: []runner.StepResult{{Name: "lint", Phase: "check", Status: runner.StatusPassed}}}
}

type scriptedLLM struct {
	calls   int
	prompts []string
	reply   func(call int, system, user string) (string, error)
}

func (s *scriptedLLM) Name() string { return "scripted" }
func (s *scriptedLLM) Complete(_ context.Context, system, user string) (string, Usage, error) {
	s.calls++
	s.prompts = append(s.prompts, user)
	r, err := s.reply(s.calls, system, user)
	return r, Usage{}, err
}

func cfgWith(max int) *config.Config {
	c := config.Default()
	c.MaxRetries = max
	return c
}

func bigPHP() string {
	var b strings.Builder
	b.WriteString("<?php\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "function helper_%d() {\n\treturn %d;\n}\n\n", i, i)
	}
	b.WriteString("function broken() {\n\treturn BAD;\n}\n")
	return b.String()
}

func lineOf(src, needle string) int {
	for i, l := range strings.Split(src, "\n") {
		if strings.Contains(l, needle) {
			return i + 1
		}
	}
	return 0
}

func replaceBlock(path string, start, end int, body string) string {
	return fmt.Sprintf("```aegis-replace %s %d-%d\n%s\n```", path, start, end, body)
}

// ---- breaker ----------------------------------------------------------------------

func TestBreakerObserve(t *testing.T) {
	b := NewBreaker(3)
	if r := b.Observe("A", 2); r != "" {
		t.Fatal(r)
	}
	if r := b.Observe("B", 1); r != "" {
		t.Fatal("progress must not trip the breaker:", r)
	}
	if r := b.Observe("B", 1); r != HaltNoProgress {
		t.Fatalf("identical failures: %q", r)
	}
	b = NewBreaker(3)
	b.Observe("A", 1)
	b.Observe("B", 1)
	if r := b.Observe("A", 1); r != HaltThrashing {
		t.Fatalf("oscillation: %q", r)
	}
	b = NewBreaker(3)
	b.Observe("A", 1)
	if r := b.Observe("B", 2); r != "" {
		t.Fatalf("one increase is tolerated: %q", r)
	}
	if r := b.Observe("C", 3); r != HaltRegressing {
		t.Fatalf("two increases in a row: %q", r)
	}
	b = NewBreaker(3)
	b.Observe("A", 1)
	b.Observe("B", 2)
	b.Observe("C", 1) // a decrease resets the streak
	if r := b.Observe("D", 2); r != "" {
		t.Fatalf("streak must reset: %q", r)
	}
}

func TestBreakerAllowAndUnusable(t *testing.T) {
	b := NewBreaker(2)
	if !b.Allow() || !b.Allow() || b.Allow() || b.Attempts() != 2 {
		t.Fatal("Allow must permit exactly MaxRetries attempts")
	}
	if NewBreaker(0).Allow() {
		t.Fatal("max_retries 0 allows no heal attempt")
	}
	b = NewBreaker(5)
	if b.Unusable() != "" || b.Unusable() != HaltUnusable {
		t.Fatal("two unusable replies in a row halt")
	}
	b = NewBreaker(5)
	b.Unusable()
	b.Usable()
	if b.Unusable() != "" {
		t.Fatal("Usable resets the counter")
	}
}

// ---- reply parsing & applying --------------------------------------------------------

func TestParseReplyFormats(t *testing.T) {
	r := ParseReply("Sure!\n```aegis-replace ./src/a.php 12-14\n  12| \tfoo();\n  13| \tbar();\n```\n```aegis-replace src/b.php 3\nx\n```\n```aegis-create src/new.php\n<?php\n```\n")
	if len(r.Lines) != 2 || r.Lines[0].Path != "src/a.php" || r.Lines[0].Start != 12 || r.Lines[0].End != 14 {
		t.Fatalf("%+v", r.Lines)
	}
	if r.Lines[0].Text != "\tfoo();\n\tbar();" {
		t.Fatalf("gutter not stripped: %q", r.Lines[0].Text)
	}
	if r.Lines[1].Start != 3 || r.Lines[1].End != 3 || len(r.Creates) != 1 {
		t.Fatalf("%+v", r)
	}

	r = ParseReply("src/a.php\n<<<<<<< SEARCH\nold();\n=======\nnew();\n>>>>>>> REPLACE\n")
	if len(r.Searches) != 1 || r.Searches[0].Path != "src/a.php" || r.Searches[0].Search != "old();" || r.Searches[0].Replace != "new();" {
		t.Fatalf("%+v", r)
	}

	r = ParseReply("Here:\n```diff\ndiff --git a/a.txt b/a.txt\n--- a/a.txt\n+++ b/a.txt\n@@ -1 +1 @@\n-x\n+y\n```\nDone")
	if !strings.HasPrefix(r.Diff, "diff --git") || strings.Contains(r.Diff, "```") || !strings.HasSuffix(r.Diff, "+y\n") {
		t.Fatalf("%q", r.Diff)
	}

	for _, junk := range []string{"", "I fixed it, trust me.", "```aegis-replace only-path\nx\n```", "```aegis-replace a.php x-y\nx\n```"} {
		if !ParseReply(junk).Empty() {
			t.Errorf("%q must parse as empty", junk)
		}
	}
}

func TestApplyReplyRangesAndSafety(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "l1\nl2\nl3\nl4\nl5\n", "crlf.php": "a\r\nb\r\nc\r\n"}}
	allowed := Allowed{"a.php": {{2, 4}}, "crlf.php": {{1, 3}}}

	res := ApplyReply(ws, Reply{Lines: []LineEdit{
		{Path: "a.php", Start: 2, End: 2, Text: "L2"},
		{Path: "a.php", Start: 4, End: 4, Text: "L4a\nL4b"},
		{Path: "a.php", Start: 1, End: 2, Text: "outside"},  // line 1 was not shown
		{Path: "a.php", Start: 3, End: 9, Text: "too far"},  // beyond EOF
		{Path: "other.php", Start: 1, End: 1, Text: "nope"}, // unknown file
	}}, allowed)
	if ws.files["a.php"] != "l1\nL2\nl3\nL4a\nL4b\nl5\n" {
		t.Fatalf("%q", ws.files["a.php"])
	}
	if res.Applied != 2 || len(res.Problems) != 3 {
		t.Fatalf("%+v", res)
	}

	// deleting a range (empty body) and CRLF preservation
	res = ApplyReply(ws, Reply{Lines: []LineEdit{{Path: "crlf.php", Start: 2, End: 2, Text: ""}}}, allowed)
	if ws.files["crlf.php"] != "a\r\nc\r\n" || res.Applied != 1 {
		t.Fatalf("%q %+v", ws.files["crlf.php"], res)
	}

	// overlapping edits: the later-numbered one wins, the other is rejected
	ws.files["o.php"] = "1\n2\n3\n4\n"
	res = ApplyReply(ws, Reply{Lines: []LineEdit{{Path: "o.php", Start: 1, End: 3, Text: "x"}, {Path: "o.php", Start: 3, End: 4, Text: "y"}}}, Allowed{"o.php": {{1, 4}}})
	if res.Applied != 1 || len(res.Problems) != 1 || !strings.Contains(res.Problems[0], "overlaps") {
		t.Fatalf("%+v", res)
	}
}

func TestApplyReplySearchReplaceAndCreate(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "<?php\nfunction f() {\n\t$x = 1;\n\treturn $x;\n}\n", "dup.php": "a\na\n"}}
	allowed := Allowed{"a.php": {{1, 5}}, "dup.php": {{1, 2}}}
	res := ApplyReply(ws, Reply{Searches: []SearchReplace{
		{Path: "a.php", Search: "$x = 1;", Replace: "$x = 2;"},
		{Path: "a.php", Search: "    return   $x;", Replace: "\treturn (int) $x;"}, // whitespace-insensitive fallback
		{Path: "a.php", Search: "not there", Replace: "x"},
		{Path: "dup.php", Search: "a", Replace: "b"},
	}}, allowed)
	if !strings.Contains(ws.files["a.php"], "$x = 2;") || !strings.Contains(ws.files["a.php"], "\treturn (int) $x;") {
		t.Fatalf("%q", ws.files["a.php"])
	}
	if res.Applied != 2 || len(res.Problems) != 2 {
		t.Fatalf("%+v", res)
	}
	res = ApplyReply(ws, Reply{Creates: []Create{{Path: "new/f.php", Text: "<?php"}, {Path: "a.php", Text: "x"}}}, nil)
	if ws.files["new/f.php"] != "<?php\n" || res.Applied != 1 || len(res.Problems) != 1 {
		t.Fatalf("%+v %q", res, ws.files["new/f.php"])
	}
}

// ---- the loop ---------------------------------------------------------------------

func TestHealSucceedsWithMinimalPrompt(t *testing.T) {
	src := bigPHP()
	ws := &memWS{files: map[string]string{"a.php": src}}
	badLine := lineOf(src, "return BAD;")
	v := &fakeVerifier{ws: ws, fn: func(w *memWS, call int) *runner.Report {
		if strings.Contains(w.files["a.php"], "BAD") {
			return failing("Undefined constant BAD", lineOf(w.files["a.php"], "return BAD;"))
		}
		return passing()
	}}
	llm := &scriptedLLM{reply: func(call int, system, user string) (string, error) {
		return "Fixed:\n" + replaceBlock("a.php", badLine-1, badLine+1, "function broken() {\n\treturn 'ok';\n}"), nil
	}}
	h := &Healer{Cfg: cfgWith(3), WS: ws, Verify: v, LLM: llm}
	res, err := h.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusPassed || llm.calls != 1 || len(res.Attempts) != 1 || res.Attempts[0].EditsApplied != 1 {
		t.Fatalf("%+v calls=%d", res, llm.calls)
	}
	if !strings.Contains(ws.files["a.php"], "return 'ok';") || strings.Contains(ws.files["a.php"], "BAD") {
		t.Fatal(ws.files["a.php"])
	}
	prompt := llm.prompts[0]
	if strings.Contains(prompt, "helper_40") || !strings.Contains(prompt, "function broken()") || !strings.Contains(prompt, "Undefined constant BAD") {
		t.Fatalf("prompt must hold only the failing function:\n%s", prompt)
	}
	if res.Tokens.Baseline == 0 || res.Tokens.SavedPercent < 70 {
		t.Fatalf("expected ≥70%% token savings, got %+v", res.Tokens)
	}
	t.Logf("tokens: %+v", res.Tokens)
}

// An LLM that always "fixes" something new but never converges must be stopped
// at exactly max_retries calls — never one more.
func TestCircuitBreakerStopsInfiniteLLMLoopAtMaxRetries(t *testing.T) {
	letters := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima"}
	for _, max := range []int{0, 1, 2, 3, 5} {
		t.Run(fmt.Sprintf("max_retries=%d", max), func(t *testing.T) {
			ws := &memWS{files: map[string]string{"a.php": "<?php\nfunction f() {\n\treturn 0;\n}\n"}}
			v := &fakeVerifier{ws: ws, fn: func(w *memWS, call int) *runner.Report {
				// a brand-new, non-numeric problem every time; failure count stays 1
				return failing("problem "+letters[call%len(letters)], 3)
			}}
			llm := &scriptedLLM{reply: func(call int, _, _ string) (string, error) {
				return replaceBlock("a.php", 3, 3, fmt.Sprintf("\treturn %d;", call)), nil
			}}
			res, err := (&Healer{Cfg: cfgWith(max), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != StatusFailed || res.Reason != HaltMaxRetries {
				t.Fatalf("%s/%s", res.Status, res.Reason)
			}
			if llm.calls != max || v.calls != max+1 {
				t.Fatalf("LLM calls = %d (want %d), verifications = %d (want %d)", llm.calls, max, v.calls, max+1)
			}
			if len(res.Remaining) != 1 {
				t.Fatalf("diagnostics must list what still fails: %+v", res.Remaining)
			}
		})
	}
}

func TestBreakerNoProgressHaltsImmediately(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "1\n2\n3\n"}}
	v := &fakeVerifier{ws: ws, fn: func(*memWS, int) *runner.Report { return failing("same problem", 2) }}
	llm := &scriptedLLM{reply: func(call int, _, _ string) (string, error) {
		return replaceBlock("a.php", 2, 2, fmt.Sprintf("edit%c", 'a'+call)), nil
	}}
	res, _ := (&Healer{Cfg: cfgWith(5), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
	if res.Reason != HaltNoProgress || llm.calls != 1 {
		t.Fatalf("a persistent error must stop after the first useless attempt: %s calls=%d", res.Reason, llm.calls)
	}
}

func TestBreakerDetectsThrashing(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "1\n2\n3\n"}}
	states := []string{"problem A", "problem B", "problem A", "problem B"}
	v := &fakeVerifier{ws: ws, fn: func(_ *memWS, call int) *runner.Report { return failing(states[(call-1)%4], 2) }}
	llm := &scriptedLLM{reply: func(call int, _, _ string) (string, error) {
		return replaceBlock("a.php", 2, 2, fmt.Sprintf("v%c", 'a'+call)), nil
	}}
	res, _ := (&Healer{Cfg: cfgWith(9), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
	if res.Reason != HaltThrashing || llm.calls != 2 {
		t.Fatalf("%s calls=%d", res.Reason, llm.calls)
	}
}

func TestBreakerDetectsRegression(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "1\n2\n3\n"}}
	v := &fakeVerifier{ws: ws, fn: func(_ *memWS, call int) *runner.Report {
		rep := &runner.Report{}
		for i := 0; i < call; i++ { // 1, 2, 3 … failures, each state distinct
			f := tokenizer.Failure{Step: "lint", File: "a.php", Line: 2, Message: fmt.Sprintf("unique %c%c", 'a'+call, 'a'+i)}
			rep.Failures = append(rep.Failures, f)
		}
		rep.Checks = []runner.StepResult{{Name: "lint", Status: runner.StatusFailed, Failures: rep.Failures}}
		return rep
	}}
	llm := &scriptedLLM{reply: func(call int, _, _ string) (string, error) {
		return replaceBlock("a.php", 2, 2, fmt.Sprintf("w%c", 'a'+call)), nil
	}}
	res, _ := (&Healer{Cfg: cfgWith(9), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
	if res.Reason != HaltRegressing || llm.calls != 2 {
		t.Fatalf("%s calls=%d", res.Reason, llm.calls)
	}
}

func TestUnusableRepliesHaltAfterOneSecondChance(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "1\n2\n3\n"}}
	v := &fakeVerifier{ws: ws, fn: func(*memWS, int) *runner.Report { return failing("problem", 2) }}
	llm := &scriptedLLM{reply: func(int, string, string) (string, error) { return "I would change line two.", nil }}
	res, _ := (&Healer{Cfg: cfgWith(5), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
	if res.Reason != HaltUnusable || llm.calls != 2 {
		t.Fatalf("%s calls=%d", res.Reason, llm.calls)
	}
	if !strings.Contains(llm.prompts[1], "could not be applied") {
		t.Fatalf("the retry must tell the model what went wrong:\n%s", llm.prompts[1])
	}
	if v.calls != 1 {
		t.Fatalf("no edit was applied, so there is nothing new to verify (%d runs)", v.calls)
	}
}

func TestEditsOutsideShownCodeAreRejected(t *testing.T) {
	src := bigPHP()
	ws := &memWS{files: map[string]string{"a.php": src}}
	v := &fakeVerifier{ws: ws, fn: func(w *memWS, _ int) *runner.Report {
		return failing("problem", lineOf(w.files["a.php"], "return BAD;"))
	}}
	llm := &scriptedLLM{reply: func(int, string, string) (string, error) {
		return replaceBlock("a.php", 1, 3, "// vandalised"), nil // top of the file was never shown
	}}
	res, _ := (&Healer{Cfg: cfgWith(3), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
	if res.Reason != HaltUnusable || ws.files["a.php"] != src {
		t.Fatalf("%s — file must be untouched", res.Reason)
	}
}

func TestAgentModeReturnsFixRequest(t *testing.T) {
	src := bigPHP()
	ws := &memWS{files: map[string]string{"a.php": src}}
	v := &fakeVerifier{ws: ws, fn: func(w *memWS, _ int) *runner.Report {
		return failing("Undefined constant BAD", lineOf(w.files["a.php"], "return BAD;"))
	}}
	res, err := (&Healer{Cfg: cfgWith(3), WS: ws, Verify: v, Task: "ship it"}).Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusNeedsFix || res.Payload == nil || len(res.Payload.Slices) != 1 || v.calls != 1 {
		t.Fatalf("%+v", res)
	}
	md := res.Payload.Markdown()
	if !strings.Contains(md, "function broken()") || strings.Contains(md, "helper_40") || !strings.Contains(md, "ship it") {
		t.Fatal(md)
	}
}

func TestLLMErrorAndInterruption(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "1\n2\n"}}
	v := &fakeVerifier{ws: ws, fn: func(*memWS, int) *runner.Report { return failing("p", 1) }}
	llm := &scriptedLLM{reply: func(int, string, string) (string, error) { return "", errors.New("HTTP 401: bad key") }}
	res, err := (&Healer{Cfg: cfgWith(3), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
	if err != nil || res.Status != StatusFailed || !strings.Contains(res.Reason, "llm_error: HTTP 401") {
		t.Fatalf("%+v %v", res, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	llm = &scriptedLLM{reply: func(int, string, string) (string, error) { cancel(); return "", ctx.Err() }}
	res, err = (&Healer{Cfg: cfgWith(3), WS: ws, Verify: v, LLM: llm}).Run(ctx)
	if !errors.Is(err, context.Canceled) || res.Reason != HaltInterrupted {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestSearchReplaceReplyAndLocalFixTracking(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "<?php\nfunction f() {\n\treturn BAD;\n}\n"}}
	v := &fakeVerifier{ws: ws, fn: func(w *memWS, call int) *runner.Report {
		rep := passing()
		if strings.Contains(w.files["a.php"], "BAD") {
			rep = failing("bad constant", 3)
		}
		rep.FixedFiles = []string{"a.php"}
		return rep
	}}
	llm := &scriptedLLM{reply: func(int, string, string) (string, error) {
		return "a.php\n<<<<<<< SEARCH\n\treturn BAD;\n=======\n\treturn 1;\n>>>>>>> REPLACE", nil
	}}
	res, _ := (&Healer{Cfg: cfgWith(3), WS: ws, Verify: v, LLM: llm}).Run(context.Background())
	if res.Status != StatusPassed || len(res.LocallyFixed) != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestGenerate(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "<?php\nfunction f() {\n\treturn 1;\n}\n"}}
	llm := &scriptedLLM{reply: func(_ int, _, user string) (string, error) {
		if !strings.Contains(user, "add a docblock") || !strings.Contains(user, "    3|") {
			t.Errorf("prompt lacks task or numbered code:\n%s", user)
		}
		return replaceBlock("a.php", 2, 2, "/** Docs. */\nfunction f() {") + "\n```aegis-create b.php\n<?php\n```", nil
	}}
	h := &Healer{Cfg: cfgWith(3), WS: ws, LLM: llm, Task: "add a docblock to f and a b.php"}
	att, err := h.Generate(context.Background(), []string{"a.php"})
	if err != nil || att.EditsApplied != 2 {
		t.Fatalf("%+v %v", att, err)
	}
	if !strings.Contains(ws.files["a.php"], "/** Docs. */") || ws.files["b.php"] != "<?php\n" {
		t.Fatal(ws.files)
	}
	if _, err := (&Healer{Cfg: cfgWith(3), WS: ws}).Generate(context.Background(), []string{"a.php"}); err == nil {
		t.Fatal("Generate without an LLM must fail")
	}
	llm.reply = func(int, string, string) (string, error) { return "no edits", nil }
	if _, err := h.Generate(context.Background(), []string{"a.php"}); err == nil {
		t.Fatal("an unusable reply must be an error")
	}
	if _, err := h.Generate(context.Background(), []string{"missing.php"}); err == nil {
		t.Fatal("missing file must be an error")
	}
}

// ---- providers --------------------------------------------------------------------

func TestProviders(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := new(strings.Builder)
		b := make([]byte, 4096)
		for {
			n, err := r.Body.Read(b)
			buf.Write(b[:n])
			if err != nil {
				break
			}
		}
		gotBody, gotPath = buf.String(), r.URL.Path
		gotAuth = r.Header.Get("x-api-key") + r.Header.Get("Authorization")
		switch {
		case strings.HasSuffix(r.URL.Path, "/messages"):
			fmt.Fprint(w, `{"content":[{"type":"text","text":"anthropic says hi"}],"usage":{"input_tokens":11,"output_tokens":7}}`)
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			fmt.Fprint(w, `{"choices":[{"message":{"content":"openai says hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`)
		case strings.HasSuffix(r.URL.Path, "/api/chat"):
			fmt.Fprint(w, `{"message":{"content":"ollama says hi"},"prompt_eval_count":9,"eval_count":2}`)
		default:
			http.Error(w, "nope", 404)
		}
	}))
	defer srv.Close()
	env := func(k string) string {
		if k == "ANTHROPIC_API_KEY" || k == "OPENAI_API_KEY" || k == "MY_KEY" {
			return "sekret"
		}
		return ""
	}
	cases := []struct {
		cfg       config.LLM
		want      string
		usage     Usage
		path, key string
		body      string
	}{
		{config.LLM{Provider: "anthropic", Endpoint: srv.URL + "/v1/messages", Model: "m1"}, "anthropic says hi", Usage{11, 7}, "/v1/messages", "sekret", `"model":"m1"`},
		{config.LLM{Provider: "openai", Endpoint: srv.URL + "/v1", APIKeyEnv: "MY_KEY"}, "openai says hi", Usage{5, 3}, "/v1/chat/completions", "Bearer sekret", `"role":"system"`},
		{config.LLM{Provider: "ollama", Endpoint: srv.URL, Model: "q"}, "ollama says hi", Usage{9, 2}, "/api/chat", "", `"stream":false`},
	}
	for _, c := range cases {
		p, err := NewProvider(c.cfg, env)
		if err != nil || p == nil {
			t.Fatalf("%s: %v", c.cfg.Provider, err)
		}
		out, usage, err := p.Complete(context.Background(), "SYS", "USER")
		if err != nil || out != c.want || usage != c.usage {
			t.Fatalf("%s: %q %+v %v", c.cfg.Provider, out, usage, err)
		}
		if gotPath != c.path || gotAuth != c.key || !strings.Contains(gotBody, c.body) || !strings.Contains(gotBody, "SYS") || !strings.Contains(gotBody, "USER") {
			t.Fatalf("%s request wrong: path=%s auth=%s body=%s", c.cfg.Provider, gotPath, gotAuth, gotBody)
		}
	}
}

func TestProviderRetriesAndErrors(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if hits == 1 {
			http.Error(w, "rate limited", 429)
			return
		}
		if r.URL.Path == "/v1/chat/completions" && hits == 99 {
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	}))
	defer srv.Close()
	p, _ := NewProvider(config.LLM{Provider: "openai", Endpoint: srv.URL + "/v1"}, func(string) string { return "k" })
	out, _, err := p.Complete(context.Background(), "s", "u")
	if err != nil || out != "ok" || hits != 2 {
		t.Fatalf("429 must be retried: %q %v hits=%d", out, err, hits)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, `{"error":"invalid key"}`, 401) }))
	defer bad.Close()
	p, _ = NewProvider(config.LLM{Provider: "anthropic", Endpoint: bad.URL}, func(string) string { return "k" })
	if _, _, err := p.Complete(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "k\"") {
		t.Fatalf("%v", err)
	}

	if _, err := NewProvider(config.LLM{Provider: "anthropic"}, func(string) string { return "" }); err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Fatalf("missing key: %v", err)
	}
	if p, err := NewProvider(config.LLM{Provider: "none"}, nil); p != nil || err != nil {
		t.Fatal("none → nil provider")
	}
	if _, err := NewProvider(config.LLM{Provider: "bogus"}, nil); err == nil {
		t.Fatal("unknown provider")
	}
	if _, err := NewProvider(config.LLM{Provider: "command"}, nil); err == nil {
		t.Fatal("empty command")
	}
}

func TestCommandProvider(t *testing.T) {
	p, err := NewProvider(config.LLM{Provider: "command", Command: `cat; echo "<<done>>"`, TimeoutSeconds: 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := p.Complete(context.Background(), "SYSTEM", "USER")
	if err != nil || !strings.Contains(out, "SYSTEM") || !strings.Contains(out, "USER") || !strings.Contains(out, "<<done>>") {
		t.Fatalf("%q %v", out, err)
	}
	p, _ = NewProvider(config.LLM{Provider: "command", Command: "echo boom >&2; exit 2", TimeoutSeconds: 5}, nil)
	if _, _, err := p.Complete(context.Background(), "s", "u"); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("%v", err)
	}
}

// A failing test is reported at the assertion. The bug lives in the function
// the test calls, which must therefore be shown and be editable.
func TestHealFixesSourceBugExposedByFailingTest(t *testing.T) {
	src := "package p\n\nfunc Add(a, b int) int { return a - b }\n"
	test := "package p\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n"
	ws := &memWS{files: map[string]string{"add.go": src, "add_test.go": test}}
	f := tokenizer.Failure{Step: "go-test", File: "add_test.go", Line: 7, Message: "bad", Severity: "error"}
	ver := &fakeVerifier{ws: ws, fn: func(w *memWS, _ int) *runner.Report {
		if strings.Contains(w.files["add.go"], "a + b") {
			return &runner.Report{Passed: true}
		}
		return &runner.Report{Failures: []tokenizer.Failure{f}, Checks: []runner.StepResult{{Name: "go-test", Phase: "check", Kind: "test", Status: runner.StatusFailed, Failures: []tokenizer.Failure{f}, Raw: "--- FAIL: TestAdd\n    add_test.go:7: bad\n"}}}
	}}
	llm := &scriptedLLM{reply: func(_ int, _, user string) (string, error) {
		if !strings.Contains(user, "add.go lines 3-3") || !strings.Contains(user, "called by the failing code") {
			return "", fmt.Errorf("the function under test was not shown:\n%s", user)
		}
		return replaceBlock("add.go", 3, 3, "func Add(a, b int) int { return a + b }"), nil
	}}
	h := &Healer{Cfg: cfgWith(3), WS: ws, Verify: ver, LLM: llm}
	res, err := h.Run(context.Background())
	if err != nil || res.Status != StatusPassed {
		t.Fatalf("%v %+v", err, res)
	}
	if !strings.Contains(ws.files["add.go"], "a + b") || ws.files["add_test.go"] != test {
		t.Fatal("the source must be fixed and the test left alone")
	}
}

// Lint failures are located at the offending line; no related code is added.
func TestNoRelatedCodeForLintFailures(t *testing.T) {
	ws := &memWS{files: map[string]string{"a.php": "<?php\nfunction a() {\n\treturn b();\n}\nfunction b() {\n\treturn 1;\n}\n"}}
	h := &Healer{Cfg: cfgWith(3), WS: ws}
	p, _, _ := h.buildPayload(context.Background(), failing("lint", 3), 1, nil)
	for _, s := range p.Slices {
		if s.Related {
			t.Fatalf("unexpected related slice %+v", s)
		}
	}
}
