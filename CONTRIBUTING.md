# Contributing to AegisLoop

Thanks for helping! AegisLoop is a small, dependency-free Go program; the bar is "boring, tested, predictable".

## Setup

```bash
git clone <repo> && cd aegisloop
make check        # gofmt check, go vet, go test -race
make build        # bin/aegis (+ bin/aegisloop)
```

Requirements: Go ≥ 1.22 and git ≥ 2.20. PHP, Node and the linters are *not* required to run the tests: the suites use
in-process fixers, shell-script fake tools and scripted fake LLMs. If `php` is installed, one test additionally runs `php -l`.

## Ground rules

- **Standard library only.** No new module dependencies (`go.mod` has none, on purpose).
- **Never write to the developer's working tree before verification passes.** Only `sandbox.Promote` may do it.
- **Every halt path needs a test.** The circuit breaker is tested with a fake LLM that never converges; keep it that way.
- **Fix bugs with a regression test first.** Mock projects live in `testdata/`; the WordPress VIP suite
  (`internal/healer/pipeline_test.go`) copies them into throw-away git repos.
- **Keep prompts small.** If you add context to a prompt, show the token impact (`go test -v` prints savings).
- Docs are tested: every ```` ```yaml ```` block in `README.md` must parse (`cmd/aegis/readme_test.go`).
- `gofmt` clean, `go vet` clean, no `panic` in library code, errors wrapped with `%w` and actionable.

## Where things live

| Area | Package | Tests |
|---|---|---|
| Config + detection + YAML subset | `internal/config` | `config_test.go` |
| Worktree lifecycle | `internal/sandbox` | `sandbox_test.go` (rollback under forced failure) |
| Fixers, guard, process execution | `internal/runner` | `runner_test.go` |
| Slicer, stripper, failure parser, prompts | `internal/tokenizer` | `tokenizer_test.go` (token-reduction assertions) |
| Loop, breaker, providers, pipeline | `internal/healer` | `healer_test.go`, `pipeline_test.go` |
| MCP server | `internal/mcp` | `server_test.go` |
| CLI | `cmd/aegis` | `main_test.go` (end-to-end with a script as the LLM) |

### Adding a standards rule
1. Add it to `Rules()` and implement it in `internal/runner/guard.go` (operate on the *masked* source).
2. Add positive **and** negative cases to `TestGuardPHPViolations` / `TestGuardJS`.
3. Document it in the README rules table.

### Adding a stack to auto-detection
Extend `Detection.Config` in `internal/config/detect.go` and add a `TestDetect…` case that also runs `Validate()`.

## Pull requests

Small, focused, with tests and a description of the behaviour change. Run `make check` first. By contributing you agree that
your work is licensed under the MIT License.
