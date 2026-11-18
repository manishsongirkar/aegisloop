package runner

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manishsongirkar/aegisloop/internal/config"
	"github.com/manishsongirkar/aegisloop/internal/tokenizer"
)

func TestSplitCommand(t *testing.T) {
	got, err := SplitCommand(`phpcs --standard="WordPress VIP" 'a b' c\ d {files}`)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"phpcs", "--standard=WordPress VIP", "a b", "c d", "{files}"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%q", got)
	}
	if _, err := SplitCommand(`echo "oops`); err == nil {
		t.Fatal("unterminated quote must fail")
	}
	if got, _ := SplitCommand(`a "" b`); !reflect.DeepEqual(got, []string{"a", "", "b"}) {
		t.Fatalf("empty quoted arg lost: %q", got)
	}
}

func TestFixTabs(t *testing.T) {
	src := "<?php\nfunction f() {\n    if ( $a ) {\n        return 1;\n    }\n    /**\n     * doc\n     */\n\t  $mixed = 1;\n}\n"
	got := FixTabs(src, tokenizer.LangPHP, nil)
	want := "<?php\nfunction f() {\n\tif ( $a ) {\n\t\treturn 1;\n\t}\n\t/**\n\t * doc\n\t */\n\t  $mixed = 1;\n}\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	if FixTabs(got, tokenizer.LangPHP, nil) != got {
		t.Fatal("not idempotent")
	}
	// only selected lines
	only3 := FixTabs(src, tokenizer.LangPHP, func(n int) bool { return n == 3 })
	if !strings.Contains(only3, "\tif ( $a ) {") || !strings.Contains(only3, "        return 1;") {
		t.Fatalf("line filter ignored:\n%s", only3)
	}
	// CRLF preserved
	crlf := FixTabs("<?php\r\n    $a = 1;\r\n", tokenizer.LangPHP, nil)
	if crlf != "<?php\r\n\t$a = 1;\r\n" {
		t.Fatalf("%q", crlf)
	}
	// heredoc / multi-line string content is data and must not be touched
	hd := "<?php\n$s = <<<EOT\n    keep me\nEOT;\n$t = 'a\n    b';\n"
	if FixTabs(hd, tokenizer.LangPHP, nil) != hd {
		t.Fatal("string/heredoc whitespace was modified")
	}
	js := "const a = `x\n    y`;\nfunction f() {\n    return 1;\n}\n"
	if got := FixTabs(js, tokenizer.LangJS, nil); !strings.Contains(got, "    y`") || !strings.Contains(got, "\treturn 1;") {
		t.Fatalf("js template literal handling:\n%s", got)
	}
}

func TestShortArrays(t *testing.T) {
	cases := map[string]string{
		"$a = array( 1, 2 );":                                             "$a = [ 1, 2 ];",
		"$a = array();":                                                   "$a = [];",
		"$a = ARRAY ( 'x' => array( 'y' ) );":                             "$a = [ 'x' => [ 'y' ] ];",
		"$a = array(array(1), array(2, array(3)));":                       "$a = [[1], [2, [3]]];",
		"foo( array( 'a' => 1 ), $b );":                                   "foo( [ 'a' => 1 ], $b );",
		"$a = array( 'k' => foo( 1 ) );":                                  "$a = [ 'k' => foo( 1 ) ];",
		"function f( array $x ) { return (array) $x; }":                   "function f( array $x ) { return (array) $x; }",
		"$a = is_array( $x ) || array_map( $f, $x );":                     "$a = is_array( $x ) || array_map( $f, $x );",
		"$a = $this->array( 1 ) . Foo::array( 2 );":                       "$a = $this->array( 1 ) . Foo::array( 2 );",
		"$s = 'array(1)'; // array(2)\n$b = \"array(3)\"; /* array(4) */": "$s = 'array(1)'; // array(2)\n$b = \"array(3)\"; /* array(4) */",
		"$a = array(\n\t1,\n\t2,\n);":                                     "$a = [\n\t1,\n\t2,\n];",
	}
	for in, want := range cases {
		got := FixShortArrays("<?php\n"+in+"\n", nil)
		if got != "<?php\n"+want+"\n" {
			t.Errorf("in:   %q\ngot:  %q\nwant: %q", in, got, want)
		}
		if strings.Count(got, "\n") != strings.Count("<?php\n"+in+"\n", "\n") {
			t.Errorf("line count changed for %q", in)
		}
	}
	// html outside php tags is untouched
	if got := FixShortArrays("<p>array( 1 )</p>\n<?php $a = array( 1 ); ?>\n", nil); got != "<p>array( 1 )</p>\n<?php $a = [ 1 ]; ?>\n" {
		t.Fatalf("%q", got)
	}
	// line filter: only arrays whose keyword sits on a selected line
	src := "<?php\n$a = array( 1 );\n$b = array( 2 );\n"
	if got := FixShortArrays(src, func(n int) bool { return n == 3 }); got != "<?php\n$a = array( 1 );\n$b = [ 2 ];\n" {
		t.Fatalf("%q", got)
	}
	if got := FixShortArrays("<?php\n$a = [ 1 ];\n", nil); got != "<?php\n$a = [ 1 ];\n" {
		t.Fatal("already short arrays must be untouched")
	}
}

func guardCfg() *config.Config {
	c := config.Default()
	c.StrictTabs, c.StrictShortArrays = true, true
	return c
}

func runGuard(t *testing.T, name, src string, cfg *config.Config) []tokenizer.Failure {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fails, err := CheckStandards(dir, Scope{Files: []string{name}}, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return fails
}

func rules(fs []tokenizer.Failure) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Rule)
	}
	return out
}

func TestGuardPHPViolations(t *testing.T) {
	cases := []struct {
		name, code string
		rule       string // expected rule, "" = must be clean
	}{
		{"unescaped var", "echo $title;", "escape-output"},
		{"unescaped concat", "echo '<li>' . $row->post_title . '</li>';", "escape-output"},
		{"unescaped call", "echo get_the_title();", "escape-output"},
		{"interpolated string", `echo "Hello $name";`, "escape-output"},
		{"short echo", "<h1><?= $title ?></h1>", "escape-output"},
		{"print", "print $x;", "escape-output"},
		{"escaped", "echo esc_html( $title );", ""},
		{"escaped concat", "echo '<li>' . esc_html( $a ) . esc_attr( $b ) . '</li>';", ""},
		{"kses", "echo wp_kses_post( $content );", ""},
		{"int cast", "echo (int) $count;", ""},
		{"absint", "echo absint( $count );", ""},
		{"literal only", "echo 'static ' . PHP_EOL;", ""},
		{"escaper partial", "echo esc_html( $a ) . $b;", "escape-output"},
		{"nested esc", "echo esc_html( sprintf( '%s', $a ) );", ""},
		{"echo list", "echo esc_html( $a ), $b;", "escape-output"},
		{"ignore marker", "echo $x; // phpcs:ignore WordPress.Security.EscapeOutput", ""},
		{"echo in string", `$s = 'echo $x;';`, ""},
		{"echo in comment", "// echo $x;", ""},
		{"unprepared", `$wpdb->query( "DELETE FROM t WHERE id = $id" );`, "db-unprepared"},
		{"prepared", `$wpdb->query( $wpdb->prepare( "DELETE FROM t WHERE id = %d", $id ) );`, ""},
		{"literal sql", `$wpdb->query( "TRUNCATE TABLE t" );`, ""},
		{"uncached read", `$r = $wpdb->get_results( "SELECT 1" );`, "db-uncached"},
		{"cached read", "if ( false === ( $r = wp_cache_get( 'k' ) ) ) {\n\t$r = $wpdb->get_results( \"SELECT 1\" );\n\twp_cache_set( 'k', $r );\n}", ""},
		{"transient read", "$r = get_transient( 'k' ); $r = $wpdb->get_var( \"SELECT 1\" );", ""},
		{"raw superglobal", "$x = $_POST['x'];", "unsanitized-input"},
		{"sanitized", "$x = sanitize_text_field( wp_unslash( $_POST['x'] ) );", ""},
		{"isset only", "if ( isset( $_GET['x'] ) ) {}", ""},
		{"eval", "eval( $code );", "forbidden-function"},
		{"extract", "extract( $args );", "forbidden-function"},
		{"method named exec", "$this->exec( 1 ); Foo::system( 2 );", ""},
		{"var_dump", "var_dump( $x );", "debug-leftover"},
		{"print_r return", "$s = print_r( $x, true );", ""},
		{"long array", "$a = array( 1 );", "short-arrays"},
		{"secret", `$key = 'AKIAABCDEFGHIJKLMNOP';`, "hardcoded-secret"},
		{"secret env", `$key = getenv( 'API_KEY_ABCDEFGHIJKLMNOP' );`, ""},
		{"password literal", `$cfg = [ 'password' => 'hunter2hunter2hunter2' ];`, "hardcoded-secret"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := "<?php\n" + c.code + "\n"
			if strings.HasPrefix(c.code, "<h1>") {
				src = c.code + "\n"
			}
			got := rules(runGuard(t, "a.php", src, guardCfg()))
			if c.rule == "" && len(got) != 0 {
				t.Fatalf("expected clean, got %v", got)
			}
			if c.rule != "" && (len(got) == 0 || got[0] != c.rule) {
				t.Fatalf("expected %s, got %v", c.rule, got)
			}
		})
	}
}

func TestGuardTabsAndScope(t *testing.T) {
	src := "<?php\nfunction f() {\n    $a = 1;\n\t$b = 2;\n    $c = 3;\n}\n"
	got := runGuard(t, "a.php", src, guardCfg())
	if len(got) != 2 || got[0].Line != 3 || got[1].Line != 5 || got[0].Rule != "tabs" {
		t.Fatalf("%+v", got)
	}
	// only added/changed lines are judged
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.php"), []byte(src), 0o644)
	fails, _ := CheckStandards(dir, Scope{Files: []string{"a.php"}, Lines: map[string]map[int]bool{"a.php": {5: true}}}, guardCfg())
	if len(fails) != 1 || fails[0].Line != 5 {
		t.Fatalf("scope ignored: %+v", fails)
	}
	// tabs inside heredoc are data
	hd := "<?php\n$s = <<<EOT\n    indented data\nEOT;\n"
	if got := runGuard(t, "a.php", hd, guardCfg()); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
	// switches
	c := guardCfg()
	c.StrictTabs = false
	if got := runGuard(t, "a.php", src, c); len(got) != 0 {
		t.Fatalf("strict_tabs=false must disable the tabs rule: %+v", got)
	}
	c = guardCfg()
	c.Guard.Disable = []string{"tabs"}
	if got := runGuard(t, "a.php", src, c); len(got) != 0 {
		t.Fatal("guard.disable ignored")
	}
	c = guardCfg()
	c.Guard.Enabled = false
	if got := runGuard(t, "a.php", "<?php\neval($x);\n", c); len(got) != 0 {
		t.Fatal("guard.enabled=false ignored")
	}
}

func TestGuardCacheLookupIsPerFunction(t *testing.T) {
	src := `<?php
function cached() {
	$r = wp_cache_get( 'k' );
	return $r;
}

function uncached() {
	global $wpdb;
	return $wpdb->get_row( "SELECT 1" );
}
`
	got := runGuard(t, "a.php", src, guardCfg())
	if len(got) != 1 || got[0].Rule != "db-uncached" || got[0].Line != 9 {
		t.Fatalf("a cache call in another function must not excuse this query: %+v", got)
	}
}

func TestGuardJS(t *testing.T) {
	cases := map[string]string{
		"eval( s );":                  "no-eval",
		"new Function( 'return 1' );": "no-eval",
		"el.innerHTML = html;":        "xss-innerhtml",
		"<div dangerouslySetInnerHTML={{ __html: h }} />;": "xss-innerhtml",
		"document.write( x );":                             "xss-innerhtml",
		"console.log( x );":                                "debug-leftover",
		"debugger;":                                        "debug-leftover",
		"el.textContent = x;":                              "",
		"// eval( x ) console.log( 1 )":                    "",
		"const s = 'el.innerHTML = x';":                    "",
	}
	for code, want := range cases {
		got := rules(runGuard(t, "a.js", code+"\n", guardCfg()))
		if want == "" && len(got) != 0 || want != "" && (len(got) == 0 || got[0] != want) {
			t.Errorf("%q: want %q got %v", code, want, got)
		}
	}
}

func TestStandardsText(t *testing.T) {
	txt := strings.Join(StandardsText(guardCfg()), "\n")
	for _, want := range []string{"tabs", "short array", "escaped", "prepare", "cached"} {
		if !strings.Contains(strings.ToLower(txt), want) {
			t.Errorf("standards text lacks %q:\n%s", want, txt)
		}
	}
	c := guardCfg()
	c.Guard.Enabled = false
	if StandardsText(c) != nil {
		t.Fatal("disabled guard has no standards")
	}
}

// ---- runner with fake tools ---------------------------------------------------

func fakeProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "bin"), 0o755)
	script := func(name, body string) {
		p := filepath.Join(dir, "bin", name)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// fixer: upper-cases the word "bad" in every file it is given; exits 1 when it changed something
	script("fixer", `changed=0
for f in "$@"; do
  if grep -q bad "$f"; then sed -i.bak 's/bad/good/g' "$f" && rm -f "$f.bak"; changed=1; fi
done
exit $changed
`)
	// linter: fails (emacs format) for every line containing "BAD"
	script("lint", `rc=0
for f in "$@"; do
  n=0
  while IFS= read -r line; do
    n=$((n+1))
    case "$line" in *BAD*) echo "$f:$n:1: error - found BAD ($n)"; rc=1;; esac
  done < "$f"
done
exit $rc
`)
	script("slow", "sleep 5\n")
	script("noisy", "echo 'FAIL boom'; echo 'stderr detail' >&2; exit 3\n")
	script("each", `echo "checking $1"; case "$1" in *broken*) echo "$1:1:1: syntax error"; exit 1;; esac
`)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("this is bad\nBAD line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRunnerFixersThenChecks(t *testing.T) {
	dir := fakeProject(t)
	cfg := config.Default()
	cfg.Fixers = []config.Step{{Name: "fixer", Run: "./bin/fixer {files}", Extensions: []string{"txt"}, OkExitCodes: []int{0, 1}}}
	cfg.Checks = []config.Step{
		{Name: "lint", Run: "./bin/lint {files}", Kind: "lint", Extensions: []string{"txt"}},
		{Name: "nofiles", Run: "./bin/lint {files}", Extensions: []string{"php"}},
		{Name: "missing", Run: "does-not-exist-xyz {files}", Extensions: []string{"txt"}},
	}
	r := &Runner{Cfg: cfg, Dir: dir}
	rep, err := r.Run(context.Background(), Scope{Files: []string{"a.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Fixers[0].Status != StatusFixed || !reflect.DeepEqual(rep.FixedFiles, []string{"a.txt"}) {
		t.Fatalf("fixer: %+v", rep.Fixers[0])
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "a.txt")); !strings.Contains(string(b), "this is good") {
		t.Fatalf("fixer did not write: %q", b)
	}
	if rep.Passed {
		t.Fatal("lint must fail on BAD")
	}
	lint := rep.Checks[0]
	if lint.Status != StatusFailed || len(lint.Failures) != 1 || lint.Failures[0].File != "a.txt" || lint.Failures[0].Line != 2 {
		t.Fatalf("lint: %+v", lint)
	}
	if rep.Checks[1].Status != StatusSkipped || !strings.Contains(rep.Checks[1].Note, "no matching") {
		t.Fatalf("%+v", rep.Checks[1])
	}
	if rep.Checks[2].Status != StatusSkipped || !strings.Contains(rep.Checks[2].Note, "tool not found") {
		t.Fatalf("a missing optional tool must be skipped: %+v", rep.Checks[2])
	}
	if len(rep.Failures) != 1 {
		t.Fatalf("aggregate failures: %+v", rep.Failures)
	}
	// second pass: the fixer now changes nothing
	rep, _ = r.Run(context.Background(), Scope{Files: []string{"a.txt"}})
	if rep.Fixers[0].Status != StatusClean || len(rep.FixedFiles) != 0 {
		t.Fatalf("%+v", rep.Fixers[0])
	}
}

func TestRunnerRequiredToolMissingFails(t *testing.T) {
	cfg := config.Default()
	cfg.Checks = []config.Step{{Name: "must", Run: "definitely-not-installed-xyz", Required: true}}
	rep, err := (&Runner{Cfg: cfg, Dir: t.TempDir()}).Run(context.Background(), Scope{})
	if err != nil || rep.Passed || rep.Checks[0].Exit != 127 {
		t.Fatalf("%+v %v", rep, err)
	}
}

func TestRunnerTimeoutAndExitCodes(t *testing.T) {
	dir := fakeProject(t)
	cfg := config.Default()
	cfg.TimeoutSeconds = 1
	cfg.Checks = []config.Step{
		{Name: "slow", Run: "./bin/slow"},
		{Name: "noisy", Run: "./bin/noisy"},
		{Name: "tolerated", Run: "./bin/noisy", OkExitCodes: []int{3}},
	}
	rep, err := (&Runner{Cfg: cfg, Dir: dir}).Run(context.Background(), Scope{})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Checks[0].Status != StatusFailed || !strings.Contains(rep.Checks[0].Note, "timed out") {
		t.Fatalf("%+v", rep.Checks[0])
	}
	if rep.Checks[0].Duration.Seconds() > 4 {
		t.Fatalf("timeout did not kill the process: %v", rep.Checks[0].Duration)
	}
	if rep.Checks[1].Status != StatusFailed || rep.Checks[1].Exit != 3 || !strings.Contains(rep.Checks[1].Raw, "stderr detail") {
		t.Fatalf("%+v", rep.Checks[1])
	}
	if rep.Checks[2].Status != StatusPassed {
		t.Fatalf("ok_exit_codes ignored: %+v", rep.Checks[2])
	}
}

func TestRunnerPerFilePlaceholder(t *testing.T) {
	dir := fakeProject(t)
	os.WriteFile(filepath.Join(dir, "ok.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, "broken.txt"), []byte("x"), 0o644)
	cfg := config.Default()
	cfg.Checks = []config.Step{{Name: "each", Run: "./bin/each {file}", Extensions: []string{"txt"}}}
	rep, _ := (&Runner{Cfg: cfg, Dir: dir}).Run(context.Background(), Scope{Files: []string{"ok.txt", "broken.txt", "gone.txt"}})
	c := rep.Checks[0]
	if c.Status != StatusFailed || !strings.Contains(c.Raw, "checking ok.txt") || !strings.Contains(c.Raw, "checking broken.txt") || strings.Contains(c.Raw, "gone.txt") {
		t.Fatalf("%+v", c)
	}
	if len(c.Failures) != 1 || c.Failures[0].File != "broken.txt" {
		t.Fatalf("%+v", c.Failures)
	}
}

func TestRunnerBuiltinsAndLineScope(t *testing.T) {
	dir := t.TempDir()
	src := "<?php\nfunction f() {\n    $a = array( 1 );\n    $b = array( 2 );\n}\n"
	os.WriteFile(filepath.Join(dir, "a.php"), []byte(src), 0o644)
	cfg := guardCfg()
	cfg.Fixers = []config.Step{
		{Name: "tabs", Run: "builtin:tabs", Extensions: []string{"php"}},
		{Name: "arrays", Run: "builtin:short-arrays", Extensions: []string{"php"}},
	}
	cfg.Checks = []config.Step{{Name: "standards", Run: "builtin:standards", Extensions: []string{"php"}}}
	scope := Scope{Files: []string{"a.php"}, Lines: map[string]map[int]bool{"a.php": {3: true}}}
	rep, err := (&Runner{Cfg: cfg, Dir: dir}).Run(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(dir, "a.php"))
	want := "<?php\nfunction f() {\n\t$a = [ 1 ];\n    $b = array( 2 );\n}\n"
	if string(b) != want {
		t.Fatalf("only the in-scope line may be rewritten:\n%q", b)
	}
	if !rep.Passed || !reflect.DeepEqual(rep.FixedFiles, []string{"a.php"}) {
		t.Fatalf("%+v", rep)
	}
}
