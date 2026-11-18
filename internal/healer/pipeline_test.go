package healer

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"aegisloop/internal/config"
	"aegisloop/internal/sandbox"
	"aegisloop/internal/testutil"
)

const fixtureDir = "../../testdata/wp-vip"

// vipRepo builds a git repo from the committed "base" fixture and drops the
// "violations" fixture (what an AI assistant just wrote) into the working tree
// as uncommitted changes.
func vipRepo(t *testing.T) (repo string, cfg *config.Config) {
	t.Helper()
	repo = testutil.NewRepo(t, testutil.LoadDir(t, filepath.Join(fixtureDir, "base")))
	testutil.WriteFiles(t, repo, testutil.LoadDir(t, filepath.Join(fixtureDir, "violations")))
	cfg, found, err := config.Load(repo)
	if err != nil || !found {
		t.Fatalf("fixture config: found=%v err=%v", found, err)
	}
	return repo, cfg
}

var sliceHeader = regexp.MustCompile("(?m)^### (\\S+) lines (\\d+)-(\\d+)")

// healedFunction is the corrected function the scripted "LLM" answers with
// (shared with the CLI end-to-end test).
var healedFunction = func() string {
	b, err := os.ReadFile(filepath.Join(fixtureDir, "healed_function.txt"))
	if err != nil {
		panic(err)
	}
	return strings.TrimRight(string(b), "\n")
}()

// vipHealer plays the part of the LLM: it reads the line range AegisLoop sent
// and answers with a corrected function.
func vipHealer(t *testing.T) *scriptedLLM {
	return &scriptedLLM{reply: func(call int, system, user string) (string, error) {
		m := sliceHeader.FindStringSubmatch(user)
		if m == nil {
			t.Errorf("prompt has no code slice:\n%s", user)
			return "no", nil
		}
		return fmt.Sprintf("```aegis-replace %s %s-%s\n%s\n```", m[1], m[2], m[3], healedFunction), nil
	}}
}

func TestRegressionWordPressVIPViolationsAreCaughtHealedAndPromoted(t *testing.T) {
	repo, cfg := vipRepo(t)
	llm := vipHealer(t)
	var log strings.Builder
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Provider: llm, Log: &log})
	if err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if res.Status != StatusPassed {
		t.Fatalf("status %s (%s)\n%s\n%s", res.Status, res.Reason, Format(res), log.String())
	}
	if llm.calls != 1 {
		t.Fatalf("exactly one LLM call expected (tabs and array() are fixed locally for free), got %d", llm.calls)
	}

	// zero-token fixers handled the trivial violations before the model saw anything
	prompt := llm.prompts[0]
	for _, leaked := range []string{"Spaces used for indentation", "array() must be written as []", "acme_unrelated_helper"} {
		if strings.Contains(prompt, leaked) {
			t.Errorf("prompt must not contain %q:\n%s", leaked, prompt)
		}
	}
	for _, want := range []string{"[escape-output]", "[db-unprepared]", "[db-uncached]", "[unsanitized-input]", "function acme_get_posts_html"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if len(res.LocallyFixed) != 1 || res.LocallyFixed[0] != "inc/render.php" {
		t.Fatalf("locally fixed: %v", res.LocallyFixed)
	}
	if res.Tokens.Sent >= res.Tokens.Baseline {
		t.Fatalf("tokens: %+v", res.Tokens)
	}

	// the developer's working tree now holds the healed, standards-compliant file
	got := testutil.ReadFile(t, repo, "inc/render.php")
	for _, want := range []string{"esc_html( $row->post_title )", "wp_cache_get(", "$wpdb->prepare(", "sanitize_text_field(", "acme_unrelated_helper"} {
		if !strings.Contains(got, want) {
			t.Errorf("healed file lacks %q", want)
		}
	}
	if strings.Contains(got, "array(") || regexp.MustCompile(`(?m)^ {4}`).MatchString(got) {
		t.Fatalf("local fixers must have converted array() and spaces:\n%s", got)
	}
	if len(res.Promoted) != 1 || res.Promoted[0] != "inc/render.php" {
		t.Fatalf("promoted: %v", res.Promoted)
	}
	if st := testutil.Git(t, repo, "status", "--porcelain=v1"); strings.TrimSpace(st) != "M inc/render.php" {
		t.Fatalf("only the one file may differ from HEAD, status:\n%s", st)
	}
	// the sandbox is gone
	if _, err := os.Stat(filepath.Join(repo, ".aegis-sandbox")); err == nil {
		t.Fatal("sandbox must be pruned after success")
	}
	if php, err := exec.LookPath("php"); err == nil {
		if out, err := exec.Command(php, "-l", filepath.Join(repo, "inc/render.php")).CombinedOutput(); err != nil {
			t.Fatalf("healed file is not valid PHP: %s", out)
		}
	}
	t.Logf("\n%s", Format(res))
}

func TestAgentModeAppliesFreeFixesAndReturnsCompactRequest(t *testing.T) {
	repo, cfg := vipRepo(t)
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusNeedsFix || res.Payload == nil {
		t.Fatalf("%s", Format(res))
	}
	got := testutil.ReadFile(t, repo, "inc/render.php")
	if strings.Contains(got, "array(") || !strings.Contains(got, "\t$args = [") && !strings.Contains(got, "\t$args  = [") {
		t.Fatalf("zero-token fixes must reach the working tree even when no LLM is configured:\n%s", got)
	}
	if !strings.Contains(got, "echo $title;") {
		t.Fatal("agent mode must not invent semantic fixes")
	}
	md := res.Payload.Markdown()
	if !strings.Contains(md, "[escape-output]") || !strings.Contains(md, "function acme_get_posts_html") || strings.Contains(md, "acme_unrelated_helper") {
		t.Fatal(md)
	}
	if _, err := os.Stat(filepath.Join(repo, ".aegis-sandbox")); err == nil {
		t.Fatal("sandbox must be pruned")
	}
}

func TestFailurePreservesSandboxAndLeavesWorkingTreeUntouched(t *testing.T) {
	repo, cfg := vipRepo(t)
	before := testutil.ReadFile(t, repo, "inc/render.php")
	llm := &scriptedLLM{reply: func(int, string, string) (string, error) { return "Sorry, I cannot help with that.", nil }}
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Provider: llm})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || res.Reason != HaltUnusable {
		t.Fatalf("%s/%s", res.Status, res.Reason)
	}
	if testutil.ReadFile(t, repo, "inc/render.php") != before {
		t.Fatal("a failed heal must never modify the working tree")
	}
	if !res.Preserved || res.PatchPath == "" {
		t.Fatalf("on_failure=preserve must keep the worktree and save the patch: %+v", res)
	}
	if _, err := os.Stat(res.SandboxPath); err != nil {
		t.Fatal("sandbox directory missing")
	}
	if b, _ := os.ReadFile(res.PatchPath); !strings.Contains(string(b), "inc/render.php") {
		t.Fatal("saved patch should hold the local fixes")
	}
	if !strings.Contains(Format(res), "aegis clean") {
		t.Fatal("diagnostics should explain how to clean up")
	}
	// manual recovery path: list + prune
	infos, _ := sandbox.List(repo, cfg.Sandbox.Dir)
	if len(infos) != 1 {
		t.Fatalf("%v", infos)
	}
	if err := sandbox.PruneInfo(context.Background(), repo, cfg.Sandbox.Dir, infos[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(res.SandboxPath); err == nil {
		t.Fatal("PruneInfo did not remove the sandbox")
	}
}

func TestFailureWithPruneStrategyRestoresCleanState(t *testing.T) {
	repo, cfg := vipRepo(t)
	cfg.Sandbox.OnFailure = "prune"
	llm := &scriptedLLM{reply: func(int, string, string) (string, error) { return "nope", nil }}
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Provider: llm})
	if err != nil || res.Status != StatusFailed || res.Preserved {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".aegis-sandbox")); err == nil {
		t.Fatal("on_failure=prune must remove the sandbox")
	}
	if br := testutil.Git(t, repo, "branch", "--list", "aegis-fix-*"); strings.TrimSpace(br) != "" {
		t.Fatal("branch left behind")
	}
}

// End to end with the real runner: an LLM that keeps "fixing" by introducing a
// different violation each time must be cut off after exactly max_retries calls.
func TestInfiniteLLMLoopIsStoppedAtMaxRetries(t *testing.T) {
	repo, cfg := vipRepo(t)
	cfg.MaxRetries = 3
	before := testutil.ReadFile(t, repo, "inc/render.php")
	bodies := []string{"eval( $code );", "var_dump( $x );", "extract( $args );", "unserialize( $s );", "shell_exec( $c );"}
	llm := &scriptedLLM{reply: func(call int, _, user string) (string, error) {
		m := sliceHeader.FindStringSubmatch(user)
		if m == nil {
			t.Fatalf("no slice in prompt %d:\n%s", call, user)
		}
		fn := "function acme_get_posts_html( $author_id ) {\n\t" + bodies[call-1] + "\n}"
		return fmt.Sprintf("```aegis-replace %s %s-%s\n%s\n```", m[1], m[2], m[3], fn), nil
	}}
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Provider: llm})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusFailed || res.Reason != HaltMaxRetries {
		t.Fatalf("%s/%s\n%s", res.Status, res.Reason, Format(res))
	}
	if llm.calls != 3 || len(res.Attempts) != 3 {
		t.Fatalf("LLM was called %d times, limit is 3", llm.calls)
	}
	if testutil.ReadFile(t, repo, "inc/render.php") != before {
		t.Fatal("working tree touched")
	}
	if !res.Preserved || len(res.Remaining) == 0 {
		t.Fatalf("must preserve the sandbox and report diagnostics: %+v", res)
	}
}

func TestInterceptedPatchIsVerifiedInSandbox(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, filepath.Join(fixtureDir, "base")))
	cfg, _, err := config.Load(repo)
	if err != nil {
		t.Fatal(err)
	}
	patch := "diff --git a/inc/new.php b/inc/new.php\nnew file mode 100644\n--- /dev/null\n+++ b/inc/new.php\n@@ -0,0 +1,6 @@\n+<?php\n+function acme_new() {\n+    $a = array( 1, 2 );\n+    return $a;\n+}\n+\n"
	llm := &scriptedLLM{reply: func(int, string, string) (string, error) {
		t.Error("the LLM must not be called: only free local fixes are needed")
		return "", nil
	}}
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Patch: patch, Provider: llm})
	if err != nil || res.Status != StatusPassed {
		t.Fatalf("%v\n%s", err, Format(res))
	}
	got := testutil.ReadFile(t, repo, "inc/new.php")
	if got != "<?php\nfunction acme_new() {\n\t$a = [ 1, 2 ];\n\treturn $a;\n}\n\n" {
		t.Fatalf("%q", got)
	}
	if llm.calls != 0 || res.Tokens.Sent != 0 {
		t.Fatal("zero tokens expected")
	}
}

func TestRejectedPatchLeavesNoTrace(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, filepath.Join(fixtureDir, "base")))
	cfg, _, _ := config.Load(repo)
	bad := "--- a/plugin.php\n+++ b/plugin.php\n@@ -1,1 +1,1 @@\n-nothing like this\n+x\n"
	_, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Patch: bad})
	if err == nil || !strings.Contains(err.Error(), "patch rejected") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".aegis-sandbox")); err == nil {
		t.Fatal("sandbox left behind")
	}
	if st := testutil.Git(t, repo, "status", "--porcelain"); strings.TrimSpace(st) != "" {
		t.Fatalf("%s", st)
	}
}

func TestDryRunDoesNotTouchWorkingTree(t *testing.T) {
	repo, cfg := vipRepo(t)
	before := testutil.ReadFile(t, repo, "inc/render.php")
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Provider: vipHealer(t), DryRun: true})
	if err != nil || res.Status != StatusPassed {
		t.Fatalf("%v\n%s", err, Format(res))
	}
	if testutil.ReadFile(t, repo, "inc/render.php") != before || len(res.Promoted) != 0 {
		t.Fatal("dry run wrote to the working tree")
	}
	if !strings.Contains(res.Diff, "esc_html( $row->post_title )") {
		t.Fatalf("dry run should expose the proposed diff:\n%s", res.Diff)
	}
}

func TestFixGeneratesThenVerifies(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, filepath.Join(fixtureDir, "base")))
	cfg, _, _ := config.Load(repo)
	llm := &scriptedLLM{reply: func(call int, _, user string) (string, error) {
		if !strings.Contains(user, "add a shout helper") || !strings.Contains(user, "acme_unrelated_helper") {
			t.Errorf("generation prompt lacks task or file:\n%s", user)
		}
		// the model writes spaces and array() — the local fixers clean that up for free
		return "```aegis-replace inc/render.php 8-8\n}\n\nfunction acme_shout( $s ) {\n    $o = array( 'a' => $s );\n    return esc_html( strtoupper( $o['a'] ) );\n}\n```", nil
	}}
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Task: "add a shout helper", Files: []string{"inc/render.php"}, Provider: llm})
	if err != nil || res.Status != StatusPassed {
		t.Fatalf("%v\n%s", err, Format(res))
	}
	got := testutil.ReadFile(t, repo, "inc/render.php")
	if !strings.Contains(got, "\t$o = [ 'a' => $s ];") || llm.calls != 1 || res.Generated == nil {
		t.Fatalf("calls=%d\n%s", llm.calls, got)
	}
	if _, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg, Task: "x", Files: []string{"inc/render.php"}}); err == nil {
		t.Fatal("fix without an LLM must error")
	}
}

func TestNothingToVerify(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, filepath.Join(fixtureDir, "base")))
	cfg, _, _ := config.Load(repo)
	res, err := RunPipeline(context.Background(), PipelineOptions{RepoDir: repo, Cfg: cfg})
	if err != nil || res.Result == nil || res.Status != StatusNothing || !strings.Contains(Format(res), "NOTHING TO VERIFY") {
		t.Fatalf("a clean tree is not an error: %v %+v", err, res)
	}
	if _, err := os.Stat(filepath.Join(repo, ".aegis-sandbox")); err == nil {
		t.Fatal("sandbox left behind")
	}
}
