# Agent Instructions

This file follows the [agents.md](https://agents.md) convention — a
tool-agnostic entry point for coding agents, as opposed to `CLAUDE.md`,
which is Claude Code–specific.

## Project instructions

Full build/test/architecture instructions live in `CLAUDE.md` at the repo
root. Read it before making changes — it covers commands (`make dev`,
`make test`, etc.), the layering conventions (`internal/http` →
`internal/app` → `internal/db`), auth design, and tech constraints
(Go stdlib first, `html/template` only, no frontend frameworks, sqlc for
all SQL).

## UI / template changes

Before creating, redesigning, or restyling anything in `web/templates/`,
read `docs/design-conventions.md`. It documents this codebase's established
visual patterns (daisyUI component usage, the icon+label+badge row
convention, page structure, a CSP gotcha around inline `onclick` handlers)
and a workflow for visually verifying a template change via a headless-Chrome
screenshot rather than reading the diff and guessing.

(Claude Code specifically will also auto-load this via
`.claude/skills/design-skill/`, which just points back here — this file is
the canonical copy.)
