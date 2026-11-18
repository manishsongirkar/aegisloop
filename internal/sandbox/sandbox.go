// Package sandbox isolates every AegisLoop run in a native git worktree so the
// developer's checkout is never touched until verification passes.
package sandbox

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Options configures Create.
type Options struct {
	Dir  string   // sandbox parent dir relative to the repo root (default .aegis-sandbox)
	Link []string // repo-relative paths symlinked into the worktree (vendor, node_modules)
	// ScopeToWorkingTree makes ChangedLines/ScopeFiles report everything that
	// differs from HEAD (the developer's uncommitted work plus AegisLoop's edits).
	// By default only what AegisLoop changed since the snapshot is in scope,
	// which is what you want when intercepting a patch.
	ScopeToWorkingTree bool
}

// Sandbox is one ephemeral worktree.
type Sandbox struct {
	ID       string
	Branch   string
	RepoRoot string // main checkout (absolute)
	Path     string // worktree directory (absolute)
	Parent   string // sandbox parent dir (absolute)
	Baseline string // commit hash of the snapshot of the developer's working tree
	Head     string // commit the worktree was created from
	links    []string

	scopeToWorking bool
}

// ErrNotGit is returned when the project is not inside a git work tree.
var ErrNotGit = errors.New("AegisLoop needs a git repository (run `git init` and make an initial commit)")

// RepoRoot returns the top-level directory of the git work tree containing dir.
func RepoRoot(ctx context.Context, dir string) (string, error) {
	out, err := git(ctx, dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", ErrNotGit
	}
	return filepath.Clean(strings.TrimSpace(out)), nil
}

func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// Create adds a worktree at <repo>/<dir>/<uuid> on a new branch aegis-fix-<id>,
// then snapshots the developer's uncommitted changes into it as a baseline
// commit so that later diffs contain only what AegisLoop changed.
func Create(ctx context.Context, repoRoot string, opts Options) (*Sandbox, error) {
	if opts.Dir == "" {
		opts.Dir = ".aegis-sandbox"
	}
	root, err := RepoRoot(ctx, repoRoot)
	if err != nil {
		return nil, err
	}
	if _, err := git(ctx, root, nil, "rev-parse", "--verify", "-q", "HEAD"); err != nil {
		return nil, errors.New("the repository has no commits yet; make an initial commit first")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	parent := filepath.Join(root, opts.Dir)
	s := &Sandbox{
		ID: id, Branch: "aegis-fix-" + id[:8], RepoRoot: root,
		Parent: parent, Path: filepath.Join(parent, id), links: opts.Link, scopeToWorking: opts.ScopeToWorkingTree,
	}
	if head, err := git(ctx, root, nil, "rev-parse", "HEAD"); err == nil {
		s.Head = strings.TrimSpace(head)
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create sandbox dir: %w", err)
	}
	if err := excludeFromGit(ctx, root, opts.Dir); err != nil {
		return nil, err
	}
	if _, err := git(ctx, root, nil, "worktree", "add", "-q", "-b", s.Branch, s.Path, "HEAD"); err != nil {
		return nil, fmt.Errorf("git worktree add: %w", err)
	}
	if err := s.syncWorkingTree(ctx); err != nil {
		_ = s.Prune(ctx)
		return nil, err
	}
	if _, err := s.gitIn(ctx, nil, "add", "-A", "--", "."); err != nil {
		_ = s.Prune(ctx)
		return nil, err
	}
	if _, err := s.gitIn(ctx, nil, "-c", "user.name=aegisloop", "-c", "user.email=aegisloop@localhost", "-c", "commit.gpgsign=false",
		"commit", "-q", "--no-verify", "--allow-empty", "-m", "aegis: baseline"); err != nil {
		_ = s.Prune(ctx)
		return nil, fmt.Errorf("baseline commit: %w", err)
	}
	head, err := s.gitIn(ctx, nil, "rev-parse", "HEAD")
	if err != nil {
		_ = s.Prune(ctx)
		return nil, err
	}
	s.Baseline = strings.TrimSpace(head)
	for _, l := range s.links {
		src := filepath.Join(root, l)
		dst := filepath.Join(s.Path, l)
		if _, err := os.Lstat(src); err != nil {
			continue
		}
		if _, err := os.Lstat(dst); err == nil {
			continue // tracked in the repo: already present
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			continue
		}
		_ = os.Symlink(src, dst)
	}
	return s, nil
}

func excludeFromGit(ctx context.Context, root, dir string) error {
	p, err := git(ctx, root, nil, "rev-parse", "--git-path", "info/exclude")
	if err != nil {
		return err
	}
	path := strings.TrimSpace(p)
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	entry := "/" + strings.Trim(filepath.ToSlash(dir), "/") + "/"
	data, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(l) == entry {
			return nil
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := ""
	if len(data) > 0 && !bytes.HasSuffix(data, []byte("\n")) {
		prefix = "\n"
	}
	_, err = f.WriteString(prefix + "# AegisLoop sandboxes\n" + entry + "\n")
	return err
}

// syncWorkingTree copies the developer's uncommitted state (modified, added,
// deleted and untracked-but-not-ignored files) into the worktree.
func (s *Sandbox) syncWorkingTree(ctx context.Context) error {
	out, err := git(ctx, s.RepoRoot, nil, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return err
	}
	entries := strings.Split(out, "\x00")
	for i := 0; i < len(entries); i++ {
		e := entries[i]
		if len(e) < 4 {
			continue
		}
		x, y, path := e[0], e[1], e[3:]
		if x == 'R' || x == 'C' {
			i++ // next entry is the rename source
			if x == 'R' && i < len(entries) {
				_ = os.Remove(filepath.Join(s.Path, entries[i]))
			}
		}
		if strings.HasPrefix(filepath.ToSlash(path), strings.Trim(filepath.ToSlash(filepath.Base(s.Parent)), "/")+"/") {
			continue
		}
		src := filepath.Join(s.RepoRoot, path)
		dst := filepath.Join(s.Path, path)
		if x == 'D' || y == 'D' {
			if _, statErr := os.Lstat(src); errors.Is(statErr, fs.ErrNotExist) {
				_ = os.Remove(dst)
				continue
			}
		}
		if err := copyEntry(src, dst); err != nil {
			return fmt.Errorf("sync %s: %w", path, err)
		}
	}
	return nil
}

func copyEntry(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	_ = os.Remove(dst)
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	}
	if info.IsDir() {
		return os.MkdirAll(dst, info.Mode().Perm())
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, data, info.Mode().Perm())
}

// ---- file access ------------------------------------------------------------

// Abs resolves a repo-relative path inside the worktree, rejecting escapes.
func (s *Sandbox) Abs(rel string) (string, error) {
	if rel == "" || filepath.IsAbs(rel) {
		return "", fmt.Errorf("path %q must be relative", rel)
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q escapes the sandbox", rel)
	}
	for _, l := range s.links {
		if clean == filepath.Clean(l) || strings.HasPrefix(clean, filepath.Clean(l)+string(filepath.Separator)) {
			return "", fmt.Errorf("path %q is a linked dependency directory and is read-only", rel)
		}
	}
	return filepath.Join(s.Path, clean), nil
}

// Dir returns the worktree directory.
func (s *Sandbox) Dir() string { return s.Path }

// Read returns the content of a repo-relative file in the worktree.
func (s *Sandbox) Read(rel string) (string, error) {
	p, err := s.Abs(rel)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	return string(b), err
}

// Write replaces (or creates) a file in the worktree.
func (s *Sandbox) Write(rel, content string) error {
	p, err := s.Abs(rel)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	mode := fs.FileMode(0o644)
	if st, err := os.Stat(p); err == nil {
		mode = st.Mode().Perm()
	}
	return os.WriteFile(p, []byte(content), mode)
}

// ---- patches ----------------------------------------------------------------

// ApplyPatch applies a unified diff inside the worktree. Line numbers in
// AI-written diffs are often slightly off, so hunks are recounted and
// whitespace differences tolerated as a second attempt.
func (s *Sandbox) ApplyPatch(ctx context.Context, patch string) error {
	if strings.TrimSpace(patch) == "" {
		return errors.New("empty patch")
	}
	if !strings.HasSuffix(patch, "\n") {
		patch += "\n"
	}
	var firstErr error
	for _, extra := range [][]string{{}, {"--recount", "--ignore-whitespace"}, {"--recount", "--ignore-whitespace", "-C1"}} {
		args := append([]string{"apply", "--whitespace=nowarn"}, extra...)
		if _, err := s.gitIn(ctx, strings.NewReader(patch), append(args, "--check", "-")...); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if _, err := s.gitIn(ctx, strings.NewReader(patch), append(args, "-")...); err != nil {
			return err
		}
		return nil
	}
	return fmt.Errorf("patch does not apply: %w", firstErr)
}

func (s *Sandbox) pathspec() []string {
	spec := []string{"--", "."}
	spec = append(spec, ":(exclude)"+filepath.ToSlash(filepath.Base(s.Parent)))
	for _, l := range s.links {
		spec = append(spec, ":(exclude)"+filepath.ToSlash(l))
	}
	return spec
}

func (s *Sandbox) stage(ctx context.Context) error {
	_, err := s.gitIn(ctx, nil, append([]string{"add", "-A"}, s.pathspec()...)...)
	return err
}

// Diff returns everything AegisLoop changed relative to the baseline snapshot.
func (s *Sandbox) Diff(ctx context.Context) (string, error) {
	if err := s.stage(ctx); err != nil {
		return "", err
	}
	return s.gitIn(ctx, nil, append([]string{"diff", "--cached", "--binary", "--no-color", s.Baseline}, s.pathspec()...)...)
}

func (s *Sandbox) scopeRef() string {
	if s.scopeToWorking && s.Head != "" {
		return s.Head
	}
	return s.Baseline
}

// ChangedFiles lists files (repo-relative, forward slashes) that AegisLoop
// changed relative to the baseline snapshot and that still exist in the
// worktree. This is the set Promote writes back.
func (s *Sandbox) ChangedFiles(ctx context.Context) ([]string, error) {
	return s.changedFiles(ctx, s.Baseline)
}

// ScopeFiles lists the files the verification scope covers (see Options).
func (s *Sandbox) ScopeFiles(ctx context.Context) ([]string, error) {
	return s.changedFiles(ctx, s.scopeRef())
}

func (s *Sandbox) changedFiles(ctx context.Context, ref string) ([]string, error) {
	if err := s.stage(ctx); err != nil {
		return nil, err
	}
	out, err := s.gitIn(ctx, nil, append([]string{"diff", "--cached", "--name-only", "--diff-filter=ACMRT", "-z", ref}, s.pathspec()...)...)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, f := range strings.Split(out, "\x00") {
		if f != "" {
			files = append(files, filepath.ToSlash(f))
		}
	}
	sort.Strings(files)
	return files, nil
}

var hunkRe = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,(\d+))? @@`)

// ChangedLines returns, for each file in the verification scope, the set of
// added/modified line numbers (new-file numbering). A nil set means "every
// line" (new files).
func (s *Sandbox) ChangedLines(ctx context.Context) (map[string]map[int]bool, error) {
	ref := s.scopeRef()
	files, err := s.changedFiles(ctx, ref)
	if err != nil {
		return nil, err
	}
	res := make(map[string]map[int]bool, len(files))
	newFiles, err := s.gitIn(ctx, nil, append([]string{"diff", "--cached", "--name-only", "--diff-filter=A", "-z", ref}, s.pathspec()...)...)
	if err != nil {
		return nil, err
	}
	isNew := map[string]bool{}
	for _, f := range strings.Split(newFiles, "\x00") {
		if f != "" {
			isNew[filepath.ToSlash(f)] = true
		}
	}
	for _, f := range files {
		if isNew[f] {
			res[f] = nil
			continue
		}
		d, err := s.gitIn(ctx, nil, "diff", "--cached", "-U0", "--no-color", ref, "--", f)
		if err != nil {
			return nil, err
		}
		set := map[int]bool{}
		for _, l := range strings.Split(d, "\n") {
			m := hunkRe.FindStringSubmatch(l)
			if m == nil {
				continue
			}
			start, _ := strconv.Atoi(m[1])
			count := 1
			if m[2] != "" {
				count, _ = strconv.Atoi(m[2])
			}
			for i := 0; i < count; i++ {
				set[start+i] = true
			}
		}
		res[f] = set
	}
	return res, nil
}

// SavePatch writes the current diff to <parent>/<id>.patch and returns its path.
func (s *Sandbox) SavePatch(ctx context.Context) (string, error) {
	d, err := s.Diff(ctx)
	if err != nil {
		return "", err
	}
	p := filepath.Join(s.Parent, s.ID+".patch")
	return p, os.WriteFile(p, []byte(d), 0o644)
}

// Promote applies the verified diff onto the developer's working tree. It is
// all-or-nothing: if the main checkout drifted so that the diff no longer
// applies, nothing is written and an error is returned.
func (s *Sandbox) Promote(ctx context.Context) (changed []string, err error) {
	d, err := s.Diff(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(d) == "" {
		return nil, nil
	}
	files, err := s.ChangedFiles(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := git(ctx, s.RepoRoot, strings.NewReader(d), "apply", "--whitespace=nowarn", "--check", "-"); err != nil {
		return nil, fmt.Errorf("the working tree changed while AegisLoop was running; the verified patch was kept at %s: %w", filepath.Join(s.Parent, s.ID+".patch"), err)
	}
	if _, err := git(ctx, s.RepoRoot, strings.NewReader(d), "apply", "--whitespace=nowarn", "-"); err != nil {
		return nil, err
	}
	return files, nil
}

// ---- lifecycle --------------------------------------------------------------

// Prune removes the worktree, its branch and its directory. It is idempotent
// and never touches the developer's working tree.
func (s *Sandbox) Prune(ctx context.Context) error {
	var errs []string
	if _, err := git(ctx, s.RepoRoot, nil, "worktree", "remove", "--force", s.Path); err != nil {
		if rmErr := os.RemoveAll(s.Path); rmErr != nil {
			errs = append(errs, rmErr.Error())
		}
	}
	_, _ = git(ctx, s.RepoRoot, nil, "worktree", "prune")
	if _, err := git(ctx, s.RepoRoot, nil, "branch", "-D", s.Branch); err != nil && !strings.Contains(err.Error(), "not found") {
		errs = append(errs, err.Error())
	}
	_ = os.Remove(filepath.Join(s.Parent, s.ID+".patch"))
	_ = os.Remove(s.Parent) // only succeeds when empty
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// Info describes a sandbox found on disk.
type Info struct {
	ID     string
	Path   string
	Branch string
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// List returns the sandboxes present under <repo>/<dir>.
func List(repoRoot, dir string) ([]Info, error) {
	if dir == "" {
		dir = ".aegis-sandbox"
	}
	parent := filepath.Join(repoRoot, dir)
	entries, err := os.ReadDir(parent)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		if e.IsDir() && uuidRe.MatchString(e.Name()) {
			out = append(out, Info{ID: e.Name(), Path: filepath.Join(parent, e.Name()), Branch: "aegis-fix-" + e.Name()[:8]})
		}
	}
	return out, nil
}

// PruneInfo removes a sandbox found by List.
func PruneInfo(ctx context.Context, repoRoot, dir string, in Info) error {
	parent := filepath.Join(repoRoot, dir)
	return (&Sandbox{ID: in.ID, Branch: in.Branch, RepoRoot: repoRoot, Path: in.Path, Parent: parent}).Prune(ctx)
}

// ---- git plumbing -----------------------------------------------------------

func (s *Sandbox) gitIn(ctx context.Context, stdin *strings.Reader, args ...string) (string, error) {
	return git(ctx, s.Path, stdin, args...)
}

func git(ctx context.Context, dir string, stdin *strings.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C", "GIT_OPTIONAL_LOCKS=0")
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args[:min(len(args), 2)], " "), msg)
	}
	return stdout.String(), nil
}
