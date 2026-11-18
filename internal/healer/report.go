package healer

import (
	"fmt"
	"strings"
)

// Format renders a human-readable summary of a pipeline run.
func Format(p *PipelineResult) string {
	if p == nil || p.Result == nil {
		return "no result"
	}
	var b strings.Builder
	switch p.Status {
	case StatusPassed:
		b.WriteString("✔ PASSED — every gate is green\n")
	case StatusNeedsFix:
		b.WriteString("✖ NEEDS FIX — local gates ran; remaining failures need a code change\n")
	default:
		fmt.Fprintf(&b, "✖ FAILED — %s\n", reasonText(p.Reason))
	}
	if len(p.LocallyFixed) > 0 {
		fmt.Fprintf(&b, "  zero-token local fixes: %s\n", strings.Join(p.LocallyFixed, ", "))
	}
	if len(p.Attempts) > 0 {
		fmt.Fprintf(&b, "  LLM heal attempts: %d\n", len(p.Attempts))
	}
	if p.Generated != nil {
		fmt.Fprintf(&b, "  generated edits in: %s\n", strings.Join(p.Generated.ChangedFiles, ", "))
	}
	if p.Tokens.Sent > 0 || p.Tokens.Baseline > 0 {
		fmt.Fprintf(&b, "  tokens: ~%d sent, ~%d received; a naive loop would send ~%d (%.0f%% saved)\n", p.Tokens.Sent, p.Tokens.Received, p.Tokens.Baseline, p.Tokens.SavedPercent)
	}
	b.WriteString("  steps:\n")
	for _, s := range p.Steps {
		mark := "·"
		switch s.Status {
		case "passed", "clean":
			mark = "✔"
		case "fixed":
			mark = "✎"
		case "failed":
			mark = "✖"
		case "warn":
			mark = "!"
		}
		line := fmt.Sprintf("    %s %-7s %-14s %s", mark, s.Phase, s.Name, s.Status)
		if s.Findings > 0 {
			line += fmt.Sprintf(" (%d)", s.Findings)
		}
		if s.Note != "" {
			line += " — " + s.Note
		}
		b.WriteString(line + "\n")
	}
	if len(p.Remaining) > 0 {
		b.WriteString("  remaining failures:\n")
		for i, f := range p.Remaining {
			if i == 15 {
				fmt.Fprintf(&b, "    … %d more\n", len(p.Remaining)-15)
				break
			}
			b.WriteString("    - " + f.String() + "\n")
		}
	}
	if len(p.Promoted) > 0 {
		fmt.Fprintf(&b, "  written to working tree: %s\n", strings.Join(p.Promoted, ", "))
	}
	if p.Preserved {
		fmt.Fprintf(&b, "  sandbox kept for inspection: %s (branch %s)\n", p.SandboxPath, p.Branch)
		if p.PatchPath != "" {
			fmt.Fprintf(&b, "  current patch saved: %s\n", p.PatchPath)
		}
		b.WriteString("  clean up with: aegis clean\n")
	}
	return b.String()
}

func reasonText(r string) string {
	switch r {
	case HaltMaxRetries:
		return "circuit breaker: max_retries exhausted"
	case HaltNoProgress:
		return "circuit breaker: the same failures persisted after a fix attempt (no progress)"
	case HaltThrashing:
		return "circuit breaker: thrashing — an earlier failure state came back"
	case HaltRegressing:
		return "circuit breaker: failures increased on consecutive attempts (regression)"
	case HaltUnusable:
		return "circuit breaker: the model's replies could not be applied twice in a row"
	case HaltInterrupted:
		return "interrupted"
	}
	return r
}
