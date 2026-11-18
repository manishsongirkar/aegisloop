// Package config loads, validates, renders and auto-detects .aegis.yml.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// FileName is the name of the project configuration file.
const FileName = ".aegis.yml"

// Step is one fixer or check executed by the runner.
//
// Run is a command template. Supported placeholders: {files} (all matching
// changed files as separate arguments) and {file} (run once per file).
// Run values starting with "builtin:" select an in-process implementation
// (builtin:tabs, builtin:short-arrays, builtin:standards).
type Step struct {
	Name        string
	Run         string
	Kind        string // lint | test | build | security (checks only; informational)
	Extensions  []string
	OkExitCodes []int
	Required    bool // a missing tool fails the step instead of skipping it
	Shell       bool // run through `sh -c` (enables pipes/&&); default is direct exec
}

// Sandbox configures git-worktree isolation.
type Sandbox struct {
	Dir       string   // relative to repo root; default .aegis-sandbox
	OnFailure string   // preserve | prune
	Link      []string // paths symlinked from the main checkout (vendor, node_modules)
}

// Guard configures the built-in standards guard.
type Guard struct {
	Enabled bool
	Disable []string // rule IDs to switch off
}

// LLM configures the healing model.
type LLM struct {
	Provider       string // anthropic | openai | ollama | command | none
	Model          string
	Endpoint       string
	APIKeyEnv      string
	MaxTokens      int
	Command        string
	TimeoutSeconds int
}

// Config is the full AegisLoop configuration.
type Config struct {
	Version           int
	Language          string // php | js | go | auto
	MaxRetries        int
	StrictTabs        bool
	StrictShortArrays bool
	TokenBudgetAlert  int
	ContextLines      int
	MaxSliceLines     int
	TimeoutSeconds    int
	Sandbox           Sandbox
	Guard             Guard
	Fixers            []Step
	Checks            []Step
	LLM               LLM

	Root string // absolute project root (not serialised)
}

// Default returns a configuration with only generic defaults (no steps).
func Default() *Config {
	return &Config{
		Version:           1,
		Language:          "auto",
		MaxRetries:        3,
		StrictTabs:        true,
		StrictShortArrays: true,
		TokenBudgetAlert:  20000,
		ContextLines:      10,
		MaxSliceLines:     120,
		TimeoutSeconds:    300,
		Sandbox:           Sandbox{Dir: ".aegis-sandbox", OnFailure: "preserve"},
		Guard:             Guard{Enabled: true},
		LLM:               LLM{Provider: "none", MaxTokens: 4096, TimeoutSeconds: 120},
	}
}

// Load reads <root>/.aegis.yml. If the file does not exist it returns the
// auto-detected defaults and found=false.
func Load(root string) (cfg *Config, found bool, err error) {
	root, err = filepath.Abs(root)
	if err != nil {
		return nil, false, err
	}
	path := filepath.Join(root, FileName)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		c := Detect(root).Config()
		c.Root = root
		return c, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("read %s: %w", path, err)
	}
	c, err := Parse(string(data))
	if err != nil {
		return nil, true, fmt.Errorf("%s: %w", path, err)
	}
	c.Root = root
	return c, true, nil
}

// Parse decodes YAML text on top of Default().
func Parse(src string) (*Config, error) {
	raw, err := ParseYAML(src)
	if err != nil {
		return nil, err
	}
	c := Default()
	d := decoder{}
	d.int(raw, "version", &c.Version)
	d.str(raw, "language", &c.Language)
	d.int(raw, "max_retries", &c.MaxRetries)
	d.bool(raw, "strict_tabs", &c.StrictTabs)
	d.bool(raw, "strict_short_arrays", &c.StrictShortArrays)
	d.int(raw, "token_budget_alert", &c.TokenBudgetAlert)
	d.int(raw, "context_lines", &c.ContextLines)
	d.int(raw, "max_slice_lines", &c.MaxSliceLines)
	d.int(raw, "timeout_seconds", &c.TimeoutSeconds)
	if m := d.sub(raw, "sandbox"); m != nil {
		d.str(m, "dir", &c.Sandbox.Dir)
		d.str(m, "on_failure", &c.Sandbox.OnFailure)
		d.strs(m, "link", &c.Sandbox.Link)
	}
	if m := d.sub(raw, "guard"); m != nil {
		d.bool(m, "enabled", &c.Guard.Enabled)
		d.strs(m, "disable", &c.Guard.Disable)
	}
	if m := d.sub(raw, "llm"); m != nil {
		d.str(m, "provider", &c.LLM.Provider)
		d.str(m, "model", &c.LLM.Model)
		d.str(m, "endpoint", &c.LLM.Endpoint)
		d.str(m, "api_key_env", &c.LLM.APIKeyEnv)
		d.int(m, "max_tokens", &c.LLM.MaxTokens)
		d.str(m, "command", &c.LLM.Command)
		d.int(m, "timeout_seconds", &c.LLM.TimeoutSeconds)
	}
	c.Fixers = d.steps(raw, "fixers")
	c.Checks = d.steps(raw, "checks")
	for k := range raw {
		switch k {
		case "version", "language", "max_retries", "strict_tabs", "strict_short_arrays", "token_budget_alert",
			"context_lines", "max_slice_lines", "timeout_seconds", "sandbox", "guard", "llm", "fixers", "checks":
		default:
			d.errs = append(d.errs, fmt.Sprintf("unknown top-level key %q", k))
		}
	}
	if len(d.errs) > 0 {
		sort.Strings(d.errs)
		return nil, errors.New(strings.Join(d.errs, "; "))
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// Validate checks value ranges and enumerations.
func (c *Config) Validate() error {
	var errs []string
	if c.MaxRetries < 0 || c.MaxRetries > 10 {
		errs = append(errs, "max_retries must be between 0 and 10")
	}
	if c.ContextLines < 0 || c.ContextLines > 200 {
		errs = append(errs, "context_lines must be between 0 and 200")
	}
	if c.MaxSliceLines < 10 {
		errs = append(errs, "max_slice_lines must be at least 10")
	}
	if c.TimeoutSeconds <= 0 {
		errs = append(errs, "timeout_seconds must be positive")
	}
	switch c.Sandbox.OnFailure {
	case "preserve", "prune":
	default:
		errs = append(errs, `sandbox.on_failure must be "preserve" or "prune"`)
	}
	if c.Sandbox.Dir == "" || filepath.IsAbs(c.Sandbox.Dir) || strings.Contains(c.Sandbox.Dir, "..") {
		errs = append(errs, "sandbox.dir must be a relative path inside the repository")
	}
	switch c.LLM.Provider {
	case "anthropic", "openai", "ollama", "command", "none", "":
	default:
		errs = append(errs, fmt.Sprintf("llm.provider %q is not one of anthropic, openai, ollama, command, none", c.LLM.Provider))
	}
	if c.LLM.Provider == "command" && strings.TrimSpace(c.LLM.Command) == "" {
		errs = append(errs, `llm.command is required when llm.provider is "command"`)
	}
	for _, group := range []struct {
		label string
		steps []Step
	}{{"fixers", c.Fixers}, {"checks", c.Checks}} {
		seen := map[string]bool{}
		for i, s := range group.steps {
			if s.Name == "" || strings.TrimSpace(s.Run) == "" {
				errs = append(errs, fmt.Sprintf("%s[%d]: name and run are required", group.label, i))
				continue
			}
			if seen[s.Name] {
				errs = append(errs, fmt.Sprintf("%s: duplicate step name %q", group.label, s.Name))
			}
			seen[s.Name] = true
			if strings.HasPrefix(s.Run, "builtin:") {
				switch s.Run {
				case "builtin:tabs", "builtin:short-arrays":
					if group.label != "fixers" {
						errs = append(errs, fmt.Sprintf("%s: %s is a fixer", s.Name, s.Run))
					}
				case "builtin:standards":
					if group.label != "checks" {
						errs = append(errs, fmt.Sprintf("%s: %s is a check", s.Name, s.Run))
					}
				default:
					errs = append(errs, fmt.Sprintf("%s: unknown builtin %q", s.Name, s.Run))
				}
			}
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// SandboxPath returns the absolute sandbox parent directory.
func (c *Config) SandboxPath() string { return filepath.Join(c.Root, c.Sandbox.Dir) }

// ---- decoding helpers -------------------------------------------------------

type decoder struct{ errs []string }

func (d *decoder) sub(m map[string]any, k string) map[string]any {
	v, ok := m[k]
	if !ok || v == nil {
		return nil
	}
	s, ok := v.(map[string]any)
	if !ok {
		d.errs = append(d.errs, fmt.Sprintf("%q must be a mapping", k))
		return nil
	}
	return s
}

func (d *decoder) str(m map[string]any, k string, dst *string) {
	if v, ok := m[k]; ok && v != nil {
		switch t := v.(type) {
		case string:
			*dst = t
		case int:
			*dst = fmt.Sprint(t)
		case bool:
			*dst = fmt.Sprint(t)
		default:
			d.errs = append(d.errs, fmt.Sprintf("%q must be a string", k))
		}
	}
}

func (d *decoder) int(m map[string]any, k string, dst *int) {
	if v, ok := m[k]; ok && v != nil {
		n, ok := v.(int)
		if !ok {
			d.errs = append(d.errs, fmt.Sprintf("%q must be an integer", k))
			return
		}
		*dst = n
	}
}

func (d *decoder) bool(m map[string]any, k string, dst *bool) {
	if v, ok := m[k]; ok && v != nil {
		b, ok := v.(bool)
		if !ok {
			d.errs = append(d.errs, fmt.Sprintf("%q must be true or false", k))
			return
		}
		*dst = b
	}
}

func (d *decoder) strs(m map[string]any, k string, dst *[]string) {
	v, ok := m[k]
	if !ok || v == nil {
		return
	}
	list, ok := v.([]any)
	if !ok {
		d.errs = append(d.errs, fmt.Sprintf("%q must be a list", k))
		return
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		s, ok := it.(string)
		if !ok {
			d.errs = append(d.errs, fmt.Sprintf("%q must be a list of strings", k))
			return
		}
		out = append(out, s)
	}
	*dst = out
}

func (d *decoder) ints(m map[string]any, k string, dst *[]int) {
	v, ok := m[k]
	if !ok || v == nil {
		return
	}
	list, ok := v.([]any)
	if !ok {
		d.errs = append(d.errs, fmt.Sprintf("%q must be a list", k))
		return
	}
	out := make([]int, 0, len(list))
	for _, it := range list {
		n, ok := it.(int)
		if !ok {
			d.errs = append(d.errs, fmt.Sprintf("%q must be a list of integers", k))
			return
		}
		out = append(out, n)
	}
	*dst = out
}

func (d *decoder) steps(m map[string]any, k string) []Step {
	v, ok := m[k]
	if !ok || v == nil {
		return nil
	}
	list, ok := v.([]any)
	if !ok {
		d.errs = append(d.errs, fmt.Sprintf("%q must be a list", k))
		return nil
	}
	var out []Step
	for i, it := range list {
		sm, ok := it.(map[string]any)
		if !ok {
			d.errs = append(d.errs, fmt.Sprintf("%s[%d] must be a mapping", k, i))
			continue
		}
		var s Step
		d.str(sm, "name", &s.Name)
		d.str(sm, "run", &s.Run)
		d.str(sm, "kind", &s.Kind)
		d.strs(sm, "extensions", &s.Extensions)
		d.ints(sm, "ok_exit_codes", &s.OkExitCodes)
		d.bool(sm, "required", &s.Required)
		d.bool(sm, "shell", &s.Shell)
		for key := range sm {
			switch key {
			case "name", "run", "kind", "extensions", "ok_exit_codes", "required", "shell":
			default:
				d.errs = append(d.errs, fmt.Sprintf("%s[%d]: unknown key %q", k, i, key))
			}
		}
		for j := range s.Extensions {
			s.Extensions[j] = strings.ToLower(strings.TrimPrefix(s.Extensions[j], "."))
		}
		out = append(out, s)
	}
	return out
}

// ---- rendering --------------------------------------------------------------

// Render serialises the configuration as commented YAML that Parse can read back.
func (c *Config) Render() string {
	var b strings.Builder
	w := func(f string, a ...any) { fmt.Fprintf(&b, f, a...) }
	w("# AegisLoop configuration (see README.md for the full reference)\n")
	w("version: %d\n", c.Version)
	w("language: %s\n\n", c.Language)
	w("# Circuit breaker: maximum number of LLM heal attempts per run.\n")
	w("max_retries: %d\n", c.MaxRetries)
	w("# Print a warning when the estimated prompt tokens of one run exceed this.\n")
	w("token_budget_alert: %d\n", c.TokenBudgetAlert)
	w("# Lines of context kept around an error when no enclosing function is found.\n")
	w("context_lines: %d\n", c.ContextLines)
	w("# Enclosing blocks longer than this are cut to a window plus the signature.\n")
	w("max_slice_lines: %d\n", c.MaxSliceLines)
	w("timeout_seconds: %d\n\n", c.TimeoutSeconds)
	w("# Built-in standards (applied to added/changed lines only).\n")
	w("strict_tabs: %t\n", c.StrictTabs)
	w("strict_short_arrays: %t\n\n", c.StrictShortArrays)
	w("sandbox:\n  dir: %s\n  # preserve = keep the worktree for inspection when healing fails; prune = delete it.\n  on_failure: %s\n", c.Sandbox.Dir, c.Sandbox.OnFailure)
	if len(c.Sandbox.Link) > 0 {
		w("  # Symlinked from the main checkout so tools find their dependencies.\n  link: %s\n", inlineList(c.Sandbox.Link))
	}
	w("\nguard:\n  enabled: %t\n  disable: %s\n\n", c.Guard.Enabled, inlineList(c.Guard.Disable))
	w("# Healing model. provider: anthropic | openai | ollama | command | none\n")
	w("# With provider none, AegisLoop still runs the free local gates and returns a\n# compact fix request for the calling editor/agent (MCP mode).\n")
	w("llm:\n  provider: %s\n", c.LLM.Provider)
	if c.LLM.Model != "" {
		w("  model: %s\n", c.LLM.Model)
	}
	if c.LLM.Endpoint != "" {
		w("  endpoint: %s\n", c.LLM.Endpoint)
	}
	if c.LLM.APIKeyEnv != "" {
		w("  api_key_env: %s\n", c.LLM.APIKeyEnv)
	}
	if c.LLM.Command != "" {
		w("  command: %s\n", quote(c.LLM.Command))
	}
	w("  max_tokens: %d\n  timeout_seconds: %d\n", c.LLM.MaxTokens, c.LLM.TimeoutSeconds)
	renderSteps(&b, "fixers", "Zero-token local fixers. Run before every verification.", c.Fixers)
	renderSteps(&b, "checks", "Verification gates. Run after the fixers.", c.Checks)
	return b.String()
}

func renderSteps(b *strings.Builder, key, comment string, steps []Step) {
	fmt.Fprintf(b, "\n# %s\n", comment)
	if len(steps) == 0 {
		fmt.Fprintf(b, "%s: []\n", key)
		return
	}
	fmt.Fprintf(b, "%s:\n", key)
	for _, s := range steps {
		fmt.Fprintf(b, "  - name: %s\n    run: %s\n", s.Name, quote(s.Run))
		if s.Kind != "" {
			fmt.Fprintf(b, "    kind: %s\n", s.Kind)
		}
		if len(s.Extensions) > 0 {
			fmt.Fprintf(b, "    extensions: %s\n", inlineList(s.Extensions))
		}
		if len(s.OkExitCodes) > 0 {
			nums := make([]string, len(s.OkExitCodes))
			for i, n := range s.OkExitCodes {
				nums[i] = fmt.Sprint(n)
			}
			fmt.Fprintf(b, "    ok_exit_codes: [%s]\n", strings.Join(nums, ", "))
		}
		if s.Required {
			fmt.Fprintf(b, "    required: true\n")
		}
		if s.Shell {
			fmt.Fprintf(b, "    shell: true\n")
		}
	}
}

func inlineList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = s
		if strings.ContainsAny(s, ",[]#:'\" ") {
			q[i] = quote(s)
		}
	}
	return "[" + strings.Join(q, ", ") + "]"
}

func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
