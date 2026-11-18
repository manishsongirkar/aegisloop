# Security policy

## Supported versions

Only the latest release receives security fixes.

## Reporting a vulnerability

Please do not open a public issue. Use GitHub's private reporting:
**Security → Report a vulnerability** on https://github.com/manishsongirkar/aegisloop/security/advisories/new

Include the version (`aegis version`), steps to reproduce and the impact. You will get a reply within a few days.

## What AegisLoop sends and writes

- Code leaves your machine only to the LLM provider you configure, and only the failing slices plus stripped tool output.
  Use `ollama` or a `command` provider for fully local operation.
- API keys are read from environment variables and are never written to config or logs.
- Your working tree is written only after every gate passes, and never if any file changed during the run.
