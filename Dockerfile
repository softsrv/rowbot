# ── Stage 1: Builder ─────────────────────────────────────────────────────────
FROM golang:1.27-alpine AS builder

WORKDIR /build

# Download dependencies first (layer cache)
COPY go.mod go.sum ./
RUN go mod download

# Download Tailwind CSS standalone CLI (no Node/npm required)
RUN apk add --no-cache curl libstdc++ && \
    ARCH=$(uname -m) && \
    if [ "$ARCH" = "aarch64" ]; then \
        TW_ARCH="arm64-musl"; \
        TW_HASH="9c106e815ce7ea99a65f91c13be2c51f00388dd3c0127c7a13a38f76cd1e26aa"; \
    else \
        TW_ARCH="x64-musl"; \
        TW_HASH="d861210e7c772e3b8a8b302d51bd4c8e2ab2e6f5dcb7545b16da6e9676c99079"; \
    fi && \
    curl -fsSL "https://github.com/tailwindlabs/tailwindcss/releases/download/v4.3.0/tailwindcss-linux-${TW_ARCH}" \
         -o /usr/local/bin/tailwindcss && \
    echo "${TW_HASH}  /usr/local/bin/tailwindcss" | sha256sum -c && \
    chmod +x /usr/local/bin/tailwindcss

# Build Tailwind CSS
COPY web/static/css/app.css ./web/static/css/app.css
COPY web/static/css/daisyui.mjs ./web/static/css/daisyui.mjs
COPY web/static/css/daisyui-theme.mjs ./web/static/css/daisyui-theme.mjs
COPY web/templates ./web/templates
RUN /usr/local/bin/tailwindcss -i ./web/static/css/app.css \
                               -o ./web/static/css/dist/app.css \
                               --minify

# Compile Go binary
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build \
      -ldflags="-s -w" \
      -o /build/bin/app \
      ./cmd/app

# ── Stage 2: Runtime ─────────────────────────────────────────────────────────
FROM gcr.io/distroless/static:latest

# APP_ENV controls JSON logging, secure cookies, and debug-detail suppression
# (see cmd/app/main.go). Bakes a default into the image via --build-arg
# APP_ENV=production; still overridable at `docker run -e APP_ENV=...`.
ARG APP_ENV=production
ENV APP_ENV=${APP_ENV}

WORKDIR /app

# Create non-root user
# (distroless has uid 65532 as "nonroot"; we use that)
USER 65532:65532

# Copy compiled binary
COPY --from=builder --chown=65532:65532 /build/bin/app ./app

# Copy web assets (templates + static files)
COPY --from=builder --chown=65532:65532 /build/web ./web

EXPOSE 8080

ENTRYPOINT ["./app"]
