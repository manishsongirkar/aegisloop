# Changelog

## 1.0.0 - 2026-11-18

First release.

- `aegis verify`, `run --patch`, `fix`, `prune`, `rules`, `clean`, `init` and an MCP server (`aegis mcp`).
- Isolated git worktree sandbox; the working tree is written only after every gate is green, and never if any file changed during the run.
- Zero-token local fixers (`phpcbf`, `eslint --fix`, `prettier`, `stylelint --fix`, `gofmt`, built-in tabs and short-array fixers).
- Built-in standards guard (WordPress escaping, SQL, input sanitization, secrets, debug leftovers, XSS sinks).
- AST-aware context slicing and noise-stripped failure output for small prompts.
- Failing tests also show the changed functions they call, so bugs in the source can be fixed.
- Bounded healing loop with a circuit breaker; providers: Anthropic, OpenAI-compatible, Ollama, or any command.
