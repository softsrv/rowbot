# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Commands

```bash
make dev              # Hot-reload: air (Go) + Tailwind watch, in parallel
make run              # Run without hot-reload
make build            # Compile to ./bin/app
make test             # Run unit tests
make test-integration # Run integration tests (requires Docker; uses testcontainers)
make test-cover       # Run tests with HTML coverage report
make coverage-gate    # Run unit tests and enforce coverage threshold (>= COVERAGE_MIN)
make fmt              # gofmt the repo
make lint             # golangci-lint run

# Database
make migrate-up                   # Apply pending migrations
make migrate-down                 # Roll back last migration
make migrate-create NAME=<name>   # Create new migration pair
make migrate-status               # Show current migration version
make sqlc-generate                # Regenerate internal/db/ from db/queries/

# CSS
make daisyui-install  # Download DaisyUI .mjs bundles (re-run on DaisyUI upgrades)
make tailwind         # One-shot CSS build
make tailwind-watch   # Watch mode (called by make dev)

# Docker
make docker-build     # Build dev image
make prod             # Build production image (multi-stage, distroless, non-root)
```

`make coverage-gate` measures unit-test coverage only (excluding sqlc-generated `internal/db/`; untested hand-written packages like `cmd/*` and `internal/oauth` count as 0%). It enforces a minimum coverage bar of 37.2%, configured in `COVERAGE_MIN` in the `Makefile`. `COVERAGE_MIN` is a ratchet that only goes up, and CI blocks any pull request below it.

To run a single test:
```bash
go test ./internal/auth/... -run TestJWTRoundTrip
go test -tags integration ./internal/app/... -run TestAuthServiceIntegration
```

## Architecture

`cmd/app/main.go` is bootstrap-only: config parsing, DB pool construction, dependency wiring, HTTP server start, and graceful shutdown (SIGINT/SIGTERM → 30s drain → pool close). No business logic lives here.

### Layer flow

```
HTTP handlers (internal/http/handlers/)
    ↓
Service layer (internal/app/)          ← business logic + orchestration
    ↓
DB layer (internal/db/)                ← sqlc-generated, pgx/v5 driver
    ↑
Middleware (internal/http/middleware/)  ← auth, rate limiting, security headers, body limit, logging, request ID
```

### Key packages

| Package | Purpose |
|---|---|
| `internal/app/` | `AuthService` and `UserService` — session lifecycle, Discord OAuth account provisioning |
| `internal/auth/` | JWT issue/validate, token hashing (SHA-256), refresh/verification token generation |
| `internal/db/` | sqlc-generated repository code; never edit by hand — regenerate with `make sqlc-generate` |
| `internal/http/` | Router wiring in `router.go`; handlers and middleware in sub-packages |
| `internal/users/` | Email normalization |

### Auth design

- **Access tokens**: JWT (HS256), 15-minute lifetime, delivered as `access_token` httpOnly cookie and in response body.
- **Refresh tokens**: 32-byte random, SHA-256 hashed before DB storage, 30-day lifetime, `refresh_token` httpOnly cookie. **No rotation** — `Refresh` issues a new access token and reuses the same refresh token, updating `last_used_at` and device metadata in place.
- **Cookies**: `SameSite=Lax`, httpOnly, `Secure` in production. No CSRF tokens — CSRF is mitigated by SameSite=Lax plus same-origin form posts.
- **Account creation and login**: Discord OAuth only — no passwords anywhere in the system.
- **Token cleanup**: background goroutine fires daily at 03:00; purges expired/revoked tokens per retention policy.

### Template rendering

`main.go` parses `base.html` once at startup into a base `*template.Template`. `handlers.TemplateRenderer` clones this base and parses the page-specific `.html` file on each request. This avoids the `{{define "content"}}` global-overwrite problem in Go's template engine when multiple page templates share a name.

### Middleware chain (outermost → innermost)

```
RequestID → Logging → SecurityHeaders → BodyLimit → mux
```

`BodyLimit` caps request bodies at 1 MiB (`DefaultMaxBodyBytes`) via `http.MaxBytesReader`. Auth middleware (`middleware.Authenticate`) is applied per-route, not globally.

### Rate limiting

In-memory (`sync.Map`), not shared across instances. Key functions: `IPKeyFunc`, `CookieRefreshTokenKeyFunc`. Limits: refresh 10/min (refresh token hash), Discord OAuth login/callback 10/15min (IP).

## Database

Migrations in `db/migrations/` (golang-migrate, run manually via `make migrate-up` — never on startup). SQL queries in `db/queries/` — edit these, then run `make sqlc-generate` to regenerate `internal/db/`.

pgxpool config: max 25 conns (`DB_MAX_CONNS`), min 5, max lifetime 1h, idle timeout 10min, health check 1min.

## Environment

Copy `.env.example` to `.env`. Required vars: `DATABASE_URL`, `JWT_SECRET` (≥32 bytes), `APP_BASE_URL`. `APP_ENV=production` gates JSON logging, secure cookies, and suppresses debug error detail.

`cmd/app`, `cmd/dbreset`, and `cmd/mockc2` each load `./.env` themselves at startup via `godotenv` — the Makefile does not read or export it, and `cmd/app`/`cmd/dbreset` fail fast if the file is missing (`cmd/mockc2` is best-effort, since it's a standalone testing tool with its own CLI flags). `godotenv.Load` never overrides a variable already present in the process environment, so real deployment env vars always win over the file. The path defaults to `.env` (repo root locally, `/app` inside the container per the Dockerfile's `WORKDIR`) but is overridable via `ENV_FILE_PATH` — needed on platforms like Cloud Run, where mounting a secret volume directly at `/app` would shadow the compiled binary and other files baked into that same directory; point `ENV_FILE_PATH` at a dedicated mount directory instead (e.g. `/secrets/.env`). Docker never bakes `.env` into the image (see `.dockerignore`); `make docker-run` bind-mounts it read-only to `/app/.env` at container start instead. The `migrate-*` Makefile targets are the one exception — `migrate` is a third-party CLI that can't load `.env` itself, so `DATABASE_URL` must already be exported in your shell before running them.

## Tech constraints

- Go stdlib first; propose libraries with rationale before adding.
- `html/template` only (not `text/template`) — auto-escaping prevents XSS.
- No frontend frameworks; HTMX for dynamics.
- Parameterized SQL only (sqlc enforces this).
- UUIDv7 via `github.com/google/uuid`.
