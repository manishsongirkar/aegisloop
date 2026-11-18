package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestParseYAMLSubset(t *testing.T) {
	src := `
# comment
version: 1
name: "quoted: value"   # trailing comment
single: 'it''s'
list: [a, "b, c", 3]
flag: true
nested:
  key: value
  items:
    - one
    - two
steps:
  - name: first
    run: vendor/bin/phpcs --standard=X {files}
    extensions: [php]
  - name: second
    run: "echo # not a comment"
`
	got, err := ParseYAML(src)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"version": 1,
		"name":    "quoted: value",
		"single":  "it's",
		"list":    []any{"a", "b, c", 3},
		"flag":    true,
		"nested":  map[string]any{"key": "value", "items": []any{"one", "two"}},
		"steps": []any{
			map[string]any{"name": "first", "run": "vendor/bin/phpcs --standard=X {files}", "extensions": []any{"php"}},
			map[string]any{"name": "second", "run": "echo # not a comment"},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got  %#v\nwant %#v", got, want)
	}
}

func TestParseYAMLErrors(t *testing.T) {
	for name, src := range map[string]string{
		"tab indent":   "a:\n\tb: 1\n",
		"duplicate":    "a: 1\na: 2\n",
		"bad line":     "just text\n",
		"unterminated": "a: \"x\n",
		"bad list":     "a: [1, 2\n",
	} {
		if _, err := ParseYAML(src); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestDefaultsMatchSpec(t *testing.T) {
	c := Default()
	if c.MaxRetries != 3 || !c.StrictTabs || c.TokenBudgetAlert <= 0 {
		t.Fatalf("defaults: %+v", c)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestRenderParseRoundTrip(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "composer.json", `{"require-dev":{"phpunit/phpunit":"^9","wp-coding-standards/wpcs":"*"}}`)
	write(t, dir, "package.json", `{"scripts":{"build":"next build","test":"jest"},"devDependencies":{"eslint":"8","jest":"29","prettier":"3","typescript":"5","stylelint":"15"}}`)
	orig := Detect(dir).Config()
	orig.LLM = LLM{Provider: "command", Command: `claude -p --model "x y"`, MaxTokens: 1000, TimeoutSeconds: 30}
	orig.Guard.Disable = []string{"debug-leftover"}
	text := orig.Render()
	back, err := Parse(text)
	if err != nil {
		t.Fatalf("parse rendered config: %v\n%s", err, text)
	}
	back.Root = orig.Root
	if !reflect.DeepEqual(orig, back) {
		t.Fatalf("round trip differs\norig %+v\nback %+v\n%s", orig, back, text)
	}
}

func TestParseRejectsBadValues(t *testing.T) {
	for name, src := range map[string]string{
		"unknown key":       "bogus: 1\n",
		"bad retries":       "max_retries: 99\n",
		"bad on_failure":    "sandbox:\n  on_failure: explode\n",
		"sandbox escape":    "sandbox:\n  dir: ../out\n",
		"bad provider":      "llm:\n  provider: skynet\n",
		"command no cmd":    "llm:\n  provider: command\n",
		"builtin misplaced": "fixers:\n  - name: x\n    run: builtin:standards\n",
		"unknown builtin":   "checks:\n  - name: x\n    run: builtin:nope\n",
		"dup step":          "checks:\n  - name: a\n    run: echo\n  - name: a\n    run: echo\n",
		"missing run":       "checks:\n  - name: a\n",
		"unknown step key":  "checks:\n  - name: a\n    run: echo\n    colour: red\n",
		"type mismatch":     "max_retries: lots\n",
	} {
		if _, err := Parse(src); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestLoadMissingFileFallsBackToDetection(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "go.mod", "module x\n")
	c, found, err := Load(dir)
	if err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if c.Language != "go" || len(c.Checks) == 0 {
		t.Fatalf("unexpected config %+v", c)
	}
	write(t, dir, FileName, "max_retries: 5\n")
	c, found, err = Load(dir)
	if err != nil || !found || c.MaxRetries != 5 || c.Root != dir {
		t.Fatalf("loaded %+v found=%v err=%v", c, found, err)
	}
	write(t, dir, FileName, "max_retries: [\n")
	if _, _, err = Load(dir); err == nil || !strings.Contains(err.Error(), FileName) {
		t.Fatalf("error should name the file: %v", err)
	}
}

func names(steps []Step) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Name)
	}
	return out
}

func TestDetectWordPressVIP(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "composer.json", `{"require-dev":{"automattic/vipwpcs":"^3","phpunit/phpunit":"^9"},"name":"a/b"}`)
	write(t, dir, ".phpcs.xml", `<ruleset><rule ref="WordPress-VIP-Go"/></ruleset>`)
	d := Detect(dir)
	if !d.PHP || !d.WordPress {
		t.Fatalf("detection: %+v", d)
	}
	c := d.Config()
	if !c.StrictTabs || !c.StrictShortArrays {
		t.Fatal("PHP projects enforce tabs and short arrays by default")
	}
	if got := names(c.Fixers); !reflect.DeepEqual(got, []string{"tabs", "short-arrays", "phpcbf"}) {
		t.Fatalf("fixers %v", got)
	}
	if got := names(c.Checks); !reflect.DeepEqual(got, []string{"standards", "php-lint", "phpcs", "phpunit"}) {
		t.Fatalf("checks %v", got)
	}
	var phpcbf Step
	for _, s := range c.Fixers {
		if s.Name == "phpcbf" {
			phpcbf = s
		}
	}
	if !strings.Contains(phpcbf.Run, "--standard=.phpcs.xml") || !reflect.DeepEqual(phpcbf.OkExitCodes, []int{0, 1}) {
		t.Fatalf("phpcbf step %+v", phpcbf)
	}
	if !reflect.DeepEqual(c.Sandbox.Link, []string{"vendor"}) {
		t.Fatalf("link %v", c.Sandbox.Link)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDetectNextJS(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"scripts":{"build":"next build","test":"jest"},"dependencies":{"next":"14"},"devDependencies":{"eslint":"8","jest":"29","typescript":"5"}}`)
	write(t, dir, "tsconfig.json", "{}")
	c := Detect(dir).Config()
	if c.StrictTabs {
		t.Fatal("tabs must not be forced on a non-PHP project")
	}
	if got := names(c.Checks); !reflect.DeepEqual(got, []string{"standards", "eslint", "tsc", "jest", "build"}) {
		t.Fatalf("checks %v", got)
	}
	if got := names(c.Fixers); !reflect.DeepEqual(got, []string{"eslint-fix"}) {
		t.Fatalf("fixers %v", got)
	}
	if !reflect.DeepEqual(c.Sandbox.Link, []string{"node_modules"}) {
		t.Fatalf("link %v", c.Sandbox.Link)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDetectNothing(t *testing.T) {
	c := Detect(t.TempDir()).Config()
	if len(c.Fixers)+len(c.Checks) != 0 || c.Language != "auto" {
		t.Fatalf("%+v", c)
	}
	if _, err := Parse(c.Render()); err != nil {
		t.Fatalf("empty config must render to valid YAML: %v\n%s", err, c.Render())
	}
}
