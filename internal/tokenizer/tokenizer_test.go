package tokenizer

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestMaskPreservesLengthAndBlanksNoise(t *testing.T) {
	src := "<html>array(1)</html>\n<?php\n$a = 'array(x)'; // array(y)\n/* array(z) */\n$b = \"hi $name\";\necho <<<EOT\n  array(q)\nEOT;\n?>\ntail array(2)\n"
	m := string(Mask(src, LangPHP))
	if len(m) != len(src) {
		t.Fatalf("length changed: %d vs %d", len(m), len(src))
	}
	if strings.Count(m, "\n") != strings.Count(src, "\n") {
		t.Fatal("newlines must be preserved")
	}
	if strings.Contains(m, "array") {
		t.Fatalf("masked source still contains 'array':\n%s", m)
	}
	if !strings.Contains(m, "$a = '") || !strings.Contains(m, "echo <<<EOT") {
		t.Fatalf("code and delimiters must survive:\n%s", m)
	}
}

func TestMaskWithStringsReportsMultilineStrings(t *testing.T) {
	src := "<?php\n$x = 'a\n    b';\n$y = 1;\n"
	_, prot := MaskWithStrings(src, LangPHP)
	if !prot[3] || prot[2] || prot[4] {
		t.Fatalf("only line 3 starts inside a string: %v", prot)
	}
}

func TestSliceGo(t *testing.T) {
	src := "package p\n\nimport \"fmt\"\n\n// Add adds.\nfunc Add(a, b int) int {\n\treturn a + b\n}\n\nfunc Other() {\n\tfmt.Println(\"x\")\n}\n"
	sl := SliceFile("p.go", src, []int{7}, SliceOptions{})
	if len(sl) != 1 {
		t.Fatalf("%d slices", len(sl))
	}
	s := sl[0]
	if s.Start != 5 || s.End != 8 || s.Kind != "function" || !strings.Contains(s.Signature, "func Add(a, b int) int") {
		t.Fatalf("%+v", s)
	}
	if strings.Contains(s.Text, "Other") {
		t.Fatal("slice leaked another function")
	}
}

func TestSlicePHPMethodInsideClass(t *testing.T) {
	src := `<?php
class Foo {
	public $x = 1;

	/**
	 * Doc.
	 */
	public function bar( $a,
		$b ) {
		if ( $a ) {
			return $b;
		}
		return 0;
	}

	public function baz() {
		return 2;
	}
}
`
	sl := SliceFile("foo.php", src, []int{11}, SliceOptions{})
	if len(sl) != 1 {
		t.Fatalf("%d slices", len(sl))
	}
	s := sl[0]
	if s.Kind != "function" || s.Start != 5 || s.End != 14 {
		t.Fatalf("expected docblock+method (5-14), got %+v", s)
	}
	if !strings.Contains(s.Signature, "function bar( $a, $b )") {
		t.Fatalf("signature %q", s.Signature)
	}
	if strings.Contains(s.Text, "baz") {
		t.Fatal("must not include sibling method")
	}
	// property line: class-level, falls back to a window, never the whole class
	sl = SliceFile("foo.php", src, []int{3}, SliceOptions{ContextLines: 2})
	if sl[0].Kind != "window" || sl[0].End-sl[0].Start > 5 {
		t.Fatalf("%+v", sl[0])
	}
}

func TestSliceJS(t *testing.T) {
	src := `import x from 'y';

export function a() {
  return 1;
}

const b = async (n) => {
  if (n) {
    return n; // error here
  }
  return 0;
};

class K {
  m() {
    return 2;
  }
}
`
	s := SliceFile("a.ts", src, []int{9}, SliceOptions{})[0]
	if s.Kind != "function" || s.Start != 7 || s.End != 12 {
		t.Fatalf("arrow fn: %+v", s)
	}
	s = SliceFile("a.js", src, []int{15}, SliceOptions{})[0]
	if s.Kind != "function" || !strings.HasPrefix(s.Signature, "m()") {
		t.Fatalf("method: %+v", s)
	}
}

func TestSliceCSSRule(t *testing.T) {
	src := ".a {\n  color: red;\n}\n\n.b {\n  color: blue;\n  margin: 0\n}\n"
	s := SliceFile("a.css", src, []int{7}, SliceOptions{})[0]
	if s.Start != 5 || s.End != 8 || s.Kind != "rule" {
		t.Fatalf("%+v", s)
	}
}

func TestSliceLongFunctionIsElidedAroundError(t *testing.T) {
	var b strings.Builder
	b.WriteString("<?php\nfunction big() {\n")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&b, "\t$v%d = %d;\n", i, i)
	}
	b.WriteString("}\n")
	s := SliceFile("big.php", b.String(), []int{200}, SliceOptions{ContextLines: 5, MaxLines: 50})[0]
	if !s.Elided || s.Kind != "function" {
		t.Fatalf("%+v", s)
	}
	n := s.Numbered()
	if !strings.Contains(n, "function big()") || !strings.Contains(n, "lines omitted") || !strings.Contains(n, "$v199 = 199;") {
		t.Fatalf("elided slice must keep the signature, a marker and the error window:\n%s", n)
	}
	if strings.Contains(n, "$v10 =") || strings.Count(n, "\n") > 30 {
		t.Fatalf("too much code kept:\n%s", n)
	}
}

func TestSliceMergesOverlappingErrors(t *testing.T) {
	src := "<?php\nfunction a() {\n\t$x = 1;\n\t$y = 2;\n}\n"
	sl := SliceFile("a.php", src, []int{3, 4, 3}, SliceOptions{})
	if len(sl) != 1 || len(sl[0].ErrLines) != 2 {
		t.Fatalf("%+v", sl)
	}
}

func TestSliceClampsOutOfRangeLines(t *testing.T) {
	sl := SliceFile("a.php", "<?php\n$x = 1;\n", []int{0, 999}, SliceOptions{})
	if len(sl) == 0 {
		t.Fatal("expected slices")
	}
	if got := SliceFile("a.php", "", []int{1}, SliceOptions{}); got != nil {
		t.Fatal("empty file yields no slices")
	}
}

// The headline requirement: surgical slicing must cut at least 70% of the
// tokens of an artificially bloated file (here it cuts >95%).
func TestPrunerTokenReductionOnBloatedFile(t *testing.T) {
	var b strings.Builder
	b.WriteString("<?php\n/**\n * Big plugin file.\n */\n\n")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "/**\n * Helper %d.\n */\nfunction acme_helper_%d( $input ) {\n\t$value = absint( $input );\n\tif ( $value > %d ) {\n\t\treturn $value * 2;\n\t}\n\t// padding padding padding padding padding\n\treturn $value + %d;\n}\n\n", i, i, i, i)
	}
	src := b.String()
	// find the line of the function 150 body
	lines := strings.Split(src, "\n")
	target := 0
	for i, l := range lines {
		if strings.Contains(l, "function acme_helper_150(") {
			target = i + 2 // the $value line
		}
	}
	if target == 0 {
		t.Fatal("fixture broken")
	}
	sl := SliceFile("big.php", src, []int{target}, SliceOptions{})
	if len(sl) != 1 || !strings.Contains(sl[0].Text, "acme_helper_150") {
		t.Fatalf("wrong slice: %+v", sl)
	}
	full := EstimateTokens(src)
	cut := EstimateTokens(sl[0].Numbered())
	reduction := 100 * (1 - float64(cut)/float64(full))
	t.Logf("whole file ≈%d tokens, slice ≈%d tokens (%.1f%% fewer)", full, cut, reduction)
	if reduction < 70 {
		t.Fatalf("token reduction %.1f%% is below the 70%% requirement", reduction)
	}
}

const phpunitNoise = `PHPUnit 9.6.13 by Sebastian Bergmann and contributors.

Runtime:       PHP 8.2.10
Configuration: /app/phpunit.xml

..F.....E.                                                          10 / 10 (100%)

Time: 00:00.123, Memory: 10.00 MB

There was 1 failure:

1) Acme\Tests\RenderTest::test_title
Failed asserting that two strings are equal.
--- Expected
+++ Actual
@@ @@
-'<li>Hello</li>'
+'<li>Hi</li>'

/app/tests/RenderTest.php:23

#0 /app/vendor/phpunit/phpunit/src/Framework/TestCase.php(1234): PHPUnit\Framework\Assert::assertSame()
#1 /app/vendor/phpunit/phpunit/src/Framework/TestCase.php(1244): PHPUnit\Framework\TestCase->runTest()
#2 /app/vendor/phpunit/phpunit/src/Framework/TestResult.php(7): PHPUnit\Framework\TestCase->runBare()
#3 /app/vendor/phpunit/phpunit/src/Framework/TestSuite.php(9): PHPUnit\Framework\TestResult->run()
#4 /app/vendor/phpunit/phpunit/src/TextUI/TestRunner.php(5): PHPUnit\Framework\TestSuite->run()
#5 /app/vendor/phpunit/phpunit/src/TextUI/Command.php(8): PHPUnit\TextUI\TestRunner->run()
#6 /app/vendor/phpunit/phpunit/phpunit(61): PHPUnit\TextUI\Command::main()
#7 {main}

FAILURES!
Tests: 10, Assertions: 12, Failures: 1.
`

func TestStripNoiseKeepsSignalDropsVendor(t *testing.T) {
	raw := phpunitNoise + strings.Repeat("#9 /app/vendor/x/y.php(1): z()\n", 200)
	st := StripNoise(raw, StripOptions{Root: "/app"})
	for _, bad := range []string{"vendor/phpunit", "Sebastian", "Runtime:", "Time: 00", "{main}", "..F....."} {
		if strings.Contains(st.Text, bad) {
			t.Errorf("noise %q survived:\n%s", bad, st.Text)
		}
	}
	for _, good := range []string{"Failed asserting that two strings are equal", "-'<li>Hello</li>'", "+'<li>Hi</li>'", "tests/RenderTest.php:23", "framework/vendor frame"} {
		if !strings.Contains(st.Text, good) {
			t.Errorf("signal %q was lost:\n%s", good, st.Text)
		}
	}
	if st.ReductionPercent() < 80 {
		t.Fatalf("reduction %.1f%% < 80%%", st.ReductionPercent())
	}
	if st.DroppedFrames < 200 {
		t.Fatalf("dropped frames = %d", st.DroppedFrames)
	}
}

func TestStripNoiseNodeAndCaps(t *testing.T) {
	raw := "\x1b[31m● Suite › adds\x1b[0m\n\n  expect(received).toBe(expected)\n\n  Expected: 3\n  Received: 4\n\n" +
		"      at Object.<anonymous> (src/add.test.js:5:19)\n" +
		"      at Promise.then.completed (node_modules/jest-circus/build/utils.js:298:28)\n" +
		"      at new Promise (<anonymous>)\n" +
		"      at processTicksAndRejections (node:internal/process/task_queues:95:5)\n" +
		"npm ERR! code ELIFECYCLE\n"
	st := StripNoise(raw, StripOptions{})
	if strings.Contains(st.Text, "node_modules") || strings.Contains(st.Text, "\x1b") || strings.Contains(st.Text, "ELIFECYCLE") {
		t.Fatalf("noise survived:\n%s", st.Text)
	}
	if !strings.Contains(st.Text, "Expected: 3") || !strings.Contains(st.Text, "src/add.test.js:5:19") {
		t.Fatalf("signal lost:\n%s", st.Text)
	}
	long := strings.Repeat("distinct line\n", 1)
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "error number %d\n", i)
	}
	capped := StripNoise(long+b.String(), StripOptions{MaxLines: 20})
	if n := strings.Count(capped.Text, "\n") + 1; n > 21 || !strings.Contains(capped.Text, "omitted") {
		t.Fatalf("cap not applied (%d lines)", n)
	}
	dup := StripNoise(strings.Repeat("same\n", 50), StripOptions{})
	if !strings.Contains(dup.Text, "(×50)") || strings.Count(dup.Text, "\n") != 0 {
		t.Fatalf("duplicates not collapsed: %q", dup.Text)
	}
}

func TestExtractFailuresFormats(t *testing.T) {
	cases := []struct {
		name, out string
		want      Failure
	}{
		{"phpcs emacs", "/r/src/a.php:12:5: error - Missing escaping (WordPress.Security.EscapeOutput.OutputNotEscaped)\n", Failure{File: "src/a.php", Line: 12, Col: 5}},
		{"eslint unix", "/r/src/a.js:3:1: Unexpected var, use let or const. [Error/no-var]\n", Failure{File: "src/a.js", Line: 3, Col: 1, Rule: "Error/no-var"}},
		{"go vet", "# pkg\n./main.go:10:2: undefined: foo\n", Failure{File: "main.go", Line: 10, Col: 2}},
		{"tsc", "src/a.ts(7,3): error TS2322: Type 'string' is not assignable to type 'number'.\n", Failure{File: "src/a.ts", Line: 7, Col: 3}},
		{"php parse", "PHP Parse error:  syntax error, unexpected '}' in /r/src/a.php on line 9\n", Failure{File: "src/a.php", Line: 9}},
		{"php -l", "Parse error: syntax error, unexpected end of file in /r/a.php on line 4\n", Failure{File: "a.php", Line: 4}},
		{"phpunit", "1) FooTest::test_x\nFailed asserting that false is true.\n\n/r/tests/FooTest.php:23\n", Failure{File: "tests/FooTest.php", Line: 23}},
		{"jest", "  ● Suite › adds\n\n    at Object.<anonymous> (/r/src/add.test.js:5:19)\n", Failure{File: "src/add.test.js", Line: 5, Col: 19}},
		{"eslint stylish", "/r/src/a.js\n  12:5  error  'x' is defined but never used  no-unused-vars\n", Failure{File: "src/a.js", Line: 12, Col: 5, Rule: "no-unused-vars"}},
		{"php frame", "1) FooTest::test_x\n#0 /r/src/Foo.php(42): Foo->bar()\n", Failure{File: "src/Foo.php", Line: 42}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ExtractFailures("s", c.out, "/r")
			if len(got) == 0 {
				t.Fatalf("no failure parsed from %q", c.out)
			}
			g := got[len(got)-1]
			if c.name == "go vet" || c.name == "phpunit" || c.name == "php frame" {
				g = got[0]
			}
			if g.File != c.want.File || g.Line != c.want.Line || g.Col != c.want.Col || (c.want.Rule != "" && g.Rule != c.want.Rule) {
				t.Fatalf("got %+v want %+v", g, c.want)
			}
			if g.Message == "" || g.Step != "s" {
				t.Fatalf("message/step missing: %+v", g)
			}
		})
	}
}

func TestExtractFailuresIgnoresVendorAndDedupes(t *testing.T) {
	out := "/r/vendor/a/b.php:1:1: error - x\n/r/node_modules/x/y.js:2:2: boom\n/r/src/a.php:5:1: error - dup\n/r/src/a.php:5:1: error - dup\n"
	got := ExtractFailures("phpcs", out, "/r")
	if len(got) != 1 || got[0].File != "src/a.php" {
		t.Fatalf("%+v", got)
	}
	if got[0].Severity != "error" || got[0].Message != "dup" {
		t.Fatalf("severity/message normalisation: %+v", got[0])
	}
}

func TestFingerprintIgnoresLineShifts(t *testing.T) {
	a := []Failure{{Step: "s", File: "a.php", Line: 10, Message: "Expected 3 got 4"}}
	b := []Failure{{Step: "s", File: "a.php", Line: 14, Message: "Expected 5 got 6"}}
	c := []Failure{{Step: "s", File: "a.php", Line: 10, Message: "Different problem"}}
	if Fingerprint(a, "") != Fingerprint(b, "") {
		t.Fatal("line/number shifts must not change the fingerprint")
	}
	if Fingerprint(a, "") == Fingerprint(c, "") {
		t.Fatal("different problems must differ")
	}
	if Fingerprint(nil, "boom 1") != Fingerprint(nil, "boom 2") || Fingerprint(nil, "boom") == Fingerprint(nil, "bang") {
		t.Fatal("unlocated output fingerprinting")
	}
}

func TestPayloadMarkdownAndJSON(t *testing.T) {
	p := Payload{
		Task: "fix it", Attempt: 1, MaxAttempts: 3,
		Failures: []Failure{{Step: "phpcs", File: "a.php", Line: 3, Message: "bad"}},
		Output:   "assertion failed",
		Slices:   []Slice{{Path: "a.php", Start: 2, End: 4, Kind: "function", Signature: "function f()", Text: "function f() {\n\treturn 1;\n}"}},
	}
	md := p.Markdown()
	for _, want := range []string{"## Task", "heal attempt 1 of 3", "[phpcs] a.php:3 — bad", "assertion failed", "### a.php lines 2-4 (function)", "    2| function f() {", "aegis-replace"} {
		if !strings.Contains(md, want) {
			t.Errorf("markdown missing %q:\n%s", want, md)
		}
	}
	js, err := p.MarshalJSON()
	if err != nil || !strings.Contains(string(js), `"code"`) || !strings.Contains(string(js), "function f()") {
		t.Fatalf("json: %s %v", js, err)
	}
	sys := SystemPrompt([]string{"Indent with tabs."})
	if !strings.Contains(sys, "Indent with tabs.") || !strings.Contains(sys, "aegis-replace") {
		t.Fatal(sys)
	}
	if EstimateTokens("abcdefgh") != 2 || EstimateTokens("") != 0 {
		t.Fatal("EstimateTokens")
	}
	if Baseline(map[string]string{"a": strings.Repeat("x", 400)}, strings.Repeat("y", 40)) != 110 {
		t.Fatal("Baseline")
	}
}

func TestRelatedSlicesFindsCalledFunctionsOnly(t *testing.T) {
	src := "package p\n\nfunc Add(a, b int) int { return a - b }\n\nfunc Other() int {\n\treturn 1\n}\n\nfunc Sum(x Add) int { return 0 }\n"
	names := CalledNames("func TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad\")\n\t}\n}")
	if !reflect.DeepEqual(names, []string{"Add", "Fatal"}) {
		t.Fatalf("called names: %v", names)
	}
	got := RelatedSlices("add.go", src, names, nil, SliceOptions{}, 5)
	if len(got) != 1 || !got[0].Related || got[0].Start != 3 || !strings.Contains(got[0].Text, "a - b") {
		t.Fatalf("want only Add, got %+v", got)
	}
	if got := RelatedSlices("add.go", src, names, func(s, e int) bool { return s == 3 }, SliceOptions{}, 5); len(got) != 0 {
		t.Fatalf("already-shown blocks must be skipped: %+v", got)
	}
	php := "<?php\nfunction render_title( $p ) {\n\treturn $p;\n}\n"
	if got := RelatedSlices("a.php", php, []string{"render_title"}, nil, SliceOptions{}, 5); len(got) != 1 {
		t.Fatalf("php: %+v", got)
	}
}
