// Command aegis is the AegisLoop CLI: an AI guardrail and local self-healing
// verification wrapper for AI-generated code.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"aegisloop/internal/config"
	"aegisloop/internal/healer"
	"aegisloop/internal/mcp"
	"aegisloop/internal/runner"
	"aegisloop/internal/sandbox"
	"aegisloop/internal/tokenizer"
)

// version is overridden at build time: -ldflags "-X main.version=1.2.3".
var version = "1.0.0"

// Exit codes.
const (
	exitOK       = 0
	exitFailed   = 1 // verification failed / circuit breaker tripped
	exitNeedsFix = 2 // no LLM configured: remaining failures need a code change
	exitUsage    = 3 // bad arguments, bad config, infrastructure error
)

const usage = `AegisLoop — AI guardrail & local self-healing verification

Usage:
  aegis init [--force]                     Detect the stack and write .aegis.yml
  aegis run --patch <file|->               Intercept an AI patch: sandbox, verify, heal, apply
  aegis verify                             Verify (and heal) the uncommitted changes in the working tree
  aegis fix "<prompt>" [files...]          Have the configured LLM implement a change, then verify/heal it
  aegis prune <file>:<line>[,<line>…]      Print only the enclosing function around the given lines
  aegis rules                              List the built-in standards rules
  aegis clean [--list]                     Remove (or list) preserved sandboxes
  aegis mcp                                Run the MCP server on stdio (Cursor, VS Code, Claude Code)
  aegis version

Common flags (run, verify, fix):
  --repo <dir>          Repository to operate on (default: current directory)
  --max-retries <n>     Circuit-breaker limit for LLM heal attempts
  --provider <name>     anthropic | openai | ollama | command | none (overrides .aegis.yml)
  --model <name>        Model override
  --no-heal             Local gates only; never call an LLM
  --dry-run             Do everything except writing to the working tree
  --keep                Keep the sandbox even when everything passes
  --json                Machine-readable output
  -q                    Quiet: no progress output

Exit codes: 0 passed · 1 failed/circuit breaker · 2 needs a code fix (no LLM) · 3 usage or configuration error
`

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return exitUsage
	}
	cmd, rest := args[0], args[1:]
	var err error
	code := exitOK
	switch cmd {
	case "init":
		err = cmdInit(rest, stdout, stderr)
	case "run", "verify", "fix":
		code, err = cmdPipeline(ctx, cmd, rest, stdin, stdout, stderr)
	case "prune":
		err = cmdPrune(rest, stdout)
	case "rules":
		for _, r := range runner.Rules() {
			fmt.Fprintf(stdout, "%-18s %-11s %s\n", r.ID, r.Lang, r.Desc)
		}
	case "clean":
		err = cmdClean(ctx, rest, stdout)
	case "mcp":
		err = cmdMCP(ctx, rest, stdin, stdout, stderr)
	case "version", "--version", "-v":
		fmt.Fprintf(stdout, "aegis %s\n", version)
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
	default:
		fmt.Fprintf(stderr, "aegis: unknown command %q\n\n%s", cmd, usage)
		return exitUsage
	}
	if err != nil {
		var ue usageError
		if errors.As(err, &ue) && ue.help {
			return exitOK
		}
		fmt.Fprintf(stderr, "aegis: %v\n", err)
		return exitUsage
	}
	return code
}

type usageError struct {
	msg  string
	help bool
}

func (u usageError) Error() string { return u.msg }

func newFlags(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("aegis "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

func parse(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return usageError{help: true}
		}
		return usageError{msg: err.Error()}
	}
	return nil
}

// ---- init -----------------------------------------------------------------------

func cmdInit(args []string, stdout, stderr io.Writer) error {
	fs := newFlags("init", stderr)
	repo := fs.String("repo", ".", "repository directory")
	force := fs.Bool("force", false, "overwrite an existing .aegis.yml")
	if err := parse(fs, args); err != nil {
		return err
	}
	root, err := filepath.Abs(*repo)
	if err != nil {
		return err
	}
	target := filepath.Join(root, config.FileName)
	if _, err := os.Stat(target); err == nil && !*force {
		return fmt.Errorf("%s already exists (use --force to overwrite)", target)
	}
	det := config.Detect(root)
	cfg := det.Config()
	if err := cfg.Validate(); err != nil {
		return err
	}
	if err := os.WriteFile(target, []byte(cfg.Render()), 0o644); err != nil {
		return err
	}
	var stacks []string
	if det.PHP {
		s := "PHP"
		if det.WordPress {
			s = "PHP / WordPress"
		}
		stacks = append(stacks, s)
	}
	if det.JS {
		s := "JavaScript"
		if det.TypeScript {
			s = "TypeScript"
		}
		stacks = append(stacks, s)
	}
	if det.Go {
		stacks = append(stacks, "Go")
	}
	if len(stacks) == 0 {
		stacks = []string{"none detected — edit the generated file to add your fixers and checks"}
	}
	fmt.Fprintf(stdout, "Detected: %s\nWrote %s (%d fixer(s), %d check(s))\n", strings.Join(stacks, ", "), target, len(cfg.Fixers), len(cfg.Checks))
	if _, err := sandbox.RepoRoot(context.Background(), root); err != nil {
		fmt.Fprintln(stderr, "warning: this directory is not a git repository; AegisLoop needs one (git init && git commit).")
	}
	fmt.Fprintln(stdout, "Next: set llm.provider in .aegis.yml (or keep \"none\" for editor/MCP mode), then try `aegis verify`.")
	return nil
}

// ---- run / verify / fix -----------------------------------------------------------

func cmdPipeline(ctx context.Context, cmd string, args []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	fs := newFlags(cmd, stderr)
	repo := fs.String("repo", ".", "repository directory")
	patchPath := fs.String("patch", "", "unified diff file, or - for stdin (run only)")
	maxRetries := fs.Int("max-retries", -1, "override max_retries")
	provider := fs.String("provider", "", "LLM provider override")
	model := fs.String("model", "", "model override")
	noHeal := fs.Bool("no-heal", false, "never call an LLM")
	dry := fs.Bool("dry-run", false, "do not write to the working tree")
	keep := fs.Bool("keep", false, "keep the sandbox on success")
	asJSON := fs.Bool("json", false, "JSON output")
	quiet := fs.Bool("q", false, "quiet")
	// allow flags after positional arguments (aegis fix "prompt" file.php --dry-run)
	pos, err := parseInterspersed(fs, args)
	if err != nil {
		return exitUsage, err
	}

	var task string
	var files []string
	switch cmd {
	case "fix":
		if len(pos) == 0 || strings.TrimSpace(pos[0]) == "" {
			return exitUsage, usageError{msg: `usage: aegis fix "<prompt>" [files...]`}
		}
		task, files = pos[0], pos[1:]
	default:
		if len(pos) > 0 {
			return exitUsage, usageError{msg: fmt.Sprintf("unexpected argument %q", pos[0])}
		}
	}
	var patch string
	if cmd == "run" {
		if *patchPath == "" {
			return exitUsage, usageError{msg: "aegis run needs --patch <file|-> (use `aegis verify` for working-tree changes)"}
		}
		var data []byte
		if *patchPath == "-" {
			data, err = io.ReadAll(stdin)
		} else {
			data, err = os.ReadFile(*patchPath)
		}
		if err != nil {
			return exitUsage, err
		}
		patch = string(data)
	} else if *patchPath != "" {
		return exitUsage, usageError{msg: "--patch is only valid for `aegis run`"}
	}

	root, err := sandbox.RepoRoot(ctx, mustAbs(*repo))
	if err != nil {
		return exitUsage, err
	}
	cfg, found, err := config.Load(root)
	if err != nil {
		return exitUsage, err
	}
	if !found && !*quiet {
		fmt.Fprintln(stderr, "note: no .aegis.yml found — using auto-detected defaults (run `aegis init` to write them).")
	}
	if *maxRetries >= 0 {
		cfg.MaxRetries = *maxRetries
	}
	if *provider != "" {
		cfg.LLM.Provider = *provider
	}
	if *model != "" {
		cfg.LLM.Model = *model
	}
	if err := cfg.Validate(); err != nil {
		return exitUsage, err
	}
	var prov healer.Provider
	if !*noHeal {
		if prov, err = healer.NewProvider(cfg.LLM, nil); err != nil {
			return exitUsage, err
		}
	}
	if cmd == "fix" && prov == nil {
		return exitUsage, errors.New("`aegis fix` needs an LLM: set llm.provider in .aegis.yml or pass --provider (and do not use --no-heal)")
	}
	var log io.Writer
	if !*quiet && !*asJSON {
		log = stderr
	}
	res, err := healer.RunPipeline(ctx, healer.PipelineOptions{
		RepoDir: root, Cfg: cfg, Patch: patch, Task: task, Files: normalizeFiles(files), Provider: prov, DryRun: *dry, Keep: *keep, Log: log,
	})
	if res == nil || res.Result == nil {
		if err == nil {
			err = errors.New("verification could not run")
		}
		return exitUsage, err
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		fmt.Fprint(stdout, healer.Format(res))
		if res.Status == healer.StatusNeedsFix && res.Payload != nil {
			fmt.Fprintf(stdout, "\n%s", res.Payload.Markdown())
		}
		if *dry && res.Diff != "" && res.Status == healer.StatusPassed {
			fmt.Fprintf(stdout, "\n--- proposed changes (dry run) ---\n%s", res.Diff)
		}
	}
	if err != nil {
		return exitUsage, err
	}
	switch res.Status {
	case healer.StatusPassed:
		return exitOK, nil
	case healer.StatusNeedsFix:
		return exitNeedsFix, nil
	}
	return exitFailed, nil
}

// parseInterspersed parses flags that may appear before, between or after
// positional arguments.
func parseInterspersed(fs *flag.FlagSet, args []string) ([]string, error) {
	var pos []string
	for len(args) > 0 {
		if err := parse(fs, args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	return pos, nil
}

func normalizeFiles(files []string) []string {
	var out []string
	for _, f := range files {
		out = append(out, filepath.ToSlash(filepath.Clean(f)))
	}
	return out
}

func mustAbs(p string) string {
	a, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	return a
}

// ---- prune ------------------------------------------------------------------------

func cmdPrune(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{msg: "usage: aegis prune <file>:<line>[,<line>…]"}
	}
	spec := args[0]
	idx := strings.LastIndex(spec, ":")
	if idx <= 0 {
		return usageError{msg: "usage: aegis prune <file>:<line>[,<line>…]"}
	}
	file := spec[:idx]
	var lines []int
	for _, p := range strings.Split(spec[idx+1:], ",") {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < 1 {
			return usageError{msg: fmt.Sprintf("bad line number %q", p)}
		}
		lines = append(lines, n)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	cfg := config.Default()
	slices := tokenizer.SliceFile(filepath.ToSlash(file), string(data), lines, tokenizer.SliceOptions{ContextLines: cfg.ContextLines, MaxLines: cfg.MaxSliceLines})
	var out strings.Builder
	for _, s := range slices {
		fmt.Fprintf(&out, "%s lines %d-%d (%s) %s\n%s\n", s.Path, s.Start, s.End, s.Kind, s.Signature, s.Numbered())
	}
	fmt.Fprint(stdout, out.String())
	whole, sent := tokenizer.EstimateTokens(string(data)), tokenizer.EstimateTokens(out.String())
	switch {
	case whole <= 0:
	case sent >= whole:
		fmt.Fprintf(stdout, "~%d tokens; the file is only ~%d, so slicing saves nothing here\n", sent, whole)
	default:
		fmt.Fprintf(stdout, "~%d tokens instead of ~%d for the whole file (%.0f%% fewer)\n", sent, whole, 100*(1-float64(sent)/float64(whole)))
	}
	return nil
}

// ---- clean ------------------------------------------------------------------------

func cmdClean(ctx context.Context, args []string, stdout io.Writer) error {
	fs := newFlags("clean", io.Discard)
	repo := fs.String("repo", ".", "repository directory")
	list := fs.Bool("list", false, "only list sandboxes")
	if err := parse(fs, args); err != nil {
		return err
	}
	root, err := sandbox.RepoRoot(ctx, mustAbs(*repo))
	if err != nil {
		return err
	}
	cfg, _, err := config.Load(root)
	if err != nil {
		return err
	}
	infos, err := sandbox.List(root, cfg.Sandbox.Dir)
	if err != nil {
		return err
	}
	if len(infos) == 0 {
		fmt.Fprintln(stdout, "no AegisLoop sandboxes found")
		return nil
	}
	for _, in := range infos {
		if *list {
			fmt.Fprintf(stdout, "%s  %s  (branch %s)\n", in.ID, in.Path, in.Branch)
			continue
		}
		if err := sandbox.PruneInfo(ctx, root, cfg.Sandbox.Dir, in); err != nil {
			return fmt.Errorf("prune %s: %w", in.ID, err)
		}
		fmt.Fprintf(stdout, "removed %s\n", in.ID)
	}
	return nil
}

// ---- mcp --------------------------------------------------------------------------

func cmdMCP(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	fs := newFlags("mcp", stderr)
	repo := fs.String("repo", "", "default repository (otherwise each call's repo_path or the working directory)")
	if err := parse(fs, args); err != nil {
		return err
	}
	s := &mcp.Server{DefaultRepo: *repo, Log: stderr}
	if err := s.Serve(ctx, stdin, stdout); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}
	return nil
}
