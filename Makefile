APP_NAME         := rowbot
BIN_DIR          := ./bin
MODULE           := github.com/softsrv/rowbot
COVERAGE_MIN     := 37.2
# Mutation testing (make mutation-test). MUTATION_EFFICACY_MIN is a ratchet
# that only goes up: measured 46.34% at 3d05d5c, and gremlins fails at
# efficacy <= the bar, so the bar sits strictly below the measured value.
GREMLINS_VERSION      := v0.6.0
MUTATION_EFFICACY_MIN := 46
MUTATION_EXCLUDE      := -E '^internal/db/' -E '^cmd/'
MIGRATE_VERSION := v4.20.1
MIGRATE := go run -tags 'postgres' github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)

export TEST_DATABASE_URL DATABASE_URL

# Disposable Postgres for integration tests (make test-db-up / test-db-down).
# Throwaway credentials, a non-5432 host port so it never collides with the
# dev DATABASE_URL, and a *_test database name the guard accepts.
TEST_DB_CONTAINER := rowbot-test-db
TEST_DB_IMAGE     := postgres:16-alpine
TEST_DB_PORT      := 55432
TEST_DB_URL       := postgres://rowbot:rowbot@localhost:$(TEST_DB_PORT)/rowbot_test?sslmode=disable

# cmd/app, cmd/dbreset, and cmd/mockc2 each load ./.env themselves at startup
# (via godotenv) — this Makefile does not read .env.
# test-integration migrates through go run at the pinned MIGRATE_VERSION, so
# no install is needed; TEST_DATABASE_URL must name a disposable *_test
# database (check-test-database-url refuses anything else, and refuses one
# naming the same database as DATABASE_URL). The migrate-* targets keep the PATH binary:
# `migrate` is a third-party CLI, not our code, so it can't load .env itself
# — export DATABASE_URL in your shell first (e.g.
# `set -a && source .env && set +a`) before running them.

.PHONY: dev stop run build test test-integration test-db-migrate test-cover coverage-gate mutation-test fmt lint check \
        check-test-database-url test-db-guard test-db-up test-db-down \
        test-integration-local \
        daisyui-install tailwind tailwind-watch \
        migrate-up migrate-down migrate-create migrate-status check-database-url \
        sqlc-generate db-reset \
        docker-build docker-run prod release clean

## ── Development ─────────────────────────────────────────────────────────────

# Full hot-reload: Go (air) + Tailwind watch in parallel. trap 'kill 0'
# ensures Ctrl+C kills the entire process group, including air's spawned
# ./tmp/main grandchild that recursive make -j2 would leave behind.
#
# The one-shot `tailwind` build runs first, synchronously: air's first build
# embeds whatever's on disk in web/static/css/dist at that instant (go:embed
# is compile-time), and air's watcher excludes that directory (.air.toml), so
# it never rebuilds when tailwind's --watch process writes to it. Without this,
# a fresh checkout races air's first build against tailwind's first output —
# lose the race and the server embeds a build with no app.css until some other
# watched file change forces a rebuild.
dev: tailwind
	@trap 'kill 0' INT TERM; \
	  air & \
	  tailwindcss -i ./web/static/css/app.css -o ./web/static/css/dist/app.css --watch & \
	  wait

# Kill any stray dev processes left over from a previous session.
stop:
	@pkill -f './tmp/main' 2>/dev/null || true
	@pkill -f 'air$$' 2>/dev/null || true
	@pkill -f 'tailwindcss.*--watch' 2>/dev/null || true

air:
	air

run:
	go run ./cmd/app

build:
	go build -ldflags="-s -w" -o $(BIN_DIR)/$(APP_NAME) ./cmd/app

## ── Quality ──────────────────────────────────────────────────────────────────

test:
	go test ./...

# Guard first (refuses anything but a *_test TEST_DATABASE_URL), then
# migrations to the latest version, then the integration-tagged tests.
test-integration: check-test-database-url test-db-migrate
	go test -tags integration -count=1 ./...

# Proves check-test-database-url's refusals; needs no database.
test-db-guard:
	sh scripts/test-db-guard.sh

# One command for a local run: starts (or reuses) the disposable test
# database, then runs the guarded test-integration against it (guard, migrate,
# integration tests). Leave the container up for reruns; make test-db-down
# removes it.
test-integration-local: test-db-up
	@$(MAKE) --no-print-directory test-integration TEST_DATABASE_URL='$(TEST_DB_URL)'

# Starts (or reuses) a disposable Postgres container for integration tests,
# waits until it accepts connections (bounded: 30 tries, 1s apart), then
# prints the TEST_DATABASE_URL to export. To use it from your shell instead
# of make test-integration-local, paste this into the shell:
#   eval "$(make -s test-db-up)" && make test-integration
test-db-up:
	@set -e; \
	if docker container inspect $(TEST_DB_CONTAINER) >/dev/null 2>&1; then \
		docker start $(TEST_DB_CONTAINER) >/dev/null; \
	else \
		docker run -d --name $(TEST_DB_CONTAINER) \
		  -p 127.0.0.1:$(TEST_DB_PORT):5432 \
		  -e POSTGRES_USER=rowbot -e POSTGRES_PASSWORD=rowbot \
		  -e POSTGRES_DB=rowbot_test \
		  $(TEST_DB_IMAGE) >/dev/null; \
	fi; \
	i=0; \
	until docker exec $(TEST_DB_CONTAINER) pg_isready -h 127.0.0.1 -U rowbot -d rowbot_test >/dev/null 2>&1; do \
		i=$$((i + 1)); \
		if [ "$$i" -ge 30 ]; then \
			echo "test-db-up: Postgres in container $(TEST_DB_CONTAINER) not ready after 30 tries — see: docker logs $(TEST_DB_CONTAINER)" >&2; \
			exit 1; \
		fi; \
		sleep 1; \
	done; \
	echo "export TEST_DATABASE_URL='$(TEST_DB_URL)'"

# Removes the disposable test database container; succeeds if it is gone.
test-db-down:
	@docker rm -f $(TEST_DB_CONTAINER) >/dev/null 2>&1 || true

test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

# Mutation testing with gremlins over the unit-test build (no integration
# tag), skipping sqlc-generated internal/db and the cmd/* entry points.
# Fails when test efficacy is at or below MUTATION_EFFICACY_MIN. Set
# MUTATION_DIFF=<git ref> to mutate only code changed since that ref (CI does
# this against the PR base); without it the whole module runs (~18 min).
# The bar goes in through a throwaway --config file, not --threshold-efficacy:
# in gremlins v0.6.0 that flag reaches its config as a string, is read as 0,
# and so never fails the run (a config-file number is read correctly). A diff
# with no in-scope mutants makes gremlins report 0% and exit 10; the recipe
# turns that all-zero summary into a pass (NOT COVERED mutants still fail).
# gremlins isn't part of the Go toolchain — install the version pinned in
# GREMLINS_VERSION above once, e.g. for v0.6.0:
#   go install github.com/go-gremlins/gremlins/cmd/gremlins@v0.6.0
mutation-test:
	@echo "==> gremlins mutation test (required efficacy above $(MUTATION_EFFICACY_MIN)%)"
	@cfg="$$(mktemp)"; out="$$(mktemp)"; st="$$(mktemp)"; trap 'rm -f "$$cfg" "$$out" "$$st"' EXIT; \
	printf 'unleash:\n  threshold:\n    efficacy: %s\n' "$(MUTATION_EFFICACY_MIN)" > "$$cfg"; \
	{ gremlins unleash --config "$$cfg" $(MUTATION_EXCLUDE) $(if $(MUTATION_DIFF),--diff $(MUTATION_DIFF)) . 2>&1; echo $$? > "$$st"; } | tee "$$out"; \
	status=$$(cat "$$st"); \
	if [ $$status -eq 10 ] && grep -q 'Killed: 0, Lived: 0, Not covered: 0$$' "$$out" && grep -q 'Timed out: 0,' "$$out"; then \
		echo "no in-scope mutants to score — nothing to gate"; \
		status=0; \
	elif [ $$status -eq 10 ]; then \
		echo "mutation efficacy below required $(MUTATION_EFFICACY_MIN)%"; \
	elif [ $$status -ne 0 ]; then \
		echo "gremlins failed (exit $$status) — see the test or tool error above"; \
	fi; \
	exit $$status

coverage-gate:
	go test -coverprofile=coverage.out ./...
	@grep -v '^$(MODULE)/internal/db/' coverage.out > coverage.filtered.out
	@total=$$(go tool cover -func=coverage.filtered.out | awk '/^total:/{sub("%","",$$NF); print $$NF}'); \
	if awk -v t="$$total" -v m="$(COVERAGE_MIN)" 'BEGIN{exit !(t+0 >= m+0)}'; then \
		echo "coverage $$total% (minimum $(COVERAGE_MIN)%)"; \
	else \
		echo "coverage $$total% is below the required minimum $(COVERAGE_MIN)%"; \
		exit 1; \
	fi

fmt:
	gofmt -w .

lint:
	golangci-lint run

# Fast local sanity check: fails on the first problem found rather than
# collecting everything, so fix-and-rerun is quick. gofmt runs in check-only
# mode (-l lists, never rewrites) — use `make fmt` to actually fix formatting.
# staticcheck and govulncheck aren't part of the Go toolchain — install once:
#   go install honnef.co/go/tools/cmd/staticcheck@latest
#   go install golang.org/x/vuln/cmd/govulncheck@latest
check:
	@echo "==> go build"
	go build ./...
	@echo "==> gofmt (check only)"
	@fmtout="$$(gofmt -l .)"; \
	if [ -n "$$fmtout" ]; then \
		echo "$$fmtout"; \
		echo "gofmt: the file(s) above need formatting — run 'make fmt'"; \
		exit 1; \
	fi
	@echo "==> go vet"
	go vet ./...
	@echo "==> staticcheck"
	staticcheck ./...
	@echo "==> govulncheck"
	govulncheck ./...
	@echo "all checks passed"

## ── CSS ──────────────────────────────────────────────────────────────────────

# Download DaisyUI .mjs bundles next to app.css so the @plugin directive
# can resolve them. Re-run this whenever you upgrade DaisyUI.
daisyui-install:
	curl -sLo web/static/css/daisyui.mjs \
	  https://github.com/saadeghi/daisyui/releases/latest/download/daisyui.mjs
	curl -sLo web/static/css/daisyui-theme.mjs \
	  https://github.com/saadeghi/daisyui/releases/latest/download/daisyui-theme.mjs
	@echo "DaisyUI bundles downloaded to web/static/css/"

tailwind:
	tailwindcss -i ./web/static/css/app.css -o ./web/static/css/dist/app.css --minify

tailwind-watch:
	tailwindcss -i ./web/static/css/app.css -o ./web/static/css/dist/app.css --watch

## ── Database ─────────────────────────────────────────────────────────────────

# Two steps, so a failure names its cause: `migrate version` first proves the
# database is reachable (on a fresh database it exits 1 printing "no
# migration", which still means reachable); then `migrate up`, whose own error
# output is shown, so a bad migration is reported as a migration failure, not
# as a missing database.
test-db-migrate: check-test-database-url
	@out=$$($(MIGRATE) -path db/migrations -database "$$TEST_DATABASE_URL" version 2>&1) || \
	case "$$out" in \
		*"no migration"*) ;; \
		*) printf '%s\n' "$$out" | sed 's#[a-z]*://[^ ]*#<TEST_DATABASE_URL>#g' >&2; \
		   echo "test-db-migrate: test database unreachable (see the error above) — locally, start one with: make test-db-up (or run: make test-integration-local)" >&2; \
		   exit 1 ;; \
	esac
	@$(MIGRATE) -path db/migrations -database "$$TEST_DATABASE_URL" up || { \
		echo "test-db-migrate: migration failed against a reachable test database — see migrate's error above; fix the migration in db/migrations" >&2; \
		exit 1; \
	}

migrate-up: check-database-url
	migrate -path db/migrations -database "$(DATABASE_URL)" up

migrate-down: check-database-url
	migrate -path db/migrations -database "$(DATABASE_URL)" down 1

migrate-create:
	@test -n "$(NAME)" || (echo "Usage: make migrate-create NAME=<name>" && exit 1)
	migrate create -ext sql -dir db/migrations -seq $(NAME)

migrate-status: check-database-url
	migrate -path db/migrations -database "$(DATABASE_URL)" version

check-database-url:
	@test -n "$(DATABASE_URL)" || (echo "DATABASE_URL is not set — export it first, e.g.: set -a && source .env && set +a" && exit 1)

# Refuses to let integration tests touch anything but a disposable database:
# TEST_DATABASE_URL must be set, be a postgres:// or postgresql:// URL, name a
# database ending in _test, and not name the same database (user, host, port,
# name — parsed, not string-compared) as DATABASE_URL. It reads the
# environment (a make command-line value is exported too) and never prints
# either URL, which may hold a password. The rules live in
# scripts/check-test-database-url.sh.
check-test-database-url:
	@sh scripts/check-test-database-url.sh

sqlc-generate:
	sqlc generate -f db/sqlc.yaml

# Truncates every application table in DATABASE_URL, for wiping local dev
# data clean. Refuses to run unless APP_ENV=development.
db-reset:
	go run ./cmd/dbreset

## ── Docker ───────────────────────────────────────────────────────────────────

docker-build:
	docker build -t $(APP_NAME):dev .

docker-run:
	docker run --rm -d \
	  -v $(CURDIR)/.env:/app/.env:ro \
	  -p $(or $(PORT),8080):$(or $(PORT),8080) \
	  $(APP_NAME):dev

prod:
	docker build -t $(APP_NAME):prod --build-arg APP_ENV=production .

## ── Release ──────────────────────────────────────────────────────────────────

# Cuts a release: regenerates CHANGELOG.md from the commits since the last
# tag, commits and pushes that to main, then tags and pushes the tag (which
# triggers release.yml to build, publish, and deploy). Usage:
#   make release v1.2.0
# The version is a positional argument, not VERSION=v1.2.0 — picked up via
# MAKECMDGOALS and passed through by the "v%:" no-op pattern rule below, so
# make doesn't try (and fail) to build a target literally named "v1.2.0".
release:
	@set -e; \
	version="$(filter-out release,$(MAKECMDGOALS))"; \
	if [ -z "$$version" ]; then \
		echo "usage: make release vX.Y.Z"; exit 1; \
	fi; \
	if ! echo "$$version" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$$'; then \
		echo "release: '$$version' is not a valid version — expected vX.Y.Z (e.g. v1.2.0)"; exit 1; \
	fi; \
	if git rev-parse "$$version" >/dev/null 2>&1; then \
		echo "release: tag $$version already exists"; exit 1; \
	fi; \
	if [ -n "$$(git status --porcelain)" ]; then \
		echo "release: working tree is not clean — commit or stash changes first"; exit 1; \
	fi; \
	branch="$$(git rev-parse --abbrev-ref HEAD)"; \
	if [ "$$branch" != "main" ]; then \
		echo "release: must be run from main (currently on $$branch)"; exit 1; \
	fi; \
	git fetch origin main --quiet; \
	if [ "$$(git rev-parse HEAD)" != "$$(git rev-parse origin/main)" ]; then \
		echo "release: local main has diverged from origin/main — pull/push first"; exit 1; \
	fi; \
	last_tag="$$(git describe --tags --abbrev=0 2>/dev/null || true)"; \
	if [ -n "$$last_tag" ]; then range="$$last_tag..HEAD"; else range="HEAD"; fi; \
	log="$$(git log $$range --no-merges --pretty=format:'- %s (%h)')"; \
	if [ -z "$$log" ]; then \
		echo "release: no commits since $${last_tag:-the initial commit} — nothing to release"; exit 1; \
	fi; \
	tmp="$$(mktemp)"; \
	{ \
		echo "## $$version - $$(date +%Y-%m-%d)"; \
		echo; \
		echo "$$log"; \
		echo; \
		[ -f CHANGELOG.md ] && cat CHANGELOG.md; \
	} > "$$tmp"; \
	mv "$$tmp" CHANGELOG.md; \
	git add CHANGELOG.md; \
	git commit -m "docs: changelog for $$version"; \
	git push origin main; \
	git tag "$$version"; \
	git push origin "$$version"; \
	echo "released $$version"

# Lets "make release v1.2.0" pass through without make complaining that
# "v1.2.0" isn't a real target — swallowed here as a no-op. Scoped to goals
# starting with "v" so a genuine typo of another target name still errors
# normally instead of silently doing nothing.
v%:
	@:

## ── Clean ────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(BIN_DIR) tmp/
