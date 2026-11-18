package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"aegisloop/internal/testutil"
)

type rpc struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

func serve(t *testing.T, lines ...string) []rpc {
	t.Helper()
	var out bytes.Buffer
	s := &Server{}
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatal(err)
	}
	var res []rpc
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var r rpc
		if err := json.Unmarshal([]byte(l), &r); err != nil {
			t.Fatalf("server wrote invalid JSON %q: %v", l, err)
		}
		res = append(res, r)
	}
	return res
}

func call(id int, name string, args any) string {
	a, _ := json.Marshal(args)
	return `{"jsonrpc":"2.0","id":` + string(rune('0'+id)) + `,"method":"tools/call","params":{"name":"` + name + `","arguments":` + string(a) + `}}`
}

type toolOut struct {
	Content []struct{ Type, Text string } `json:"content"`
	Struct  map[string]any                `json:"structuredContent"`
	IsError bool                          `json:"isError"`
}

func result(t *testing.T, r rpc) toolOut {
	t.Helper()
	if r.Error != nil {
		t.Fatalf("rpc error: %+v", r.Error)
	}
	var tr toolOut
	if err := json.Unmarshal(r.Result, &tr); err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestHandshakeAndToolCatalogue(t *testing.T) {
	rs := serve(t,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"cursor"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"ping"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":4,"method":"nope/nope"}`,
		`not json at all`,
		`{"id":5,"method":"ping"}`,
	)
	if len(rs) != 6 {
		t.Fatalf("notifications must not be answered; got %d responses", len(rs))
	}
	var init struct {
		ProtocolVersion string                         `json:"protocolVersion"`
		ServerInfo      struct{ Name, Version string } `json:"serverInfo"`
		Capabilities    map[string]any                 `json:"capabilities"`
	}
	json.Unmarshal(rs[0].Result, &init)
	if init.ProtocolVersion != "2025-06-18" || init.ServerInfo.Name != "aegisloop" || init.Capabilities["tools"] == nil {
		t.Fatalf("%+v", init)
	}
	var list struct {
		Tools []Tool `json:"tools"`
	}
	json.Unmarshal(rs[2].Result, &list)
	names := map[string]bool{}
	for _, tl := range list.Tools {
		names[tl.Name] = true
		if tl.Description == "" || tl.InputSchema["type"] != "object" {
			t.Errorf("tool %s is missing a description or schema", tl.Name)
		}
	}
	for _, want := range []string{"verify_and_heal_patch", "run_spec_test_loop", "prune_context", "strip_trace"} {
		if !names[want] {
			t.Errorf("tool %s not exposed", want)
		}
	}
	if rs[3].Error == nil || rs[3].Error.Code != -32601 {
		t.Fatalf("%+v", rs[3].Error)
	}
	if rs[4].Error == nil || rs[4].Error.Code != -32700 {
		t.Fatalf("%+v", rs[4].Error)
	}
	if rs[5].Error == nil || rs[5].Error.Code != -32600 {
		t.Fatalf("%+v", rs[5].Error)
	}
}

func TestVerifyAndHealPatchInAgentMode(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, "../../testdata/wp-vip/base"))
	testutil.WriteFiles(t, repo, testutil.LoadDir(t, "../../testdata/wp-vip/violations"))
	rs := serve(t,
		call(1, "verify_and_heal_patch", map[string]any{"repo_path": repo}),
		call(2, "run_spec_test_loop", map[string]any{"repo_path": repo, "dry_run": true}),
	)
	first := result(t, rs[0])
	if first.IsError {
		t.Fatal("needs_fix is a normal tool outcome, not a tool error")
	}
	text := first.Content[0].Text
	for _, want := range []string{"NEEDS FIX", "zero-token local fixes", "[escape-output]", "function acme_get_posts_html", "aegis-replace"} {
		if !strings.Contains(text, want) {
			t.Errorf("result text lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "acme_helper_30") {
		t.Fatal("unrelated code must not be sent")
	}
	if first.Struct["status"] != "needs_fix" || first.Struct["fix_request"] == nil {
		t.Fatalf("structured content: %v", first.Struct)
	}
	// free fixes landed in the real working tree
	if strings.Contains(testutil.ReadFile(t, repo, "inc/render.php"), "array(") {
		t.Fatal("short-array fixer did not run")
	}
	// the alias behaves the same
	second := result(t, rs[1])
	if second.Struct["status"] != "needs_fix" {
		t.Fatalf("%v", second.Struct)
	}
}

func TestVerifyPassesAndPromotesAPatch(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, "../../testdata/wp-vip/base"))
	patch := "diff --git a/inc/n.php b/inc/n.php\nnew file mode 100644\n--- /dev/null\n+++ b/inc/n.php\n@@ -0,0 +1,3 @@\n+<?php\n+$a = array( 1 );\n+\n"
	rs := serve(t, call(1, "verify_and_heal_patch", map[string]any{"repo_path": repo, "patch": patch}))
	tr := result(t, rs[0])
	if tr.IsError || tr.Struct["status"] != "passed" {
		t.Fatalf("%+v", tr)
	}
	if got := testutil.ReadFile(t, repo, "inc/n.php"); !strings.Contains(got, "[ 1 ]") {
		t.Fatal(got)
	}
}

func TestToolErrors(t *testing.T) {
	notRepo := t.TempDir()
	rs := serve(t,
		call(1, "verify_and_heal_patch", map[string]any{"repo_path": notRepo}),
		call(2, "verify_and_heal_patch", map[string]any{"repo_path": filepath.Join(notRepo, "x"), "max_retries": 99}),
		call(3, "nope", map[string]any{}),
		call(4, "strip_trace", map[string]any{}),
		call(5, "prune_context", map[string]any{"file": "../../etc/passwd", "lines": []int{1}, "repo_path": notRepo}),
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":"bad"}`,
	)
	for i := 0; i < 5; i++ {
		if tr := result(t, rs[i]); !tr.IsError || tr.Content[0].Text == "" {
			t.Errorf("call %d should be a tool error: %+v", i+1, tr)
		}
	}
	if rs[5].Error == nil || rs[5].Error.Code != -32602 {
		t.Fatalf("%+v", rs[5])
	}
}

func TestPruneContextAndStripTrace(t *testing.T) {
	repo := testutil.NewRepo(t, testutil.LoadDir(t, "../../testdata/wp-vip/base"))
	lines := strings.Split(testutil.ReadFile(t, repo, "inc/render.php"), "\n")
	target := 0
	for i, l := range lines {
		if strings.Contains(l, "$label = 'item-' . absint( $id ) . '-30';") {
			target = i + 1
		}
	}
	noisy := "1) FooTest::test_x\nFailed asserting that false is true.\n\n/app/tests/FooTest.php:23\n\n" + strings.Repeat("#1 /app/vendor/phpunit/phpunit/src/x.php(1): y()\n", 100)
	rs := serve(t,
		call(1, "prune_context", map[string]any{"repo_path": repo, "file": "inc/render.php", "lines": []int{target}}),
		call(2, "strip_trace", map[string]any{"output": noisy}),
	)
	p := result(t, rs[0])
	if !strings.Contains(p.Content[0].Text, "function acme_helper_30( $id )") || strings.Contains(p.Content[0].Text, "acme_helper_31") {
		t.Fatalf("%s", p.Content[0].Text)
	}
	if st := p.Struct["slice_tokens"].(float64); st*10 > p.Struct["file_tokens"].(float64) {
		t.Fatalf("slice should be <10%% of the file: %v", p.Struct)
	}
	s := result(t, rs[1])
	if strings.Contains(s.Content[0].Text, "vendor/phpunit") || !strings.Contains(s.Content[0].Text, "Failed asserting") || s.Struct["reduction_percent"].(float64) < 80 {
		t.Fatalf("%+v", s)
	}
}
