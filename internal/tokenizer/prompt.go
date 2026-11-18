package tokenizer

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// EstimateTokens is a cheap, provider-independent token estimate (≈4 bytes per
// token for code). It is used for budgeting and savings reports, not billing.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

// Payload is the minimal, structured request sent to the model (or returned to
// a calling editor over MCP when no model is configured).
type Payload struct {
	Task        string    `json:"task,omitempty"`
	Attempt     int       `json:"attempt"`
	MaxAttempts int       `json:"max_attempts"`
	Failures    []Failure `json:"failures"`
	Output      string    `json:"output,omitempty"` // noise-stripped tool output (only for unlocated failures)
	Slices      []Slice   `json:"slices"`
}

// SliceBlock is a Slice with its text, used for JSON rendering.
type SliceBlock struct {
	Slice
	Code string `json:"code"`
}

// MarshalJSON emits slices together with their numbered code.
func (p Payload) MarshalJSON() ([]byte, error) {
	type alias Payload
	blocks := make([]SliceBlock, len(p.Slices))
	for i, s := range p.Slices {
		blocks[i] = SliceBlock{Slice: s, Code: s.Numbered()}
	}
	return json.Marshal(struct {
		alias
		Slices []SliceBlock `json:"slices"`
	}{alias(p), blocks})
}

// ReplyFormat documents the edit syntax the healer understands.
const ReplyFormat = "Reply with ONLY edit blocks, no prose. Each block replaces an inclusive range of file lines\n" +
	"(use the line numbers shown, never edit outside the shown ranges):\n\n" +
	"```aegis-replace path/to/file.php 12-30\n" +
	"<new code for lines 12-30, exactly as it should appear, using the project's indentation>\n" +
	"```\n\n" +
	"Use a single line number (`12`) to replace one line; leave the body empty to delete the range.\n" +
	"Do not include the `NNN|` line-number gutter in your code."

// Markdown renders the payload as a compact prompt body.
func (p Payload) Markdown() string {
	var b strings.Builder
	if p.Task != "" {
		fmt.Fprintf(&b, "## Task\n%s\n\n", strings.TrimSpace(p.Task))
	}
	fmt.Fprintf(&b, "## Failures (heal attempt %d of %d)\n", p.Attempt, p.MaxAttempts)
	if len(p.Failures) == 0 {
		b.WriteString("_No located failures; see tool output below._\n")
	}
	for _, f := range p.Failures {
		b.WriteString("- " + f.String() + "\n")
	}
	if strings.TrimSpace(p.Output) != "" {
		fmt.Fprintf(&b, "\n## Tool output (noise stripped)\n```\n%s\n```\n", p.Output)
	}
	if len(p.Slices) > 0 {
		b.WriteString("\n## Code (only the failing regions)\n")
		slices := append([]Slice{}, p.Slices...)
		sort.SliceStable(slices, func(i, j int) bool {
			if slices[i].Path != slices[j].Path {
				return slices[i].Path < slices[j].Path
			}
			return slices[i].Start < slices[j].Start
		})
		for _, s := range slices {
			sig := ""
			if s.Signature != "" {
				sig = " — `" + s.Signature + "`"
			}
			fmt.Fprintf(&b, "\n### %s lines %d-%d (%s)%s\n```\n%s```\n", s.Path, s.Start, s.End, s.Kind, sig, s.Numbered())
		}
	}
	b.WriteString("\n## Reply format\n" + ReplyFormat + "\n")
	return b.String()
}

// SystemPrompt returns the fixed system prompt. rules lists the project's
// enforced standards as short imperative sentences.
func SystemPrompt(rules []string) string {
	var b strings.Builder
	b.WriteString("You are AegisLoop's repair engine. You receive failing lint/test/build/security output and the smallest ")
	b.WriteString("code regions needed to fix it. Make the minimal change that resolves every listed failure without ")
	b.WriteString("changing unrelated behaviour, and without weakening tests or suppressing rules (no phpcs:ignore, eslint-disable, @ts-ignore).\n")
	if len(rules) > 0 {
		b.WriteString("\nProject standards you must follow:\n")
		for _, r := range rules {
			b.WriteString("- " + r + "\n")
		}
	}
	b.WriteString("\n" + ReplyFormat + "\n")
	return b.String()
}

// Baseline estimates the tokens a naive retry loop would send: whole files
// plus the raw, unfiltered tool output.
func Baseline(files map[string]string, rawOutput string) int {
	n := EstimateTokens(rawOutput)
	for _, c := range files {
		n += EstimateTokens(c)
	}
	return n
}
