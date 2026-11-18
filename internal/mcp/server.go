// Package mcp implements a Model Context Protocol server over stdio
// (newline-delimited JSON-RPC 2.0) so AI editors can call AegisLoop natively.
package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"aegisloop/internal/config"
	"aegisloop/internal/healer"
	"aegisloop/internal/sandbox"
	"aegisloop/internal/tokenizer"
)

// Version is reported to MCP clients.
const Version = "1.0.0"

// Server is an MCP stdio server.
type Server struct {
	DefaultRepo string // used when a call omits repo_path
	Log         io.Writer

	mu  sync.Mutex
	out io.Writer
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// Serve reads requests from in until EOF or ctx is cancelled.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	s.out = out
	r := bufio.NewReaderSize(in, 1<<20)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			s.handleLine(ctx, line)
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

func (s *Server) logf(format string, a ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, "aegis-mcp: "+format+"\n", a...)
	}
}

func (s *Server) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		s.logf("marshal error: %v", err)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, _ = s.out.Write(append(data, '\n'))
}

func (s *Server) reply(id json.RawMessage, result any) {
	s.send(response{JSONRPC: "2.0", ID: id, Result: result})
}

func (s *Server) fail(id json.RawMessage, code int, msg string) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	s.send(response{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}})
}

func (s *Server) handleLine(ctx context.Context, line []byte) {
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		s.fail(nil, -32700, "parse error: "+err.Error())
		return
	}
	if req.JSONRPC != "2.0" || req.Method == "" {
		s.fail(req.ID, -32600, "invalid request")
		return
	}
	isNotification := len(req.ID) == 0
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2024-11-05"
		}
		s.reply(req.ID, map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
			"serverInfo":      map[string]any{"name": "aegisloop", "version": Version},
			"instructions":    "Call verify_and_heal_patch after generating or editing code. It runs zero-token local fixers, lint/test gates and returns a minimal fix request (or heals with the configured model).",
		})
	case "ping":
		s.reply(req.ID, map[string]any{})
	case "tools/list":
		s.reply(req.ID, map[string]any{"tools": Tools()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			s.fail(req.ID, -32602, "invalid params: "+err.Error())
			return
		}
		s.reply(req.ID, s.callTool(ctx, p.Name, p.Arguments))
	default:
		if isNotification || strings.HasPrefix(req.Method, "notifications/") {
			return
		}
		s.fail(req.ID, -32601, "method not found: "+req.Method)
	}
}

// Tool describes an MCP tool.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Tools returns the tool catalogue.
func Tools() []Tool {
	verifySchema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo_path":   map[string]any{"type": "string", "description": "Absolute path of the git repository (default: the server's working directory)."},
			"patch":       map[string]any{"type": "string", "description": "Optional unified diff to intercept. Omit to verify the uncommitted changes already in the working tree."},
			"heal":        map[string]any{"type": "boolean", "description": "Let AegisLoop call its configured LLM to heal failures (default true). When false or no LLM is configured, a compact fix request is returned for you to apply."},
			"dry_run":     map[string]any{"type": "boolean", "description": "Verify without writing anything to the working tree."},
			"max_retries": map[string]any{"type": "integer", "description": "Override max_retries (circuit breaker limit)."},
		},
	}
	return []Tool{
		{Name: "verify_and_heal_patch", Description: "Verify AI-generated code in an isolated git worktree: run zero-token local fixers (phpcbf, eslint --fix, prettier…), the standards guard and the lint/test/build gates, then heal remaining failures within a bounded retry loop. Passing results are applied to the working tree. Returns the status, remaining failures and, without an LLM, a minimal fix request (failing function slices + noise-stripped output).", InputSchema: verifySchema},
		{Name: "run_spec_test_loop", Description: "Alias of verify_and_heal_patch: run the spec/test loop (fix → lint → test) for the current changes and return a compact result.", InputSchema: verifySchema},
		{Name: "prune_context", Description: "Return only the enclosing function/class/rule (with signature and line numbers) around given lines of a file, instead of the whole file.", InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"file", "lines"},
			"properties": map[string]any{
				"repo_path":     map[string]any{"type": "string"},
				"file":          map[string]any{"type": "string", "description": "Repo-relative file path."},
				"lines":         map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "1-based line numbers of interest (e.g. error lines)."},
				"context_lines": map[string]any{"type": "integer", "description": "Window radius when no enclosing block exists (default from .aegis.yml)."},
			},
		}},
		{Name: "strip_trace", Description: "Strip vendor/framework noise (vendor/, node_modules/, runtime frames, progress dots) from raw lint/test output and return only the high-signal lines.", InputSchema: map[string]any{
			"type":     "object",
			"required": []string{"output"},
			"properties": map[string]any{
				"output":    map[string]any{"type": "string"},
				"max_lines": map[string]any{"type": "integer"},
			},
		}},
	}
}

type toolResult struct {
	Content           []map[string]any `json:"content"`
	StructuredContent any              `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError,omitempty"`
}

func textResult(text string, structured any, isErr bool) toolResult {
	return toolResult{Content: []map[string]any{{"type": "text", "text": text}}, StructuredContent: structured, IsError: isErr}
}

func (s *Server) callTool(ctx context.Context, name string, args json.RawMessage) toolResult {
	switch name {
	case "verify_and_heal_patch", "run_spec_test_loop":
		return s.verify(ctx, args)
	case "prune_context":
		return s.prune(ctx, args)
	case "strip_trace":
		var a struct {
			Output   string `json:"output"`
			MaxLines int    `json:"max_lines"`
		}
		if err := json.Unmarshal(args, &a); err != nil || a.Output == "" {
			return textResult("strip_trace requires an `output` string", nil, true)
		}
		st := tokenizer.StripNoise(a.Output, tokenizer.StripOptions{MaxLines: a.MaxLines})
		return textResult(st.Text, map[string]any{"original_bytes": st.OriginalBytes, "stripped_bytes": st.StrippedBytes, "reduction_percent": st.ReductionPercent()}, false)
	}
	return textResult("unknown tool: "+name, nil, true)
}

func (s *Server) repoFor(ctx context.Context, p string) (string, error) {
	if p == "" {
		p = s.DefaultRepo
	}
	if p == "" {
		var err error
		if p, err = os.Getwd(); err != nil {
			return "", err
		}
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	return sandbox.RepoRoot(ctx, p)
}

func (s *Server) verify(ctx context.Context, raw json.RawMessage) toolResult {
	var a struct {
		RepoPath   string `json:"repo_path"`
		Patch      string `json:"patch"`
		Heal       *bool  `json:"heal"`
		DryRun     bool   `json:"dry_run"`
		MaxRetries *int   `json:"max_retries"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return textResult("invalid arguments: "+err.Error(), nil, true)
		}
	}
	root, err := s.repoFor(ctx, a.RepoPath)
	if err != nil {
		return textResult(err.Error(), nil, true)
	}
	cfg, _, err := config.Load(root)
	if err != nil {
		return textResult("config error: "+err.Error(), nil, true)
	}
	if a.MaxRetries != nil {
		cfg.MaxRetries = *a.MaxRetries
		if err := cfg.Validate(); err != nil {
			return textResult("invalid max_retries: "+err.Error(), nil, true)
		}
	}
	var provider healer.Provider
	if a.Heal == nil || *a.Heal {
		provider, err = healer.NewProvider(cfg.LLM, nil)
		if err != nil {
			return textResult("llm configuration error: "+err.Error()+" — call again with heal=false to get a fix request instead", nil, true)
		}
	}
	var logs strings.Builder
	res, err := healer.RunPipeline(ctx, healer.PipelineOptions{
		RepoDir: root, Cfg: cfg, Patch: a.Patch, Provider: provider, DryRun: a.DryRun, Log: &logs,
	})
	if res == nil || res.Result == nil {
		msg := "verification could not run"
		if err != nil {
			msg = err.Error()
		}
		return textResult(msg, nil, true)
	}
	text := healer.Format(res)
	if res.Status == healer.StatusNeedsFix && res.Payload != nil {
		text += "\n" + res.Payload.Markdown()
	}
	if err != nil {
		text += "\nerror: " + err.Error() + "\n"
	}
	return textResult(text, res, res.Status == healer.StatusFailed || err != nil)
}

func (s *Server) prune(ctx context.Context, raw json.RawMessage) toolResult {
	var a struct {
		RepoPath     string `json:"repo_path"`
		File         string `json:"file"`
		Lines        []int  `json:"lines"`
		ContextLines int    `json:"context_lines"`
	}
	if err := json.Unmarshal(raw, &a); err != nil || a.File == "" || len(a.Lines) == 0 {
		return textResult("prune_context requires `file` and `lines`", nil, true)
	}
	root, err := s.repoFor(ctx, a.RepoPath)
	if err != nil {
		return textResult(err.Error(), nil, true)
	}
	clean := filepath.Clean(a.File)
	if filepath.IsAbs(clean) || strings.HasPrefix(clean, "..") {
		return textResult("file must be a path inside the repository", nil, true)
	}
	data, err := os.ReadFile(filepath.Join(root, clean))
	if err != nil {
		return textResult(err.Error(), nil, true)
	}
	cfg, _, err := config.Load(root)
	if err != nil {
		return textResult("config error: "+err.Error(), nil, true)
	}
	ctxLines := cfg.ContextLines
	if a.ContextLines > 0 {
		ctxLines = a.ContextLines
	}
	slices := tokenizer.SliceFile(filepath.ToSlash(clean), string(data), a.Lines, tokenizer.SliceOptions{ContextLines: ctxLines, MaxLines: cfg.MaxSliceLines})
	var b strings.Builder
	for _, sl := range slices {
		fmt.Fprintf(&b, "%s lines %d-%d (%s) %s\n%s\n", sl.Path, sl.Start, sl.End, sl.Kind, sl.Signature, sl.Numbered())
	}
	whole := tokenizer.EstimateTokens(string(data))
	sent := tokenizer.EstimateTokens(b.String())
	return textResult(b.String(), map[string]any{"slices": slices, "file_tokens": whole, "slice_tokens": sent}, false)
}
