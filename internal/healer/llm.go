package healer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"aegisloop/internal/config"
)

// Usage is the token usage reported by a provider (0 when unknown).
type Usage struct{ InputTokens, OutputTokens int }

// Provider is an LLM backend.
type Provider interface {
	Name() string
	Complete(ctx context.Context, system, user string) (string, Usage, error)
}

// NewProvider builds the configured provider. It returns (nil, nil) for
// provider "none": the healer then runs in agent mode and hands the compact
// fix request back to the caller instead of calling a model itself.
func NewProvider(c config.LLM, getenv func(string) string) (Provider, error) {
	if getenv == nil {
		getenv = os.Getenv
	}
	timeout := time.Duration(c.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	maxTok := c.MaxTokens
	if maxTok <= 0 {
		maxTok = 4096
	}
	client := &http.Client{Timeout: timeout}
	key := func(def string) (string, error) {
		env := c.APIKeyEnv
		if env == "" {
			env = def
		}
		v := getenv(env)
		if v == "" {
			return "", fmt.Errorf("environment variable %s is not set (needed for llm.provider %q)", env, c.Provider)
		}
		return v, nil
	}
	switch c.Provider {
	case "", "none":
		return nil, nil
	case "anthropic":
		k, err := key("ANTHROPIC_API_KEY")
		if err != nil {
			return nil, err
		}
		return &anthropic{client: client, key: k, model: orDefault(c.Model, "claude-sonnet-5-5"), endpoint: orDefault(c.Endpoint, "https://api.anthropic.com/v1/messages"), maxTokens: maxTok}, nil
	case "openai":
		k, err := key("OPENAI_API_KEY")
		if err != nil {
			return nil, err
		}
		return &openAI{client: client, key: k, model: orDefault(c.Model, "gpt-4o-mini"), endpoint: strings.TrimRight(orDefault(c.Endpoint, "https://api.openai.com/v1"), "/"), maxTokens: maxTok}, nil
	case "ollama":
		return &ollama{client: client, model: orDefault(c.Model, "qwen2.5-coder:7b"), endpoint: strings.TrimRight(orDefault(c.Endpoint, "http://localhost:11434"), "/")}, nil
	case "command":
		if strings.TrimSpace(c.Command) == "" {
			return nil, errors.New("llm.command is empty")
		}
		return &command{cmd: c.Command, timeout: timeout}, nil
	}
	return nil, fmt.Errorf("unknown llm.provider %q", c.Provider)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

// postJSON sends body and decodes the JSON response, retrying 429/5xx twice.
func postJSON(ctx context.Context, client *http.Client, url string, headers map[string]string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 700 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if resp.StatusCode == 429 || resp.StatusCode >= 500 {
			lastErr = fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(data))
			continue
		}
		if resp.StatusCode >= 300 {
			return fmt.Errorf("HTTP %d: %s", resp.StatusCode, snippet(data))
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
		return nil
	}
	return lastErr
}

func snippet(b []byte) string {
	s := strings.Join(strings.Fields(string(b)), " ")
	if len(s) > 300 {
		s = s[:300] + "…"
	}
	return s
}

// ---- Anthropic ---------------------------------------------------------------

type anthropic struct {
	client               *http.Client
	key, model, endpoint string
	maxTokens            int
}

func (a *anthropic) Name() string { return "anthropic:" + a.model }

func (a *anthropic) Complete(ctx context.Context, system, user string) (string, Usage, error) {
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Usage struct {
			In  int `json:"input_tokens"`
			Out int `json:"output_tokens"`
		} `json:"usage"`
	}
	err := postJSON(ctx, a.client, a.endpoint, map[string]string{"x-api-key": a.key, "anthropic-version": "2023-06-01"}, map[string]any{
		"model": a.model, "max_tokens": a.maxTokens, "system": system,
		"messages": []map[string]string{{"role": "user", "content": user}},
	}, &resp)
	if err != nil {
		return "", Usage{}, fmt.Errorf("anthropic: %w", err)
	}
	var b strings.Builder
	for _, c := range resp.Content {
		if c.Type == "text" {
			b.WriteString(c.Text)
		}
	}
	return b.String(), Usage{resp.Usage.In, resp.Usage.Out}, nil
}

// ---- OpenAI-compatible ---------------------------------------------------------

type openAI struct {
	client               *http.Client
	key, model, endpoint string
	maxTokens            int
}

func (o *openAI) Name() string { return "openai:" + o.model }

func (o *openAI) Complete(ctx context.Context, system, user string) (string, Usage, error) {
	var resp struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Usage struct {
			In  int `json:"prompt_tokens"`
			Out int `json:"completion_tokens"`
		} `json:"usage"`
	}
	err := postJSON(ctx, o.client, o.endpoint+"/chat/completions", map[string]string{"Authorization": "Bearer " + o.key}, map[string]any{
		"model": o.model, "max_tokens": o.maxTokens, "temperature": 0,
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
	}, &resp)
	if err != nil {
		return "", Usage{}, fmt.Errorf("openai: %w", err)
	}
	if len(resp.Choices) == 0 {
		return "", Usage{}, errors.New("openai: response has no choices")
	}
	return resp.Choices[0].Message.Content, Usage{resp.Usage.In, resp.Usage.Out}, nil
}

// ---- Ollama --------------------------------------------------------------------

type ollama struct {
	client          *http.Client
	model, endpoint string
}

func (o *ollama) Name() string { return "ollama:" + o.model }

func (o *ollama) Complete(ctx context.Context, system, user string) (string, Usage, error) {
	var resp struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		PromptEval int `json:"prompt_eval_count"`
		Eval       int `json:"eval_count"`
	}
	err := postJSON(ctx, o.client, o.endpoint+"/api/chat", nil, map[string]any{
		"model": o.model, "stream": false, "options": map[string]any{"temperature": 0},
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": user}},
	}, &resp)
	if err != nil {
		return "", Usage{}, fmt.Errorf("ollama: %w", err)
	}
	return resp.Message.Content, Usage{resp.PromptEval, resp.Eval}, nil
}

// ---- external command ----------------------------------------------------------

// command pipes "system\n\n---\n\nuser" to a shell command and uses its stdout
// as the reply, e.g. `claude -p`, `llm -m gpt-4o`, `ollama run qwen2.5-coder`.
type command struct {
	cmd     string
	timeout time.Duration
}

func (c *command) Name() string { return "command" }

func (c *command) Complete(ctx context.Context, system, user string) (string, Usage, error) {
	cctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "sh", "-c", c.cmd)
	cmd.Stdin = strings.NewReader(system + "\n\n---\n\n" + user)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return "", Usage{}, fmt.Errorf("llm command failed: %v: %s", err, snippet(errb.Bytes()))
	}
	return out.String(), Usage{}, nil
}
