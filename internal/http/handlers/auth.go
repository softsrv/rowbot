package handlers

import (
	"context"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"

	"github.com/mssola/useragent"

	"github.com/softsrv/rowbot/internal/app"
	"github.com/softsrv/rowbot/internal/http/middleware"
)

// authServicer defines the subset of app.AuthService that AuthHandler requires.
// Accepting an interface (rather than the concrete type) makes the handler
// independently testable without a real database. Account creation and login
// happen exclusively via Discord OAuth (see OAuthHandler); this handler only
// manages the generic session lifecycle shared by every sign-in path.
type authServicer interface {
	Logout(ctx context.Context, rawRefreshToken string) error
	Refresh(ctx context.Context, rawRefreshToken string, meta app.DeviceMeta) (app.TokenResult, error)
}

// AuthHandler groups all authentication HTTP handlers.
type AuthHandler struct {
	auth              authServicer
	renderer          *TemplateRenderer
	secure            bool // true in production (Secure cookie flag)
	trustedProxyCount int  // number of trusted reverse-proxy hops for client-IP extraction
}

// NewAuthHandler constructs an AuthHandler.
func NewAuthHandler(authSvc authServicer, renderer *TemplateRenderer, secure bool, trustedProxyCount int) *AuthHandler {
	return &AuthHandler{
		auth:              authSvc,
		renderer:          renderer,
		secure:            secure,
		trustedProxyCount: trustedProxyCount,
	}
}

// SilentRefresh exchanges a valid refresh token for a new access token and
// redirects the user to the original destination. Used when the JWT has expired
// but a long-lived refresh token is still available.
func (h *AuthHandler) SilentRefresh(w http.ResponseWriter, r *http.Request) {
	next := r.URL.Query().Get("next")
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		next = "/dashboard"
	}

	candidates := refreshTokenCandidates(r)
	if len(candidates) == 0 {
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	meta := h.deviceMeta(r)
	result, err := h.tryRefresh(r.Context(), candidates, meta)
	if err != nil {
		slog.WarnContext(r.Context(), "silent refresh failed", "error", err)
		clearAuthCookies(w, h.secure)
		http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		return
	}

	h.setTokenCookies(w, result)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

func (h *AuthHandler) Logout(w http.ResponseWriter, r *http.Request) {
	for _, rawToken := range refreshTokenCandidates(r) {
		if logoutErr := h.auth.Logout(r.Context(), rawToken); logoutErr != nil {
			slog.WarnContext(r.Context(), "logout: revoke refresh token", "error", logoutErr)
		}
	}
	clearAuthCookies(w, h.secure)
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

func (h *AuthHandler) Refresh(w http.ResponseWriter, r *http.Request) {
	candidates := refreshTokenCandidates(r)
	if len(candidates) == 0 {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	meta := h.deviceMeta(r)
	result, err := h.tryRefresh(r.Context(), candidates, meta)
	if err != nil {
		slog.WarnContext(r.Context(), "token refresh failed", "error", err)
		clearAuthCookies(w, h.secure)
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	h.setTokenCookies(w, result)
	w.WriteHeader(http.StatusOK)
}

// tryRefresh attempts app.AuthService.Refresh with each candidate raw
// refresh-token value in turn, returning the first success. Normally there's
// only one candidate; see refreshTokenCandidates for why there can be more,
// and ErrTokenNotFound below for why trying each one is safe — a lookup miss
// on one candidate has no side effect that could interfere with the next.
func (h *AuthHandler) tryRefresh(ctx context.Context, candidates []string, meta app.DeviceMeta) (app.TokenResult, error) {
	var result app.TokenResult
	var err error
	for _, rawToken := range candidates {
		result, err = h.auth.Refresh(ctx, rawToken, meta)
		if err == nil {
			return result, nil
		}
	}
	return app.TokenResult{}, err
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func (h *AuthHandler) setTokenCookies(w http.ResponseWriter, result app.TokenResult) {
	setTokenCookies(w, result, h.secure)
}

func (h *AuthHandler) deviceMeta(r *http.Request) app.DeviceMeta {
	return deviceMetaFromRequest(r, h.trustedProxyCount)
}

func deviceMetaFromRequest(r *http.Request, trustedProxyCount int) app.DeviceMeta {
	var addr *netip.Addr

	if parsed, err := netip.ParseAddr(middleware.ClientIP(r, trustedProxyCount)); err == nil {
		addr = &parsed
	}

	ua := useragent.New(r.UserAgent())
	browser, _ := ua.Browser()
	os := ua.OS()
	deviceName := browser
	if os != "" && browser != "" {
		deviceName = browser + " on " + os
	}

	return app.DeviceMeta{
		DeviceName: deviceName,
		IPAddress:  addr,
		UserAgent:  r.UserAgent(),
	}
}
