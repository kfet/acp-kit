# Changelog

All notable changes to acp-kit will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it leaves v0.

## [Unreleased]

### Added
- Initial extraction of shared ACP relay primitives from `poe-acp` and `slack-acp`.
- `client` — stdio ACP child process client (`Start`, sessions, prompts, caps, model selection, auth, fs callbacks).
- `client/auth` — auth method/result schema.
- `state` — conversation-key → ACP-session manager with stable cwd allocation, best-effort resume, idle GC, and two-regime system-prompt fallback.
- `attachments` — `os.Root`-sandboxed attachment store + ACP `ResourceLink` / embedded text `Resource` block builders.
- `skills` — embedded + host fir-style skill loader and `<available_skills>` catalog formatter.
- `sysprompt` — base/extra/catalog composer with disabled toggle.
- `paths` — XDG state/config path resolvers.
- `log` — opt-in `atomic.Bool` debug logger.
- `Makefile`, `.covignore`, and `make`-driven 100% coverage gate via covgate. `make` runs `fmt`, `tidy`, `vet`, race+cover, e2e, and license check.
