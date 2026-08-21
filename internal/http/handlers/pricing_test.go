package handlers_test

import (
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/softsrv/rowbot/internal/http/handlers"
	"github.com/softsrv/rowbot/web"
)

func newPricingRealTemplateRenderer(t *testing.T) *handlers.TemplateRenderer {
	t.Helper()
	base, err := template.ParseFS(web.FS, "templates/base.html")
	if err != nil {
		t.Fatalf("parse base template: %v", err)
	}
	if _, err := base.ParseFS(web.FS, "templates/partials/*.html"); err != nil {
		t.Fatalf("parse partials: %v", err)
	}
	return handlers.NewTemplateRenderer(base, web.FS, "templates")
}

func TestPricingPageRendersPublicPricingContent(t *testing.T) {
	renderer := newPricingRealTemplateRenderer(t)
	rr := httptest.NewRecorder()

	renderer.Page(rr, http.StatusOK, "pricing.html", nil)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"Free",
		"Cozy Community",
		"Large Community",
		"up to 10",
		"up to 50",
		"up to 250",
		"$5",
		"$10",
		"js-get-started",
		`id="get-started-modal"`,
		`href="/pricing"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered pricing page missing %q in:\n%s", want, body)
		}
	}
}
