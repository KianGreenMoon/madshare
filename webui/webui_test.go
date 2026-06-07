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
	RegisterAdminPage(r, "")

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
