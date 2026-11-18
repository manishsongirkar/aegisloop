package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aegisloop/internal/testutil"
)

const fixtures = "../../testdata/wp-vip"

func exec(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	code = run(context.Background(), args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func vipRepo(t *testing.T) string {
	t.Helper()
	repo := testutil.NewRepo(t, testutil.LoadDir(t, filepath.Join(fixtures, "base")))
	testutil.WriteFiles(t, repo, testutil.LoadDir(t, filepath.Join(fixtures, "violations")))
	return repo
}

func TestBasicCommands(t *testing.T) {
	if code, _, errs := exec(t, ""); code != exitUsage || !strings.Contains(errs, "Usage:") {
		t.Fatalf("%d %s", code, errs)
	}
	if code, out, _ := exec(t, "", "version"); code != 0 || !strings.HasPrefix(out, "aegis ") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out, _ := exec(t, "", "help"); code != 0 || !strings.Contains(out, "aegis mcp") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _, errs := exec(t, "", "frobnicate"); code != exitUsage || !strings.Contains(errs, "unknown command") {
		t.Fatalf("%d %s", code, errs)
	}
	if code, out, _ := exec(t, "", "rules"); code != 0 || !strings.Contains(out, "escape-output") || !strings.Contains(out, "db-uncached") {
		t.Fatalf("%d %s", code, out)
	}
	if code, _, errs := exec(t, "", "verify", "--bogus"); code != exitUsage || errs == "" {
		t.Fatalf("bad flag: %d %s", code, errs)
	}
	if code, _, _ := exec(t, "", "verify", "-h"); code != 0 {
		t.Fatalf("-h must not be an error: %d", code)
	}
}

func TestInitDetectsAndWritesConfig(t *testing.T) {
	dir := t.TempDir()
	testutil.WriteFiles(t, dir, map[string]string{
		"composer.json": `{"require-dev":{"automattic/vipwpcs":"*","phpunit/phpunit":"*"}}`,
		"package.json":  `{"devDependencies":{"eslint":"8"},"scripts":{"build":"x"}}`,
	})
	code, out, errs := exec(t, "", "init", "--repo", dir)
	if code != 0 {
		t.Fatalf("%d %s %s", code, out, errs)
	}
	if !strings.Contains(out, "PHP / WordPress") || !strings.Contains(out, "JavaScript") {
		t.Fatal(out)
	}
	cfg := testutil.ReadFile(t, dir, ".aegis.yml")
	for _, want := range []string{"max_retries: 3", "strict_tabs: true", "phpcbf --standard=WordPress-VIP-Go", "phpcs --standard=WordPress-VIP-Go", "phpunit", "eslint --fix", "npm run build", "token_budget_alert"} {
		if !strings.Contains(cfg, want) {
			t.Errorf(".aegis.yml lacks %q:\n%s", want, cfg)
		}
	}
	if code, _, errs := exec(t, "", "init", "--repo", dir); code != exitUsage || !strings.Contains(errs, "already exists") {
		t.Fatalf("must not overwrite: %d %s", code, errs)
	}
	if code, _, _ := exec(t, "", "init", "--repo", dir, "--force"); code != 0 {
		t.Fatal("--force")
	}
	if !strings.Contains(errsOf(t, dir), "not a git repository") {
		t.Log("(warning about missing git repo is printed to stderr)")
	}
}

func errsOf(t *testing.T, dir string) string {
	_, _, e := exec(t, "", "init", "--repo", dir, "--force")
	return e
}

func TestVerifyAgentModeExitCodeAndJSON(t *testing.T) {
	repo := vipRepo(t)
	code, out, _ := exec(t, "", "verify", "--repo", repo, "-q")
	if code != exitNeedsFix || !strings.Contains(out, "NEEDS FIX") || !strings.Contains(out, "[escape-output]") {
		t.Fatalf("%d\n%s", code, out)
	}
	repo = vipRepo(t)
	code, out, _ = exec(t, "", "verify", "--repo", repo, "--json")
	if code != exitNeedsFix {
		t.Fatalf("%d\n%s", code, out)
	}
	var j map[string]any
	if err := json.Unmarshal([]byte(out), &j); err != nil {
		t.Fatalf("--json must emit pure JSON: %v\n%s", err, out)
	}
	if j["status"] != "needs_fix" || j["fix_request"] == nil || j["steps"] == nil {
		t.Fatalf("%v", j)
	}
}

func TestRunWithPatchFromStdin(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, filepath.Join(fixtures, "base")))
	patch := "diff --git a/inc/n.php b/inc/n.php\nnew file mode 100644\n--- /dev/null\n+++ b/inc/n.php\n@@ -0,0 +1,3 @@\n+<?php\n+$a = array( 1 );\n+\n"
	code, out, errs := exec(t, patch, "run", "--patch", "-", "--repo", repo)
	if code != 0 || !strings.Contains(out, "PASSED") || !strings.Contains(errs, "sandbox") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(testutil.ReadFile(t, repo, "inc/n.php"), "[ 1 ]") {
		t.Fatal("patch not applied/fixed")
	}
	if code, _, e := exec(t, "", "run", "--repo", repo); code != exitUsage || !strings.Contains(e, "--patch") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := exec(t, "garbage", "run", "--patch", "-", "--repo", repo, "-q"); code != exitUsage || !strings.Contains(e, "patch rejected") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := exec(t, "", "verify", "--patch", "x", "--repo", repo); code != exitUsage || !strings.Contains(e, "only valid") {
		t.Fatalf("%d %s", code, e)
	}
}

func TestFixArgumentsAndProviderErrors(t *testing.T) {
	repo := vipRepo(t)
	if code, _, e := exec(t, "", "fix", "--repo", repo); code != exitUsage || !strings.Contains(e, "usage: aegis fix") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := exec(t, "", "fix", "do it", "--repo", repo); code != exitUsage || !strings.Contains(e, "needs an LLM") {
		t.Fatalf("%d %s", code, e)
	}
	t.Setenv("ANTHROPIC_API_KEY", "")
	if code, _, e := exec(t, "", "fix", "do it", "--repo", repo, "--provider", "anthropic"); code != exitUsage || !strings.Contains(e, "ANTHROPIC_API_KEY") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := exec(t, "", "verify", "--repo", repo, "--provider", "bogus"); code != exitUsage || !strings.Contains(e, "llm.provider") {
		t.Fatalf("%d %s", code, e)
	}
	if code, _, e := exec(t, "", "verify", "--repo", t.TempDir()); code != exitUsage || !strings.Contains(e, "git repository") {
		t.Fatalf("%d %s", code, e)
	}
}

// Full end-to-end through the real binary entry point: a shell script plays the
// LLM (reads the prompt on stdin, answers with an aegis-replace block).
func TestEndToEndWithCommandProvider(t *testing.T) {
	repo := vipRepo(t)
	script := filepath.Join(t.TempDir(), "fake-llm.sh")
	body := `#!/bin/sh
prompt=$(cat)
range=$(printf '%s\n' "$prompt" | grep -m1 '^### inc/render.php lines' | sed -E 's/.*lines ([0-9]+-[0-9]+).*/\1/')
printf '%s\n' '` + "```" + `aegis-replace inc/render.php '"$range"
cat "$AEGIS_TEST_FIX"
printf '%s\n' '` + "```" + `'
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	fix, _ := filepath.Abs(filepath.Join(fixtures, "healed_function.txt"))
	t.Setenv("AEGIS_TEST_FIX", fix)

	cfg := testutil.ReadFile(t, repo, ".aegis.yml")
	cfg = strings.Replace(cfg, "provider: none", "provider: command\n  command: \""+script+"\"", 1)
	testutil.WriteFiles(t, repo, map[string]string{".aegis.yml": cfg})

	code, out, errs := exec(t, "", "verify", "--repo", repo)
	if code != 0 || !strings.Contains(out, "PASSED") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
	if !strings.Contains(errs, "heal attempt 1/3") {
		t.Fatalf("progress log missing:\n%s", errs)
	}
	got := testutil.ReadFile(t, repo, "inc/render.php")
	for _, want := range []string{"esc_html( $row->post_title )", "wp_cache_get(", "$wpdb->prepare("} {
		if !strings.Contains(got, want) {
			t.Fatalf("healed file lacks %q", want)
		}
	}
	if _, err := os.Stat(filepath.Join(repo, ".aegis-sandbox")); err == nil {
		t.Fatal("sandbox left behind")
	}
}

func TestFailedRunExitCodeAndClean(t *testing.T) {
	repo := vipRepo(t)
	cfg := testutil.ReadFile(t, repo, ".aegis.yml")
	cfg = strings.Replace(cfg, "provider: none", "provider: command\n  command: \"echo I give up\"", 1)
	testutil.WriteFiles(t, repo, map[string]string{".aegis.yml": cfg})
	code, out, _ := exec(t, "", "verify", "--repo", repo, "-q")
	if code != exitFailed || !strings.Contains(out, "FAILED") || !strings.Contains(out, "aegis clean") {
		t.Fatalf("%d\n%s", code, out)
	}
	code, out, _ = exec(t, "", "clean", "--list", "--repo", repo)
	if code != 0 || !strings.Contains(out, "aegis-fix-") {
		t.Fatalf("%d %s", code, out)
	}
	code, out, _ = exec(t, "", "clean", "--repo", repo)
	if code != 0 || !strings.Contains(out, "removed") {
		t.Fatalf("%d %s", code, out)
	}
	if _, out, _ = exec(t, "", "clean", "--repo", repo); !strings.Contains(out, "no AegisLoop sandboxes") {
		t.Fatal(out)
	}
	if br := testutil.Git(t, repo, "branch", "--list", "aegis-fix-*"); strings.TrimSpace(br) != "" {
		t.Fatal(br)
	}
}

func TestPruneCommand(t *testing.T) {
	var b strings.Builder
	b.WriteString("<?php\n")
	for i := 0; i < 100; i++ {
		b.WriteString("function f" + string(rune('a'+i%26)) + strings.Repeat("x", i) + "() {\n\treturn 1;\n}\n\n")
	}
	file := filepath.Join(t.TempDir(), "big.php")
	os.WriteFile(file, []byte(b.String()), 0o644)
	code, out, _ := exec(t, "", "prune", file+":52")
	if code != 0 || !strings.Contains(out, "(function)") || !strings.Contains(out, "fewer") {
		t.Fatalf("%d\n%s", code, out)
	}
	for _, bad := range [][]string{{}, {"nofile"}, {file + ":zero"}, {file + ":0"}, {"/no/such/file.php:3"}} {
		if code, _, _ := exec(t, "", append([]string{"prune"}, bad...)...); code != exitUsage {
			t.Errorf("%v should fail", bad)
		}
	}
}

func TestMCPCommandOverStdio(t *testing.T) {
	code, out, errs := exec(t, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`+"\n", "mcp")
	if code != 0 || !strings.Contains(out, "verify_and_heal_patch") || !strings.Contains(out, "run_spec_test_loop") {
		t.Fatalf("%d\n%s\n%s", code, out, errs)
	}
}
