# Changelog

All notable changes to acp-kit will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html)
once it leaves v0.

## [Unreleased]

## [0.1.1] - 2026-05-24

### Added
- `client.ReadOnlyPermissions` and `client.DenyAllPermissions` — built-in
  policies promoted from `poe-acp`. (The published v0.1.0 tag was cut before
  the implementation landed; v0.1.1 is the first tag that actually carries
  this code.)

### Changed
- Internal cleanup: collapsed the per-call-site `must*` panic helpers in
  `client` and `attachments` into a single `mustNot(err, label)` per
  package. No public API impact.

### Removed
- `client/auth` sub-package. The `AuthMethod` and `AuthResult` types it
  defined are now declared directly in the `client` package; the names
  consumers used (`client.AuthMethod`, `client.AuthResult`) are unchanged.
  Direct importers of `github.com/kfet/acp-kit/client/auth` would break,
  but neither `poe-acp` nor `slack-acp` imported it.

## [0.1.0] - 2026-05-22

### Added
- Initial extraction of shared ACP relay primitives from `poe-acp` and `slack-acp`.
- `client` — stdio ACP child process client (`Start`, sessions, prompts, caps, model selection, auth, fs callbacks). Built-in permission policies: `AllowAllPermissions`, `ReadOnlyPermissions` (heuristic — rejects titles containing write/edit/bash/exec/run/delete/rm), `DenyAllPermissions`.
- `client/auth` — auth method/result schema.
- `state` — conversation-key → ACP-session manager with stable cwd allocation, best-effort resume, idle GC, and two-regime system-prompt fallback.
- `attachments` — `os.Root`-sandboxed attachment store + ACP `ResourceLink` / embedded text `Resource` block builders.
- `skills` — embedded + host fir-style skill loader and `<available_skills>` catalog formatter.
- `sysprompt` — base/extra/catalog composer with disabled toggle.
- `paths` — XDG state/config path resolvers.
- `log` — opt-in `atomic.Bool` debug logger.
- `Makefile`, `.covignore`, and `make`-driven 100% coverage gate via covgate. `make` runs `fmt`, `tidy`, `vet`, race+cover, e2e, and license check.
