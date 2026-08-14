package http_test

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/google/uuid"

	"github.com/softsrv/rowbot/internal/auth"
	httpapp "github.com/softsrv/rowbot/internal/http"
	"github.com/softsrv/rowbot/internal/http/handlers"
)

const routerTestJWTSecret = "router-test-secret-that-is-at-least-32-bytes!!"

var routerTestTemplateFS = fstest.MapFS{
	"templates/base.html": &fstest.MapFile{Data: []byte(
		`{{define "base.html"}}<!doctype html><html><body>{{block "content" .}}{{end}}</body></html>{{end}}`,
	)},
	"templates/landing.html": &fstest.MapFile{Data: []byte(
		`{{define "content"}}landing{{end}}`,
	)},
	"templates/terms.html": &fstest.MapFile{Data: []byte(
		`{{define "content"}}terms{{end}}`,
	)},
	"templates/privacy.html": &fstest.MapFile{Data: []byte(
		`{{define "content"}}privacy{{end}}`,
	)},
}

func newRouterTestRenderer(t *testing.T) *handlers.TemplateRenderer {
	t.Helper()
	base, err := template.ParseFS(routerTestTemplateFS, "templates/base.html")
	if err != nil {
		t.Fatalf("parse base template: %v", err)
	}
	return handlers.NewTemplateRenderer(base, routerTestTemplateFS, "templates")
}

func newRouterTestToken(t *testing.T, expiry time.Duration) string {
	t.Helper()
	tp, err := auth.IssueAccessToken(uuid.New(), "test@example.com", routerTestJWTSecret, expiry)
	if err != nil {
		t.Fatalf("IssueAccessToken: %v", err)
	}
	return tp.AccessToken
}

// TestLandingPage_MissingTokenWithRefreshCookieAttemptsSilentRefresh guards
// against the landing page ("/") stranding a visitor with an expired access
// token: unlike every route behind authMW, "/" does its own inline check for
// a valid access_token cookie rather than going through
// middleware.Authenticate, so it must independently mirror that middleware's
// refresh-token recovery path or a session that expired while sitting on "/"
// never gets a chance to recover.
func TestLandingPage_MissingTokenWithRefreshCookieAttemptsSilentRefresh(t *testing.T) {
	t.Parallel()
	router := httpapp.NewRouter(context.Background(), httpapp.RouterConfig{
		Renderer:  newRouterTestRenderer(t),
		JWTSecret: routerTestJWTSecret,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "refresh_token", Value: "rt-123"})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("got %d, want 302", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/auth/silent-refresh?next=/dashboard" {
		t.Errorf("redirect = %q, want silent-refresh with next=/dashboard", got)
	}
}

func TestLandingPage_NoCookiesShowsLandingPage(t *testing.T) {
	t.Parallel()
	router := httpapp.NewRouter(context.Background(), httpapp.RouterConfig{
		Renderer:  newRouterTestRenderer(t),
		JWTSecret: routerTestJWTSecret,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
}

func TestLandingPage_ValidAccessTokenRedirectsToDashboard(t *testing.T) {
	t.Parallel()
	router := httpapp.NewRouter(context.Background(), httpapp.RouterConfig{
		Renderer:  newRouterTestRenderer(t),
		JWTSecret: routerTestJWTSecret,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: "access_token", Value: newRouterTestToken(t, 15*time.Minute)})
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Errorf("got %d, want 302", rr.Code)
	}
	if got := rr.Header().Get("Location"); got != "/dashboard" {
		t.Errorf("redirect = %q, want /dashboard", got)
	}
}

// TestUnmatchedPathReturns404 guards against "GET /" silently reverting to a
// net/http ServeMux subtree match (matching every unmatched path, not just
// "/") — without the "{$}" exact-path anchor, bot-scan noise like
// /wp-admin/install.php would run the full landing-page handler (including
// its cookie/JWT checks) instead of getting a plain 404.
func TestUnmatchedPathReturns404(t *testing.T) {
	t.Parallel()
	router := httpapp.NewRouter(context.Background(), httpapp.RouterConfig{
		Renderer:  newRouterTestRenderer(t),
		JWTSecret: routerTestJWTSecret,
	})

	req := httptest.NewRequest(http.MethodGet, "/wp-admin/install.php", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rr.Code)
	}
}

// TestRobotsTxt guards it being served at the root path (not just under
// /static/, which crawlers don't look at) with the correct content type.
func TestRobotsTxt(t *testing.T) {
	t.Parallel()
	router := httpapp.NewRouter(context.Background(), httpapp.RouterConfig{
		Renderer:  newRouterTestRenderer(t),
		JWTSecret: routerTestJWTSecret,
	})

	req := httptest.NewRequest(http.MethodGet, "/robots.txt", nil)
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("got %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/plain; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/plain; charset=utf-8", ct)
	}
	if !strings.Contains(rr.Body.String(), "User-agent: *") {
		t.Errorf("body missing expected content, got: %s", rr.Body.String())
	}
}

func TestTermsAndPrivacyPagesArePublic(t *testing.T) {
	t.Parallel()
	router := httpapp.NewRouter(context.Background(), httpapp.RouterConfig{
		Renderer:  newRouterTestRenderer(t),
		JWTSecret: routerTestJWTSecret,
	})

	for _, path := range []string{"/terms", "/privacy"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		router.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("%s: got %d, want 200", path, rr.Code)
		}
	}
}
