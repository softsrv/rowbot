package handlers_test

import (
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

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

// verifyFullPageCopyrightFooter checks that the rendered HTML satisfies the footer
// contract: <footer> appears after </main>, contains the copyright text, the <body>
// has flex/min-h-screen/flex-col classes, and <main> has flex-1.
func verifyFullPageCopyrightFooter(html string) error {
	mainCloseIdx := strings.Index(html, "</main>")
	if mainCloseIdx == -1 {
		return errors.New("missing </main> tag")
	}

	footerIdx := strings.Index(html, "<footer")
	if footerIdx == -1 {
		return errors.New("missing <footer tag")
	}

	if footerIdx < mainCloseIdx {
		return fmt.Errorf("<footer (idx %d) appears before </main> (idx %d)", footerIdx, mainCloseIdx)
	}

	footerEndIdx := strings.Index(html[footerIdx:], "</footer>")
	if footerEndIdx == -1 {
		return errors.New("missing </footer> tag")
	}
	footerContent := html[footerIdx : footerIdx+footerEndIdx+len("</footer>")]

	const wantCopyright = "&copy; 2026 softsrv"
	if !strings.Contains(footerContent, wantCopyright) {
		return fmt.Errorf("footer does not contain %q", wantCopyright)
	}

	bodyTagStart := strings.Index(html, "<body")
	if bodyTagStart == -1 {
		return errors.New("missing <body tag")
	}
	bodyTagEnd := strings.Index(html[bodyTagStart:], ">")
	if bodyTagEnd == -1 {
		return errors.New("malformed <body tag")
	}
	bodyTag := html[bodyTagStart : bodyTagStart+bodyTagEnd+1]
	for _, reqClass := range []string{"flex", "min-h-screen", "flex-col"} {
		if !strings.Contains(bodyTag, reqClass) {
			return fmt.Errorf("<body> tag missing required class %q: %s", reqClass, bodyTag)
		}
	}

	mainTagStart := strings.Index(html, "<main")
	if mainTagStart == -1 {
		return errors.New("missing <main tag")
	}
	mainTagEnd := strings.Index(html[mainTagStart:], ">")
	if mainTagEnd == -1 {
		return errors.New("malformed <main tag")
	}
	mainTag := html[mainTagStart : mainTagStart+mainTagEnd+1]
	if !strings.Contains(mainTag, "flex-1") {
		return fmt.Errorf("<main> tag missing required class \"flex-1\": %s", mainTag)
	}

	return nil
}

func TestEveryFullPageShowsCopyrightFooter(t *testing.T) {
	// Verify that verifyFullPageCopyrightFooter fails on a fixture without a footer.
	t.Run("negative fixture fails", func(t *testing.T) {
		fixtureWithoutFooter := `<!DOCTYPE html>
<html lang="en">
<body class="flex min-h-screen flex-col">
  <main class="flex flex-1 flex-col">
    <div>Content</div>
  </main>
</body>
</html>`
		if err := verifyFullPageCopyrightFooter(fixtureWithoutFooter); err == nil {
			t.Fatal("expected verifyFullPageCopyrightFooter to fail on fixture without footer, but got nil")
		}
	})

	renderer := newPricingRealTemplateRenderer(t)

	pages := []struct {
		name     string
		template string
		data     any
	}{
		{
			name:     "landing",
			template: "landing.html",
			data:     nil,
		},
		{
			name:     "terms",
			template: "terms.html",
			data:     nil,
		},
		{
			name:     "privacy",
			template: "privacy.html",
			data:     nil,
		},
		{
			name:     "pricing",
			template: "pricing.html",
			data:     nil,
		},
		{
			name:     "dashboard",
			template: "dashboard.html",
			data: map[string]any{
				"ShowWizard": false,
			},
		},
		{
			name:     "dashboard-server",
			template: "dashboard-server.html",
			data: map[string]any{
				"GuildName":  "Test Club",
				"UserGuilds": []any{},
			},
		},
		{
			name:     "profile",
			template: "profile.html",
			data:     map[string]any{},
		},
	}

	for _, tc := range pages {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			renderer.Page(rr, http.StatusOK, tc.template, tc.data)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			body := rr.Body.String()
			if err := verifyFullPageCopyrightFooter(body); err != nil {
				t.Fatalf("template %q failed copyright footer verification: %v\nbody:\n%s", tc.template, err, body)
			}
		})
	}
}

// verifyOnlyBaseLayoutDefinesDocumentShell walks an fs.FS starting at templatesDir and checks
// that only base.html contains document shell markers ("<!doctype", "<html", "<footer").
func verifyOnlyBaseLayoutDefinesDocumentShell(fsys fs.FS, templatesDir string) error {
	var violations []string

	err := fs.WalkDir(fsys, templatesDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !strings.HasSuffix(path, ".html") {
			return nil
		}

		// base.html is the designated shared base layout allowed to define the document shell and footer.
		if strings.HasSuffix(path, "/base.html") || path == "base.html" {
			return nil
		}

		content, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}

		lower := strings.ToLower(string(content))
		for _, marker := range []string{"<!doctype", "<html", "<footer"} {
			if strings.Contains(lower, marker) {
				violations = append(violations, fmt.Sprintf("%s defines forbidden marker %q", path, marker))
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(violations) > 0 {
		return fmt.Errorf("document shell violations found:\n%s", strings.Join(violations, "\n"))
	}
	return nil
}

func TestOnlyBaseLayoutDefinesDocumentShell(t *testing.T) {
	// Verify that the checker flags a violation against an fs containing a page with its own shell.
	t.Run("negative fixture fails", func(t *testing.T) {
		badFS := fstest.MapFS{
			"templates/base.html": &fstest.MapFile{
				Data: []byte(`<!DOCTYPE html><html><body>{{block "content" .}}{{end}}<footer>&copy; 2026 softsrv</footer></body></html>`),
			},
			"templates/bad-page.html": &fstest.MapFile{
				Data: []byte(`<!DOCTYPE html><html><body><h1>Forbidden Page Shell</h1></body></html>`),
			},
		}
		if err := verifyOnlyBaseLayoutDefinesDocumentShell(badFS, "templates"); err == nil {
			t.Fatal("expected verifyOnlyBaseLayoutDefinesDocumentShell to report a violation for bad-page.html, got nil")
		}
	})

	t.Run("real embedded templates", func(t *testing.T) {
		if err := verifyOnlyBaseLayoutDefinesDocumentShell(web.FS, "templates"); err != nil {
			t.Fatalf("real templates failed shell check: %v", err)
		}
	})
}

// verifyNoFooter checks that neither "<footer" nor "2026 softsrv" appears in the rendered output.
func verifyNoFooter(html string) error {
	lower := strings.ToLower(html)
	if strings.Contains(lower, "<footer") {
		return fmt.Errorf("rendered output contains \"<footer\": %s", html)
	}
	if strings.Contains(html, "2026 softsrv") {
		return fmt.Errorf("rendered output contains \"2026 softsrv\": %s", html)
	}
	return nil
}

func TestPartialsHaveNoFooter(t *testing.T) {
	// Verify that the absence checker flags a fixture containing the footer string.
	t.Run("negative fixture fails", func(t *testing.T) {
		fixtureWithFooter := `<div class="card">Some content</div><footer>&copy; 2026 softsrv</footer>`
		if err := verifyNoFooter(fixtureWithFooter); err == nil {
			t.Fatal("expected verifyNoFooter to fail on fixture with footer, got nil")
		}
		fixtureWithTextOnly := `<div class="card">Some content 2026 softsrv</div>`
		if err := verifyNoFooter(fixtureWithTextOnly); err == nil {
			t.Fatal("expected verifyNoFooter to fail on fixture with copyright text, got nil")
		}
	})

	renderer := newPricingRealTemplateRenderer(t)

	partials := []struct {
		name string
		data any
	}{
		{
			name: "partials/session-list.html",
			data: map[string]any{"Sessions": nil},
		},
		{
			name: "channel-region",
			data: map[string]any{
				"ChannelConfigured": false,
				"IsAdmin":           false,
			},
		},
		{
			name: "partials/error.html",
			data: map[string]any{"Error": "Something went wrong"},
		},
		{
			name: "partials/flash.html",
			data: map[string]any{"Message": "Operation succeeded"},
		},
	}

	for _, tc := range partials {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			renderer.Partial(rr, http.StatusOK, tc.name, tc.data)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rr.Code)
			}
			body := rr.Body.String()
			if err := verifyNoFooter(body); err != nil {
				t.Fatalf("partial %q failed no-footer check: %v", tc.name, err)
			}
		})
	}
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
		"Enthusiast",
		"Power Ten",
		"Unlimited rowers",
		"Up to 5 reporting channels",
		"Up to 10 reporting channels",
		"Per-channel leaderboards",
		"Per-server leaderboards",
		"Customizable monthly events",
		"$5",
		"$10",
		"Make your report card yours",
		"Customizable cards",
		"Stickers",
		"Themes",
		"js-get-started",
		`id="get-started-modal"`,
		`href="/pricing"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered pricing page missing %q in:\n%s", want, body)
		}
	}
}
