package http

import (
	"context"
	"io/fs"
	"net/http"
	"time"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/auth"
	"github.com/softsrv/rowbot/internal/db"
	"github.com/softsrv/rowbot/internal/http/handlers"
	"github.com/softsrv/rowbot/internal/http/middleware"
	"github.com/softsrv/rowbot/web"
)

// RouterConfig holds all dependencies required to build the router.
type RouterConfig struct {
	Queries                   *db.Queries
	Pool                      handlers.DBPinger
	AuthSvc                   *app.AuthService
	UserSvc                   *app.UserService
	OAuthSvc                  *app.OAuthService
	DiscordAuthorizeURL       func(state string) string
	DiscordSilentAuthorizeURL func(state string) string
	DiscordLinkAuthorizeURL   func(state string) string
	DiscordBotInstallURL      string
	DiscordInteractions       *handlers.DiscordHandler
	DiscordSvc                *app.DiscordService
	DiscordBotToken           string
	Concept2AuthorizeURL      func(state string) string
	RowingSvc                 *app.RowingService
	Renderer                  *handlers.TemplateRenderer
	JWTSecret                 string
	Secure                    bool // true in production
	TrustedProxyCount         int
}

// NewRouter builds and returns the main http.Handler with all routes and middleware.
// ctx controls the lifetime of background goroutines (rate-limiter sweepers); it
// should be cancelled during application shutdown after the HTTP server drains.
func NewRouter(ctx context.Context, cfg RouterConfig) http.Handler {
	mux := http.NewServeMux()

	// ── Handlers ──────────────────────────────────────────────────────────────
	authH := handlers.NewAuthHandler(cfg.AuthSvc, cfg.Renderer, cfg.Secure, cfg.TrustedProxyCount)
	sessH := handlers.NewSessionHandler(cfg.UserSvc, cfg.Renderer, cfg.Secure)

	var profileOAuth handlers.ProfileOAuthServicer
	if cfg.OAuthSvc != nil {
		profileOAuth = cfg.OAuthSvc
	}
	profileH := handlers.NewProfileHandler(cfg.UserSvc, profileOAuth, cfg.DiscordSvc, cfg.DiscordBotInstallURL, cfg.Renderer, cfg.Secure)

	// ── Rate limiters ─────────────────────────────────────────────────────────
	// Each limiter spawns a sweep goroutine that exits when ctx is cancelled.
	ipKey := middleware.IPKeyFunc(cfg.TrustedProxyCount)
	refreshRL := middleware.NewRateLimiter(ctx, 10, time.Minute, middleware.CookieRefreshTokenKeyFunc)

	authMW := middleware.Authenticate(cfg.Queries, cfg.JWTSecret, cfg.Secure)

	if cfg.OAuthSvc != nil {
		oauthH := handlers.NewOAuthHandler(cfg.OAuthSvc, cfg.DiscordAuthorizeURL, cfg.DiscordSilentAuthorizeURL, cfg.DiscordLinkAuthorizeURL, cfg.Concept2AuthorizeURL, cfg.DiscordSvc, cfg.DiscordBotToken, nil, cfg.UserSvc, cfg.Secure, cfg.TrustedProxyCount)
		discordRL := middleware.NewRateLimiter(ctx, 10, 15*time.Minute, ipKey)
		mux.Handle("GET /auth/discord/login", discordRL.Middleware(http.HandlerFunc(oauthH.DiscordLogin)))
		mux.Handle("GET /auth/discord/callback", discordRL.Middleware(http.HandlerFunc(oauthH.DiscordCallback)))
		mux.Handle("GET /auth/discord/link", authMW(http.HandlerFunc(oauthH.DiscordLinkStart)))
		mux.Handle("GET /auth/discord/link/callback", authMW(http.HandlerFunc(oauthH.DiscordLinkCallback)))
		mux.Handle("GET /auth/discord/bot-install/callback", authMW(http.HandlerFunc(oauthH.DiscordBotInstallCallback)))

		mux.Handle("GET /auth/concept2/link", authMW(http.HandlerFunc(oauthH.Concept2LinkStart)))
		mux.Handle("GET /auth/concept2/link/callback", authMW(http.HandlerFunc(oauthH.Concept2LinkCallback)))
		mux.Handle("POST /profile/connections/concept2/unlink", authMW(http.HandlerFunc(oauthH.Concept2Unlink)))
		mux.Handle("POST /profile/discord-registration", authMW(http.HandlerFunc(profileH.RegisterDiscordServer)))
	}

	// ── Static assets ─────────────────────────────────────────────────────────
	staticFS, _ := fs.Sub(web.FS, "static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(staticFS)))

	// robots.txt is served at the root path (not under /static/) since that's
	// the only place crawlers look for it. Read once at startup — it's an
	// embedded asset, not something that changes at runtime.
	robotsTxt, err := web.FS.ReadFile("static/robots.txt")
	if err != nil {
		panic("read embedded robots.txt: " + err.Error())
	}
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Write(robotsTxt)
	})

	// ── Discord interactions (public — protected by Ed25519 signature) ────────
	if cfg.DiscordInteractions != nil {
		mux.HandleFunc("POST /discord/interactions", cfg.DiscordInteractions.Interactions)
	}

	// ── Concept2 webhook (public — trust is structural, not cryptographic; ────
	// Concept2 does not sign deliveries, so the handler only validates payload
	// shape and re-fetches real data via our own OAuth token; see webhooks.go)
	webhookH := handlers.NewWebhookHandler(cfg.RowingSvc)
	mux.Handle("POST /webhooks/concept2", http.HandlerFunc(webhookH.Concept2))

	// ── Public routes ─────────────────────────────────────────────────────────
	mux.HandleFunc("GET /health", handlers.HandleLiveness)
	mux.HandleFunc("GET /ready", handlers.HandleReadiness(cfg.Pool))

	mux.HandleFunc("GET /terms", func(w http.ResponseWriter, r *http.Request) {
		cfg.Renderer.Page(w, http.StatusOK, "terms.html", nil)
	})
	mux.HandleFunc("GET /privacy", func(w http.ResponseWriter, r *http.Request) {
		cfg.Renderer.Page(w, http.StatusOK, "privacy.html", nil)
	})

	// {$} anchors this to the exact path "/" — without it, a pattern ending in
	// "/" is a subtree match in net/http's ServeMux and catches every
	// otherwise-unmatched path (e.g. bot-scan noise like /wp-admin/install.php),
	// running the full landing-page logic below for each one instead of a
	// plain 404.
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		// Send an already-authenticated visitor straight to the dashboard;
		// only send them there for a genuinely valid access token — a stale
		// or malformed cookie should land on the landing page rather than
		// bouncing through a protected route. Anonymous visitors see the
		// marketing landing page instead of being redirected anywhere.
		if cookie, err := r.Cookie("access_token"); err == nil && cookie.Value != "" {
			if _, vErr := auth.ValidateAccessToken(cookie.Value, cfg.JWTSecret); vErr == nil {
				http.Redirect(w, r, "/dashboard", http.StatusFound)
				return
			}
		}
		// No valid access token — before giving up and showing the marketing
		// page, mirror middleware.Authenticate's recovery path: a still-valid
		// refresh_token means this is an expired session, not a genuinely
		// anonymous visitor, so attempt a silent refresh first.
		if _, err := r.Cookie("refresh_token"); err == nil {
			http.Redirect(w, r, "/auth/silent-refresh?next=/dashboard", http.StatusFound)
			return
		}
		cfg.Renderer.Page(w, http.StatusOK, "landing.html", nil)
	})

	mux.Handle("GET /auth/silent-refresh", refreshRL.Middleware(http.HandlerFunc(authH.SilentRefresh)))
	mux.Handle("POST /auth/refresh", refreshRL.Middleware(http.HandlerFunc(authH.Refresh)))

	// ── Protected routes ──────────────────────────────────────────────────────
	mux.Handle("POST /auth/logout", authMW(http.HandlerFunc(authH.Logout)))

	mux.Handle("GET /auth/sessions", authMW(http.HandlerFunc(sessH.ListSessions)))
	mux.Handle("DELETE /auth/sessions/{id}", authMW(http.HandlerFunc(sessH.RevokeSession)))

	mux.Handle("GET /profile", authMW(http.HandlerFunc(profileH.ProfilePage)))
	mux.Handle("POST /profile/delete", authMW(http.HandlerFunc(profileH.DeleteAccount)))

	// Dashboard now requires a session — anonymous visitors are sent to the
	// landing page ("/") instead, which is the real sign-in entry point via
	// its Discord OAuth "Get Started" link. Same authMW as /profile.
	mux.Handle("GET /dashboard", authMW(http.HandlerFunc(profileH.DashboardPage)))
	mux.Handle("POST /dashboard/setup/next", authMW(http.HandlerFunc(profileH.SetupNext)))
	mux.Handle("POST /dashboard/setup/previous", authMW(http.HandlerFunc(profileH.SetupPrevious)))
	mux.Handle("POST /dashboard/setup/skip", authMW(http.HandlerFunc(profileH.SetupSkipToRegister)))
	mux.Handle("GET /dashboard/servers/{guildID}", authMW(http.HandlerFunc(profileH.GuildPage)))
	mux.Handle("POST /dashboard/servers/{guildID}/channel", authMW(http.HandlerFunc(profileH.SetGuildChannel)))
	mux.Handle("DELETE /dashboard/servers/{guildID}/channel", authMW(http.HandlerFunc(profileH.RemoveGuildChannel)))
	mux.Handle("POST /dashboard/servers/{guildID}/unregister", authMW(http.HandlerFunc(profileH.UnregisterDiscordServer)))

	// ── Global middleware chain ───────────────────────────────────────────────
	// BodyLimit is innermost so it wraps r.Body before any handler reads it.
	return middleware.RequestID(
		middleware.Logging(
			middleware.SecurityHeaders(cfg.Secure,
				middleware.BodyLimit(middleware.DefaultMaxBodyBytes)(mux),
			),
		),
	)
}
