APP_NAME         := rowbot
BIN_DIR          := ./bin
MODULE           := github.com/softsrv/rowbot

# cmd/app, cmd/dbreset, and cmd/mockc2 each load ./.env themselves at startup
# (via godotenv) — this Makefile no longer reads or exports it. The one
# exception is the migrate-* targets below: `migrate` is a third-party CLI,
# not our code, so it can't load .env itself — export DATABASE_URL in your
# shell first (e.g. `set -a && source .env && set +a`) before running them.

.PHONY: dev stop run build test fmt lint check \
        daisyui-install tailwind tailwind-watch \
        migrate-up migrate-down migrate-create migrate-status check-database-url \
        sqlc-generate db-reset \
        docker-build docker-run prod clean

## ── Development ─────────────────────────────────────────────────────────────

# Full hot-reload: Go (air) + Tailwind watch in parallel. trap 'kill 0'
# ensures Ctrl+C kills the entire process group, including air's spawned
# ./tmp/main grandchild that recursive make -j2 would leave behind.
dev:
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

test-integration:
	go test -tags integration ./...

test-cover:
	go test -coverprofile=coverage.out ./...
	go tool cover -html=coverage.out

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

## ── Clean ────────────────────────────────────────────────────────────────────

clean:
	rm -rf $(BIN_DIR) tmp/
