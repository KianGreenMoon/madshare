//go:build !nowebui

package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
)

// TestAdminSubPagesRender checks that every reworked admin sub-page executes its
// template (shell partials included) and marks the right secondary-nav link
// active. It guards the Phase-1 scaffold wiring in RegisterAdminPage.
func TestAdminSubPagesRender(t *testing.T) {
	r := chi.NewRouter()
	RegisterAdminPage(r, "", "")

	// The /admin landing (dashboard) shares the shell; verify it renders.
	t.Run("dashboard", func(t *testing.T) {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /admin = %d, want 200", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), "dashboard-grid") {
			t.Errorf("/admin: missing dashboard grid")
		}
	})

	for sub := range adminSubPages {
		t.Run(sub, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/"+sub, nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("GET /admin/%s = %d, want 200", sub, rec.Code)
			}
			// Collapse runs of whitespace so the readable column-aligned
			// markup in the partial doesn't make exact-substring checks brittle.
			body := strings.Join(strings.Fields(rec.Body.String()), " ")

			// Shared shell rendered.
			if !strings.Contains(body, "admin-banner") {
				t.Errorf("/admin/%s: missing admin banner", sub)
			}
			// Active state on this section's nav link.
			want := `href="/admin/` + sub + `" class="admin-nav-link admin-nav-link--active"`
			if !strings.Contains(body, want) {
				t.Errorf("/admin/%s: expected active nav link\nwant substring: %s", sub, want)
			}
		})
	}
}

// TestSettingsPageRenders checks the user settings page executes its template
// and carries its three sections plus the no-FOUC head guard.
func TestSettingsPageRenders(t *testing.T) {
	r := chi.NewRouter()
	Register(r, "", "")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`data-module="/static/js/settings.js"`,
		`id="passSettingsForm"`, // Account
		`id="tokenForm"`,        // API tokens
		`id="tokenExpiry"`,      // optional expiry date
		`id="themeChoices"`,     // Appearance
		"madshare-theme",        // theme-guard inline script
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/settings: missing %q", want)
		}
	}
}

// TestHeaderUserArea guards the header rework: the Settings link replaced the
// voluntary change-password button and the theme-switcher dots were removed.
func TestHeaderUserArea(t *testing.T) {
	r := chi.NewRouter()
	Register(r, "", "")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `id="settingsLink"`) {
		t.Errorf("header: missing Settings link")
	}
	if strings.Contains(body, `id="changePassBtn"`) {
		t.Errorf("header: voluntary change-password button should be gone")
	}
	if strings.Contains(body, "theme-switcher") {
		t.Errorf("header: theme switcher dots should be gone")
	}
}

// TestAboutMenu checks the header About mini-menu: the GitRepo entry renders
// with the configured URL and is absent when the URL is empty, while the Source
// and License entries always render inside the menu.
func TestAboutMenu(t *testing.T) {
	render := func(gitRepo string) string {
		r := chi.NewRouter()
		Register(r, "", gitRepo)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET / = %d, want 200", rec.Code)
		}
		return rec.Body.String()
	}

	withRepo := render("https://git.example.org/me/madshare")
	if !strings.Contains(withRepo, `href="https://git.example.org/me/madshare"`) ||
		!strings.Contains(withRepo, ">GitRepo</a>") {
		t.Errorf("expected the GitRepo entry in the About menu")
	}
	// Source and License moved into the About menu (Source was previously hidden).
	if !strings.Contains(withRepo, `href="/source"`) {
		t.Errorf("expected the Source entry in the About menu")
	}
	if !strings.Contains(withRepo, `href="/license"`) {
		t.Errorf("expected the License entry in the About menu")
	}
	// The Version entry opens the About modal.
	if !strings.Contains(withRepo, `id="aboutVersion"`) || !strings.Contains(withRepo, `id="aboutModal"`) {
		t.Errorf("expected the Version entry and the About modal in the header")
	}

	hidden := render("")
	if strings.Contains(hidden, ">GitRepo</a>") {
		t.Errorf("GitRepo entry should be hidden for an empty URL")
	}
}
