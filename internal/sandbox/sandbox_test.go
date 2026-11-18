package sandbox

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/manishsongirkar/aegisloop/internal/testutil"
)

var ctx = context.Background()

func status(t *testing.T, dir string) string {
	return testutil.Git(t, dir, "status", "--porcelain=v1", "--untracked-files=all")
}

func exists(p string) bool { _, err := os.Lstat(p); return err == nil }

func TestCreateSnapshotsWorkingTreeAndIsolates(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.php": "<?php\n$a = 1;\n", "b.php": "<?php\n$b = 1;\n", "gone.php": "x\n", ".gitignore": "ignored.log\n"})
	// developer's uncommitted state: modify, delete, add untracked, ignored file
	testutil.WriteFiles(t, repo, map[string]string{"a.php": "<?php\n$a = 2;\n", "new/n.php": "<?php\n", "ignored.log": "noise"})
	os.Remove(filepath.Join(repo, "gone.php"))
	before := status(t, repo)

	sb, err := Create(ctx, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Prune(ctx)

	if !strings.HasPrefix(sb.Path, filepath.Join(repo, ".aegis-sandbox")+string(filepath.Separator)) || !uuidRe.MatchString(filepath.Base(sb.Path)) {
		t.Fatalf("worktree must live in .aegis-sandbox/<uuid>: %s", sb.Path)
	}
	if !strings.HasPrefix(sb.Branch, "aegis-fix-") {
		t.Fatalf("branch %s", sb.Branch)
	}
	if got, _ := sb.Read("a.php"); got != "<?php\n$a = 2;\n" {
		t.Fatalf("modified file not snapshotted: %q", got)
	}
	if got, _ := sb.Read("new/n.php"); got != "<?php\n" {
		t.Fatalf("untracked file not snapshotted: %q", got)
	}
	if exists(filepath.Join(sb.Path, "gone.php")) {
		t.Fatal("deleted file must be deleted in the sandbox")
	}
	if exists(filepath.Join(sb.Path, "ignored.log")) {
		t.Fatal("ignored files must not be copied")
	}
	// the snapshot is the baseline: nothing counts as changed yet
	files, err := sb.ChangedFiles(ctx)
	if err != nil || len(files) != 0 {
		t.Fatalf("baseline must be clean: %v %v", files, err)
	}
	// main checkout untouched and the sandbox dir is not reported as untracked
	if after := status(t, repo); after != before {
		t.Fatalf("working tree changed:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if !strings.Contains(testutil.ReadFile(t, repo, ".git/info/exclude"), "/.aegis-sandbox/") {
		t.Fatal(".aegis-sandbox must be excluded from git")
	}
}

func TestDiffChangedLinesAndPromote(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.php": "<?php\n$a = 1;\n$b = 2;\n$c = 3;\n"})
	sb, err := Create(ctx, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Prune(ctx)
	if err := sb.Write("a.php", "<?php\n$a = 1;\n$b = 22;\n$c = 3;\n$d = 4;\n"); err != nil {
		t.Fatal(err)
	}
	if err := sb.Write("sub/new.php", "<?php\n"); err != nil {
		t.Fatal(err)
	}
	lines, err := sb.ChangedLines(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(lines["a.php"], map[int]bool{3: true, 5: true}) {
		t.Fatalf("a.php lines: %v", lines["a.php"])
	}
	if set, ok := lines["sub/new.php"]; !ok || set != nil {
		t.Fatalf("new files mean every line: %v %v", set, ok)
	}
	diff, err := sb.Diff(ctx)
	if err != nil || !strings.Contains(diff, "+$b = 22;") || !strings.Contains(diff, "sub/new.php") {
		t.Fatalf("%v\n%s", err, diff)
	}
	if status(t, repo) != "" {
		t.Fatal("nothing may reach the main tree before Promote")
	}
	promoted, err := sb.Promote(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(promoted, []string{"a.php", "sub/new.php"}) {
		t.Fatalf("%v", promoted)
	}
	if got := testutil.ReadFile(t, repo, "a.php"); !strings.Contains(got, "$b = 22;") {
		t.Fatalf("%q", got)
	}
	if testutil.ReadFile(t, repo, "sub/new.php") != "<?php\n" {
		t.Fatal("new file not promoted")
	}
}

func TestApplyPatchToleratesWrongLineNumbers(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "one\ntwo\nthree\nfour\nfive\n"})
	sb, _ := Create(ctx, repo, Options{})
	defer sb.Prune(ctx)
	patch := "--- a/a.txt\n+++ b/a.txt\n@@ -9,3 +9,3 @@\n two\n-three\n+THREE\n four\n"
	if err := sb.ApplyPatch(ctx, patch); err != nil {
		t.Fatalf("recounted hunk should apply: %v", err)
	}
	if got, _ := sb.Read("a.txt"); !strings.Contains(got, "THREE") {
		t.Fatal(got)
	}
	// a patch that cannot apply leaves the sandbox untouched and reports an error
	bad := "--- a/a.txt\n+++ b/a.txt\n@@ -1,1 +1,1 @@\n-nonexistent\n+x\n"
	if err := sb.ApplyPatch(ctx, bad); err == nil {
		t.Fatal("expected failure")
	}
	if err := sb.ApplyPatch(ctx, "  \n"); err == nil {
		t.Fatal("empty patch must fail")
	}
	if got, _ := sb.Read("a.txt"); !strings.Contains(got, "THREE") || strings.Contains(got, "nonexistent") {
		t.Fatal("failed patch modified the sandbox")
	}
}

// Forced failure: whatever happens inside the sandbox, Prune restores a clean
// state — no worktree, no branch, no leftover directory — and the developer's
// checkout is bit-for-bit what it was.
func TestPruneRollsBackCompletely(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.php": "<?php\n$a = 1;\n"})
	testutil.WriteFiles(t, repo, map[string]string{"a.php": "<?php\n$a = 'dev edit';\n"})
	before := status(t, repo)
	content := testutil.ReadFile(t, repo, "a.php")

	sb, err := Create(ctx, repo, Options{})
	if err != nil {
		t.Fatal(err)
	}
	sb.Write("a.php", "<?php\nBROKEN(\n")
	sb.Write("junk/x.php", "junk")
	if _, err := sb.SavePatch(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sb.Prune(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sb.Prune(ctx); err != nil { // idempotent
		t.Fatalf("second prune: %v", err)
	}
	if exists(sb.Path) || exists(filepath.Join(sb.Parent, sb.ID+".patch")) {
		t.Fatal("sandbox files remain")
	}
	if wl := testutil.Git(t, repo, "worktree", "list"); strings.Count(wl, "\n") != 1 {
		t.Fatalf("worktree still registered:\n%s", wl)
	}
	if br := testutil.Git(t, repo, "branch", "--list", "aegis-fix-*"); strings.TrimSpace(br) != "" {
		t.Fatalf("branch remains: %s", br)
	}
	if status(t, repo) != before || testutil.ReadFile(t, repo, "a.php") != content {
		t.Fatal("developer's working tree was disturbed")
	}
}

func TestPrunePartialFailureStillCleans(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "x\n"})
	sb, _ := Create(ctx, repo, Options{})
	// simulate a crash that already removed the directory
	if err := os.RemoveAll(sb.Path); err != nil {
		t.Fatal(err)
	}
	if err := sb.Prune(ctx); err != nil {
		t.Fatalf("prune after external deletion: %v", err)
	}
	if br := testutil.Git(t, repo, "branch", "--list", "aegis-fix-*"); strings.TrimSpace(br) != "" {
		t.Fatal("branch remains")
	}
}

func TestPromoteRefusesWhenWorkingTreeDrifted(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "one\ntwo\nthree\n"})
	sb, _ := Create(ctx, repo, Options{})
	defer sb.Prune(ctx)
	sb.Write("a.txt", "one\nTWO\nthree\n")
	// the developer edits the same line while AegisLoop is running
	testutil.WriteFiles(t, repo, map[string]string{"a.txt": "one\ndev\nthree\n"})
	_, err := sb.Promote(ctx)
	if err == nil || !strings.Contains(err.Error(), "working tree changed") {
		t.Fatalf("expected a drift error, got %v", err)
	}
	if got := testutil.ReadFile(t, repo, "a.txt"); got != "one\ndev\nthree\n" {
		t.Fatalf("promotion must be all-or-nothing: %q", got)
	}
}

func TestPromoteNothing(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "x\n"})
	sb, _ := Create(ctx, repo, Options{})
	defer sb.Prune(ctx)
	files, err := sb.Promote(ctx)
	if err != nil || files != nil {
		t.Fatalf("%v %v", files, err)
	}
}

func TestPathSafetyAndLinks(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "x\n"})
	os.MkdirAll(filepath.Join(repo, "vendor", "bin"), 0o755)
	os.WriteFile(filepath.Join(repo, "vendor", "bin", "tool"), []byte("#!/bin/sh\n"), 0o755)
	sb, err := Create(ctx, repo, Options{Link: []string{"vendor", "node_modules"}})
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Prune(ctx)
	for _, bad := range []string{"../x", "a/../../x", "/etc/passwd", "", "vendor/bin/tool", "vendor"} {
		if _, err := sb.Abs(bad); err == nil {
			t.Errorf("%q must be rejected", bad)
		}
		if err := sb.Write(bad, "x"); err == nil {
			t.Errorf("write %q must be rejected", bad)
		}
	}
	if !exists(filepath.Join(sb.Path, "vendor", "bin", "tool")) {
		t.Fatal("vendor must be linked into the sandbox so tools can run")
	}
	if exists(filepath.Join(sb.Path, "node_modules")) {
		t.Fatal("absent link sources are skipped")
	}
	// linked dirs never show up in diffs or changed files
	sb.Write("b.txt", "y\n")
	files, _ := sb.ChangedFiles(ctx)
	if !reflect.DeepEqual(files, []string{"b.txt"}) {
		t.Fatalf("%v", files)
	}
	// pruning removes the symlink but must never delete the real vendor dir
	sb.Prune(ctx)
	if !exists(filepath.Join(repo, "vendor", "bin", "tool")) {
		t.Fatal("Prune followed a symlink and deleted the real vendor directory")
	}
}

func TestErrorsOutsideGit(t *testing.T) {
	dir := t.TempDir()
	if _, err := Create(ctx, dir, Options{}); !errors.Is(err, ErrNotGit) {
		t.Fatalf("%v", err)
	}
	empty := t.TempDir()
	testutil.Git(t, empty, "init", "-q")
	if _, err := Create(ctx, empty, Options{}); err == nil || !strings.Contains(err.Error(), "no commits") {
		t.Fatalf("%v", err)
	}
}

func TestListAndPruneInfoAndIndependence(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "x\n"})
	s1, _ := Create(ctx, repo, Options{})
	s2, _ := Create(ctx, repo, Options{})
	s1.Write("a.txt", "one\n")
	if got, _ := s2.Read("a.txt"); got != "x\n" {
		t.Fatal("sandboxes must be independent")
	}
	infos, err := List(repo, "")
	if err != nil || len(infos) != 2 {
		t.Fatalf("%v %v", infos, err)
	}
	for _, in := range infos {
		if err := PruneInfo(ctx, repo, ".aegis-sandbox", in); err != nil {
			t.Fatal(err)
		}
	}
	if infos, _ := List(repo, ""); len(infos) != 0 {
		t.Fatalf("%v", infos)
	}
	if exists(filepath.Join(repo, ".aegis-sandbox")) {
		t.Fatal("empty sandbox parent should be removed")
	}
	if br := testutil.Git(t, repo, "branch", "--list", "aegis-fix-*"); strings.TrimSpace(br) != "" {
		t.Fatal(br)
	}
}

func TestSubdirectoryRepoRoot(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"pkg/a.txt": "x\n"})
	root, err := RepoRoot(ctx, filepath.Join(repo, "pkg"))
	if err != nil || root != repo {
		t.Fatalf("%q %v", root, err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git missing")
	}
}

func TestPromoteRefusesWhenADifferentFileDrifted(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "one\n", "b.txt": "x\n"})
	testutil.WriteFiles(t, repo, map[string]string{"a.txt": "dirty\n"})
	sb, _ := Create(ctx, repo, Options{})
	defer sb.Prune(ctx)
	sb.Write("a.txt", "fixed\n")
	// a file AegisLoop does not touch changes mid-run: the verdict is stale
	testutil.WriteFiles(t, repo, map[string]string{"b.txt": "edited by the developer\n"})
	drift, _ := sb.Drift(ctx)
	if len(drift) != 1 || drift[0] != "b.txt" {
		t.Fatalf("drift = %v", drift)
	}
	if _, err := sb.Promote(ctx); err == nil || !strings.Contains(err.Error(), "b.txt") {
		t.Fatalf("expected a drift error naming b.txt, got %v", err)
	}
	if got := testutil.ReadFile(t, repo, "a.txt"); got != "dirty\n" {
		t.Fatalf("nothing may be written on drift: %q", got)
	}
}

func TestDriftDetectsNewUntrackedRevertAndCommit(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.txt": "one\n"})
	testutil.WriteFiles(t, repo, map[string]string{"a.txt": "dirty\n"})
	sb, _ := Create(ctx, repo, Options{})
	defer sb.Prune(ctx)
	if d, _ := sb.Drift(ctx); len(d) != 0 {
		t.Fatalf("no drift expected right after Create: %v", d)
	}
	testutil.WriteFiles(t, repo, map[string]string{"new.txt": "n\n"})
	testutil.Git(t, repo, "checkout", "a.txt") // revert
	d, _ := sb.Drift(ctx)
	if !reflect.DeepEqual(d, []string{"a.txt", "new.txt"}) {
		t.Fatalf("drift = %v", d)
	}
}

// A heal edit that restores a file to its HEAD content must not drop the file
// from the verification scope (the gates would be skipped and still "pass").
func TestScopeKeepsFileHealedBackToHead(t *testing.T) {
	repo := testutil.NewRepo(t, map[string]string{"a.go": "good\n", "b.go": "b\n"})
	testutil.WriteFiles(t, repo, map[string]string{"a.go": "broken\n"})
	sb, _ := Create(ctx, repo, Options{ScopeToWorkingTree: true})
	defer sb.Prune(ctx)
	if f, _ := sb.ScopeFiles(ctx); !reflect.DeepEqual(f, []string{"a.go"}) {
		t.Fatalf("scope before heal: %v", f)
	}
	sb.Write("a.go", "good\n") // identical to HEAD again
	f, _ := sb.ScopeFiles(ctx)
	if !reflect.DeepEqual(f, []string{"a.go"}) {
		t.Fatalf("a healed file must stay in scope, got %v", f)
	}
	lines, _ := sb.ChangedLines(ctx)
	if _, ok := lines["a.go"]; !ok {
		t.Fatalf("ChangedLines must keep a.go: %v", lines)
	}
}
