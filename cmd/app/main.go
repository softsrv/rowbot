package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joho/godotenv"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/discord"
	internalhttp "github.com/softsrv/rowbot/internal/http"
	"github.com/softsrv/rowbot/internal/http/handlers"
	oauthpkg "github.com/softsrv/rowbot/internal/oauth"
	"github.com/softsrv/rowbot/internal/secrets"
	"github.com/softsrv/rowbot/web"
)

// defaultEnvFilePath is where the app expects its .env file when ENV_FILE_PATH
// isn't set, resolved relative to the working directory: the repo root when
// run locally (go run, air), and /app inside the container (Dockerfile's
// WORKDIR). Override via ENV_FILE_PATH when that default collides with
// something else mounted into the same directory — e.g. Cloud Run secret
// volumes replace the entire directory they're mounted at, so mounting one
// directly over /app would shadow the compiled binary itself.
const defaultEnvFilePath = ".env"

func main() {
	envFilePath := os.Getenv("ENV_FILE_PATH")
	if envFilePath == "" {
		envFilePath = defaultEnvFilePath
	}
	if err := godotenv.Load(envFilePath); err != nil {
		slog.Error("load env file", "path", envFilePath, "error", err)
		os.Exit(1)
	}

	cfg := mustLoadConfig()
	setupLogger(cfg.AppEnv)

	slog.Info("starting", "env", cfg.AppEnv, "port", cfg.Port)

	// ── Database ──────────────────────────────────────────────────────────────
	if cfg.DBMinConns > cfg.DBMaxConns {
		slog.Error("DB_MIN_CONNS must not exceed DB_MAX_CONNS",
			"min", cfg.DBMinConns, "max", cfg.DBMaxConns)
		os.Exit(1)
	}

	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		slog.Error("parse database url", "error", err)
		os.Exit(1)
	}
	poolCfg.MaxConns = int32(cfg.DBMaxConns)
	poolCfg.MinConns = int32(cfg.DBMinConns)
	poolCfg.MaxConnLifetime = time.Hour
	poolCfg.MaxConnIdleTime = 10 * time.Minute
	poolCfg.HealthCheckPeriod = time.Minute

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		slog.Error("connect to database", "error", err)
		os.Exit(1)
	}

	queries := db.New(pool)

	// ── Background context (token cleanup + rate-limiter/cache sweepers) ─────
	// cleanupCtx is cancelled after the HTTP server drains, giving background
	// goroutines a clean signal to stop without racing active requests.
	cleanupCtx, stopCleanup := context.WithCancel(context.Background())

	// ── Services ──────────────────────────────────────────────────────────────
	authSvc := app.NewAuthService(queries, pool, app.AuthServiceConfig{
		JWTSecret:     cfg.JWTSecret,
		AccessExpiry:  cfg.JWTAccessExpiry,
		RefreshExpiry: cfg.RefreshTokenExpiry,
		AppBaseURL:    cfg.AppBaseURL,
	})
	userSvc := app.NewUserService(queries)
	discordSvc := app.NewDiscordService(queries, cfg.DiscordBotToken, nil)

	// ── OAuth ─────────────────────────────────────────────────────────────────
	var encrypter *secrets.Encrypter
	if len(cfg.OAuthTokenEncKey) == 32 {
		var encErr error
		encrypter, encErr = secrets.New(cfg.OAuthTokenEncKey)
		if encErr != nil {
			slog.Error("build token encrypter", "error", encErr)
			os.Exit(1)
		}
	}

	baseURL := strings.TrimRight(cfg.AppBaseURL, "/")

	var discordClient *oauthpkg.DiscordClient
	var discordAuthorizeURL func(string) string
	var discordSilentAuthorizeURL func(string) string
	var discordLinkAuthorizeURL func(string) string
	var discordBotInstallURL string
	if cfg.DiscordClientID != "" {
		discordClient = oauthpkg.NewDiscordClient(
			cfg.DiscordClientID,
			cfg.DiscordClientSecret,
			baseURL+"/auth/discord/callback",
			nil,
		)
		discordAuthorizeURL = discordClient.AuthorizeURL
		discordSilentAuthorizeURL = discordClient.SilentAuthorizeURL

		discordBotInstallClient := oauthpkg.NewDiscordClient(
			cfg.DiscordClientID,
			cfg.DiscordClientSecret,
			baseURL+"/auth/discord/bot-install/callback",
			nil,
		)
		discordBotInstallURL = discordBotInstallClient.BotInstallURL(cfg.DiscordBotPermissions)

		discordLinkClient := oauthpkg.NewDiscordClient(
			cfg.DiscordClientID,
			cfg.DiscordClientSecret,
			baseURL+"/auth/discord/link/callback",
			nil,
		)
		discordLinkAuthorizeURL = discordLinkClient.AuthorizeURL
	}

	var concept2Client *oauthpkg.Concept2Client
	var concept2AuthorizeURL func(string) string
	if cfg.Concept2ClientID != "" {
		if encrypter == nil {
			slog.Error("OAUTH_TOKEN_ENC_KEY is required when Concept2 OAuth is enabled")
			os.Exit(1)
		}
		concept2Client = oauthpkg.NewConcept2Client(
			cfg.Concept2ClientID,
			cfg.Concept2ClientSecret,
			baseURL+"/auth/concept2/link/callback",
			cfg.Concept2APIBase,
			nil,
		)
		concept2AuthorizeURL = concept2Client.AuthorizeURL
	}

	var oauthSvc *app.OAuthService
	if discordClient != nil || concept2Client != nil {
		oauthSvc = app.NewOAuthService(cleanupCtx, queries, pool, discordClient, concept2Client, encrypter, authSvc)
	}

	var rowingSvc *app.RowingService
	if concept2Client != nil && encrypter != nil {
		rowingSvc = app.NewRowingService(queries, concept2Client, encrypter, cfg.DiscordBotToken, nil)
	}

	// ── Discord interactions handler ───────────────────────────────────────────
	var discordInteractionsH *handlers.DiscordHandler
	if cfg.DiscordPublicKey != "" {
		pubKeyBytes, hexErr := hex.DecodeString(cfg.DiscordPublicKey)
		if hexErr != nil || len(pubKeyBytes) != ed25519.PublicKeySize {
			slog.Error("DISCORD_PUBLIC_KEY must be a 64-character hex-encoded Ed25519 public key")
			os.Exit(1)
		}
		discordInteractionsH = handlers.NewDiscordHandler(ed25519.PublicKey(pubKeyBytes), cfg.DiscordBotToken, nil, discordSvc)
	}

	if discordInteractionsH != nil && cfg.DiscordBotToken != "" {
		regCtx, regCancel := context.WithTimeout(context.Background(), 10*time.Second)
		regErr := discord.RegisterCommands(regCtx, nil, cfg.DiscordBotToken, cfg.DiscordApplicationID, discord.Commands)
		regCancel()
		if regErr != nil {
			slog.Error("discord command registration failed", "error", regErr)
			// Non-fatal: log and continue. Previously-registered commands remain
			// active in Discord until the next successful registration.
		} else {
			slog.Info("discord commands registered", "count", len(discord.Commands))
		}
	}

	// ── Templates ─────────────────────────────────────────────────────────────
	// Build a base template containing only the layout and shared partials.
	// Page templates are NOT loaded here — the TemplateRenderer clones this
	// base and parses the page-specific file per-request, preventing the
	// {{define "content"}} global-overwrite gotcha in Go's template engine.
	baseTmpl, err := template.ParseFS(web.FS, "templates/base.html")
	if err != nil {
		slog.Error("parse base template", "error", err)
		os.Exit(1)
	}
	if _, parseErr := baseTmpl.ParseFS(web.FS, "templates/partials/*.html"); parseErr != nil {
		slog.Warn("parse partial templates", "error", parseErr)
	}

	renderer := handlers.NewTemplateRenderer(baseTmpl, web.FS, "templates")

	// ── Router ────────────────────────────────────────────────────────────────
	handler := internalhttp.NewRouter(cleanupCtx, internalhttp.RouterConfig{
		Queries:                   queries,
		Pool:                      pool,
		AuthSvc:                   authSvc,
		UserSvc:                   userSvc,
		OAuthSvc:                  oauthSvc,
		DiscordAuthorizeURL:       discordAuthorizeURL,
		DiscordSilentAuthorizeURL: discordSilentAuthorizeURL,
		DiscordLinkAuthorizeURL:   discordLinkAuthorizeURL,
		DiscordBotInstallURL:      discordBotInstallURL,
		DiscordInteractions:       discordInteractionsH,
		DiscordSvc:                discordSvc,
		DiscordBotToken:           cfg.DiscordBotToken,
		Concept2AuthorizeURL:      concept2AuthorizeURL,
		RowingSvc:                 rowingSvc,
		Renderer:                  renderer,
		JWTSecret:                 cfg.JWTSecret,
		Secure:                    cfg.AppEnv == "production",
		TrustedProxyCount:         cfg.TrustedProxyCount,
	})

	srv := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           handler,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// ── Token cleanup background worker ───────────────────────────────────────
	go app.RunTokenCleanup(cleanupCtx, queries)

	// ── Graceful shutdown ─────────────────────────────────────────────────────
	sigCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		slog.Info("listening", "addr", srv.Addr)
		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("listen", "error", listenErr)
			os.Exit(1)
		}
	}()

	<-sigCtx.Done()
	slog.Info("shutdown signal received")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("graceful shutdown", "error", err)
	}

	// Stop background goroutines (token cleanup, rate-limiter sweepers).
	stopCleanup()

	pool.Close()
	slog.Info("shutdown complete")
}

// ── Config ────────────────────────────────────────────────────────────────────

type config struct {
	AppEnv                string
	Port                  string
	AppBaseURL            string
	DatabaseURL           string
	DBMaxConns            int
	DBMinConns            int
	TrustedProxyCount     int
	JWTSecret             string
	JWTAccessExpiry       time.Duration
	RefreshTokenExpiry    time.Duration
	OAuthTokenEncKey      []byte
	DiscordClientID       string
	DiscordClientSecret   string
	DiscordBotPermissions int
	DiscordPublicKey      string // hex-encoded Ed25519 public key
	DiscordApplicationID  string
	DiscordBotToken       string
	Concept2ClientID      string
	Concept2ClientSecret  string
	Concept2APIBase       string
}

func mustLoadConfig() config {
	cfg := config{
		AppEnv:      getEnvOrDefault("APP_ENV", "development"),
		Port:        getEnvOrDefault("PORT", "8080"),
		AppBaseURL:  mustGetEnv("APP_BASE_URL"),
		DatabaseURL: mustGetEnv("DATABASE_URL"),
		JWTSecret:   mustGetEnv("JWT_SECRET"),
	}

	if len(cfg.JWTSecret) < 32 {
		slog.Error("JWT_SECRET must be at least 32 bytes")
		os.Exit(1)
	}

	var err error
	cfg.DBMaxConns, err = strconv.Atoi(getEnvOrDefault("DB_MAX_CONNS", "25"))
	if err != nil {
		cfg.DBMaxConns = 25
	}

	cfg.DBMinConns, err = strconv.Atoi(getEnvOrDefault("DB_MIN_CONNS", "5"))
	if err != nil {
		cfg.DBMinConns = 5
	}

	cfg.TrustedProxyCount, err = strconv.Atoi(getEnvOrDefault("TRUSTED_PROXY_COUNT", "0"))
	if err != nil {
		cfg.TrustedProxyCount = 0
	}

	cfg.JWTAccessExpiry, err = time.ParseDuration(getEnvOrDefault("JWT_ACCESS_EXPIRY", "15m"))
	if err != nil {
		cfg.JWTAccessExpiry = 15 * time.Minute
	}

	cfg.RefreshTokenExpiry, err = time.ParseDuration(getEnvOrDefault("REFRESH_TOKEN_EXPIRY", "120h"))
	if err != nil {
		cfg.RefreshTokenExpiry = 720 * time.Hour
	}

	{
		raw := os.Getenv("OAUTH_TOKEN_ENC_KEY")
		if raw != "" {
			key, decErr := base64.StdEncoding.DecodeString(raw)
			if decErr != nil || len(key) != 32 {
				slog.Error("OAUTH_TOKEN_ENC_KEY must be base64-encoded 32 bytes")
				os.Exit(1)
			}
			cfg.OAuthTokenEncKey = key
		}
	}

	cfg.DiscordClientID = mustGetEnv("DISCORD_CLIENT_ID")
	cfg.DiscordClientSecret = mustGetEnv("DISCORD_CLIENT_SECRET")
	cfg.DiscordBotPermissions, err = strconv.Atoi(getEnvOrDefault("DISCORD_BOT_PERMISSIONS", "0"))
	if err != nil {
		cfg.DiscordBotPermissions = 0
	}
	cfg.DiscordPublicKey = mustGetEnv("DISCORD_PUBLIC_KEY")
	cfg.DiscordApplicationID = mustGetEnv("DISCORD_APPLICATION_ID")
	cfg.DiscordBotToken = os.Getenv("DISCORD_BOT_TOKEN")

	cfg.Concept2ClientID = mustGetEnv("CONCEPT2_CLIENT_ID")
	cfg.Concept2ClientSecret = mustGetEnv("CONCEPT2_CLIENT_SECRET")
	cfg.Concept2APIBase = getEnvOrDefault("CONCEPT2_API_BASE", "https://log.concept2.com")

	return cfg
}

func mustGetEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error(fmt.Sprintf("required environment variable %s is not set", key))
		os.Exit(1)
	}
	return v
}

func getEnvOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func setupLogger(env string) {
	var handler slog.Handler
	if env == "production" {
		handler = slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo})
	} else {
		handler = slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})
	}
	slog.SetDefault(slog.New(handler))
}

// Compile-time checks.
var _ handlers.DBPinger = (*pgxpool.Pool)(nil)
var _ db.DBTX = (*pgxpool.Pool)(nil)
