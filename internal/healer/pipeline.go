package healer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"aegisloop/internal/config"
	"aegisloop/internal/runner"
	"aegisloop/internal/sandbox"
)

// PipelineOptions configures one end-to-end AegisLoop run.
type PipelineOptions struct {
	RepoDir  string
	Cfg      *config.Config
	Patch    string   // unified diff to intercept (optional)
	Task     string   // natural-language instruction for `aegis fix` (optional)
	Files    []string // files given to the model for Task; default: files changed in the working tree
	Provider Provider // nil = agent mode
	DryRun   bool     // verify/heal but do not write to the working tree
	Keep     bool     // keep the sandbox even on success
	Log      io.Writer
}

// PipelineResult is the outcome of RunPipeline.
type PipelineResult struct {
	*Result
	SandboxID   string   `json:"sandbox_id,omitempty"`
	SandboxPath string   `json:"sandbox_path,omitempty"`
	Branch      string   `json:"branch,omitempty"`
	Promoted    []string `json:"promoted_files,omitempty"`
	Preserved   bool     `json:"sandbox_preserved"`
	PatchPath   string   `json:"patch_path,omitempty"`
	Diff        string   `json:"-"`
	Generated   *Attempt `json:"generation,omitempty"`
}

type sandboxWS struct{ *sandbox.Sandbox }

func (w sandboxWS) Scope(ctx context.Context) (runner.Scope, error) {
	lines, err := w.ChangedLines(ctx)
	if err != nil {
		return runner.Scope{}, err
	}
	sc := runner.Scope{Lines: lines}
	for f := range lines {
		sc.Files = append(sc.Files, f)
	}
	sortStrings(sc.Files)
	return sc, nil
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// RunPipeline creates a sandbox, applies the intercepted patch (or generates
// code from Task), verifies and heals inside the sandbox and, only when every
// gate passes, promotes the result into the developer's working tree.
func RunPipeline(ctx context.Context, o PipelineOptions) (*PipelineResult, error) {
	cfg := o.Cfg
	logf := func(format string, a ...any) {
		if o.Log != nil {
			fmt.Fprintf(o.Log, format+"\n", a...)
		}
	}
	sb, err := sandbox.Create(ctx, o.RepoDir, sandbox.Options{Dir: cfg.Sandbox.Dir, Link: cfg.Sandbox.Link, ScopeToWorkingTree: strings.TrimSpace(o.Patch) == ""})
	if err != nil {
		return nil, err
	}
	logf("sandbox %s (branch %s)", relTo(sb.RepoRoot, sb.Path), sb.Branch)
	out := &PipelineResult{SandboxID: sb.ID, SandboxPath: sb.Path, Branch: sb.Branch}
	ws := sandboxWS{sb}

	// Uncancellable cleanup context so Ctrl-C still prunes/preserves correctly.
	cleanupCtx := context.WithoutCancel(ctx)
	fail := func(err error) (*PipelineResult, error) {
		_ = sb.Prune(cleanupCtx)
		return out, err
	}

	if strings.TrimSpace(o.Patch) != "" {
		logf("applying intercepted patch in sandbox…")
		if err := sb.ApplyPatch(ctx, o.Patch); err != nil {
			return fail(fmt.Errorf("patch rejected (working tree untouched): %w", err))
		}
	}

	h := &Healer{Cfg: cfg, WS: ws, Verify: &runner.Runner{Cfg: cfg, Dir: sb.Path, Log: o.Log}, LLM: o.Provider, Task: o.Task, Log: o.Log}
	if strings.TrimSpace(o.Task) != "" {
		files := o.Files
		if len(files) == 0 {
			sc, err := ws.Scope(ctx)
			if err != nil {
				return fail(err)
			}
			files = sc.Files
		}
		if len(files) == 0 {
			return fail(errors.New("`aegis fix` needs files to work on: pass them after the prompt (aegis fix \"…\" path/to/file.php) or modify them in your working tree first"))
		}
		att, err := h.Generate(ctx, files)
		out.Generated = att
		if err != nil {
			return fail(err)
		}
	}

	if sc, err := ws.Scope(ctx); err != nil {
		return fail(err)
	} else if len(sc.Files) == 0 {
		reason := "the patch/prompt produced no file changes"
		if strings.TrimSpace(o.Patch) == "" && strings.TrimSpace(o.Task) == "" {
			reason = "no uncommitted changes in the working tree (edit files, or pass a patch with `aegis run --patch`)"
		}
		out.Result = &Result{Status: StatusNothing, Reason: reason}
		_ = sb.Prune(cleanupCtx)
		return out, nil
	}

	res, runErr := h.Run(ctx)
	out.Result = res
	if res == nil {
		return fail(runErr)
	}
	if out.Generated != nil {
		res.Tokens.Sent += out.Generated.PromptTokens
		res.Tokens.Received += out.Generated.CompletionTokens
		res.Tokens.Baseline += out.Generated.BaselineTokens
	}
	diff, derr := sb.Diff(cleanupCtx)
	if derr != nil {
		return fail(derr)
	}
	out.Diff = diff

	promote := func() error {
		if o.DryRun {
			return nil
		}
		files, err := sb.Promote(cleanupCtx)
		out.Promoted = files
		return err
	}

	switch {
	case runErr != nil: // interrupted or infrastructure error: keep evidence
		out.keep(cleanupCtx, sb, cfg)
		return out, runErr
	case res.Status == StatusPassed:
		if err := promote(); err != nil {
			out.keep(cleanupCtx, sb, cfg)
			return out, err
		}
		if o.Keep {
			out.Preserved = true
		} else {
			_ = sb.Prune(cleanupCtx)
		}
	case res.Status == StatusNeedsFix:
		// Agent mode: no model ran, so the diff is exactly what the free local
		// fixers changed. Apply it so the caller only has to fix what is left.
		if err := promote(); err != nil {
			out.keep(cleanupCtx, sb, cfg)
			return out, err
		}
		_ = sb.Prune(cleanupCtx)
	default:
		out.keep(cleanupCtx, sb, cfg)
	}
	return out, nil
}

// keep applies the on_failure policy.
func (p *PipelineResult) keep(ctx context.Context, sb *sandbox.Sandbox, cfg *config.Config) {
	if cfg.Sandbox.OnFailure == "prune" {
		_ = sb.Prune(ctx)
		return
	}
	if path, err := sb.SavePatch(ctx); err == nil {
		p.PatchPath = path
	}
	p.Preserved = true
}

func relTo(root, p string) string {
	if r, err := filepath.Rel(root, p); err == nil {
		return r
	}
	return p
}
