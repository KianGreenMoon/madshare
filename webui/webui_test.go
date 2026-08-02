//go:build !nowebui

package webui

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"daemonlord.ygg/madshare/auth"
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
	Register(r, "", "", false)
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

// TestHeaderUserArea guards the header rework: the username itself links to the
// Settings page (the separate Settings button is gone), the voluntary
// change-password button is gone, and the theme-switcher dots were removed.
func TestHeaderUserArea(t *testing.T) {
	r := chi.NewRouter()
	Register(r, "", "", false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/library", nil))
	body := rec.Body.String()
	if !strings.Contains(body, `id="userName"`) || !strings.Contains(body, `href="/settings"`) {
		t.Errorf("header: username should link to /settings")
	}
	if strings.Contains(body, `id="settingsLink"`) {
		t.Errorf("header: separate Settings button should be gone (the username is the link now)")
	}
	if strings.Contains(body, `id="changePassBtn"`) {
		t.Errorf("header: voluntary change-password button should be gone")
	}
	if strings.Contains(body, "theme-switcher") {
		t.Errorf("header: theme switcher dots should be gone")
	}
}

// TestHeaderAuthState guards the server-side auth rendering that kills the header
// FOUC: an anonymous load omits the privileged nav links and shows Sign in with
// the user area hidden; a signed-in load paints the username + user area and the
// permitted links straight away (no client round-trip). See webui.makeHandler.
func TestHeaderAuthState(t *testing.T) {
	r := chi.NewRouter()
	Register(r, "", "", false)

	// Anonymous (no identity in context).
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/library", nil))
	anon := rec.Body.String()
	if strings.Contains(anon, `href="/upload"`) || strings.Contains(anon, `href="/admin"`) {
		t.Errorf("anonymous header should omit the Upload/Admin nav links")
	}
	if strings.Contains(anon, `href="/playlists"`) {
		t.Errorf("anonymous header should omit the Playlists subtab")
	}
	if !strings.Contains(anon, `id="signInBtn">`) {
		t.Errorf("anonymous header should show Sign in (not hidden)")
	}
	if !strings.Contains(anon, `id="userArea" hidden`) {
		t.Errorf("anonymous header should hide the user area")
	}

	// Signed-in admin: privileged links present, user area shows the username.
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/library", nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{
		Username: "alice",
		Permissions: map[string]bool{
			auth.PermFileUpload:    true,
			auth.PermFileDelete:    true,
			auth.PermContentAccess: true,
		},
	}))
	r.ServeHTTP(rec, req)
	in := rec.Body.String()
	for _, want := range []string{`href="/upload"`, `href="/admin"`, `href="/playlists"`, `>alice</a>`} {
		if !strings.Contains(in, want) {
			t.Errorf("signed-in header missing %q", want)
		}
	}
	if strings.Contains(in, `id="userArea" hidden`) {
		t.Errorf("signed-in header should show the user area (not hidden)")
	}
	if !strings.Contains(in, `id="signInBtn" hidden`) {
		t.Errorf("signed-in header should hide the Sign in button")
	}
}

// TestAboutMenu checks the header About mini-menu: the GitRepo entry renders
// with the configured URL and is absent when the URL is empty, while the Source
// and License entries always render inside the menu.
func TestAboutMenu(t *testing.T) {
	render := func(gitRepo string) string {
		r := chi.NewRouter()
		Register(r, "", gitRepo, false)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/library", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /library = %d, want 200", rec.Code)
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

// TestHomeIsAFrontDoor pins the "/" behaviour: it is not a page but a forward to
// whichever section this node opens on. A node with federation off sends every
// caller to its library; a federating node sends the principals who may browse
// the network there, and everyone else — anonymous visitors and accounts without
// madnetwork.access — to the library, so the entry URL is never a dead end.
func TestHomeIsAFrontDoor(t *testing.T) {
	listener := &auth.Identity{Username: "listener", Permissions: map[string]bool{
		auth.PermContentAccess: true,
	}}
	browser := &auth.Identity{Username: "browser", Permissions: map[string]bool{
		auth.PermContentAccess:    true,
		auth.PermMadnetworkAccess: true,
	}}

	for _, tc := range []struct {
		name      string
		federated bool
		id        *auth.Identity
		want      string
	}{
		{"federation off, anonymous", false, nil, "/library"},
		{"federation off, may browse the network", false, browser, "/library"},
		{"federation on, anonymous", true, nil, "/library"},
		{"federation on, no madnetwork.access", true, listener, "/library"},
		{"federation on, may browse the network", true, browser, "/madnetwork"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := chi.NewRouter()
			Register(r, "", "", tc.federated)
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.id != nil {
				req = req.WithContext(auth.WithIdentity(req.Context(), tc.id))
			}
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("GET / = %d, want %d", rec.Code, http.StatusFound)
			}
			if got := rec.Header().Get("Location"); got != tc.want {
				t.Errorf("GET / → %q, want %q", got, tc.want)
			}
			// The target depends on the caller, so it must not be cached.
			if got := rec.Header().Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", got)
			}
		})
	}
}

// TestLibraryPageMoved checks the library browse now answers at its own URL and
// that the header/subtab links point there rather than at the front door.
func TestLibraryPageMoved(t *testing.T) {
	r := chi.NewRouter()
	Register(r, "", "", false)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, LibraryPath, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", LibraryPath, rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `data-page="library"`) || !strings.Contains(body, `data-module="/static/js/app.js"`) {
		t.Errorf("%s should render the library page", LibraryPath)
	}
	// Header tab and Music subtab both address the page, not the redirect.
	if n := strings.Count(body, `href="/library"`); n < 2 {
		t.Errorf("expected the header tab and the Music subtab to link to %s, found %d link(s)", LibraryPath, n)
	}
	// The wordmark is the ONLY thing that still points at the front door: it
	// stands for "wherever this server starts", which is exactly what "/" means.
	// Any other link there would be a needless redirect hop.
	if n := strings.Count(body, `href="/"`); n != 1 {
		t.Errorf("expected exactly one link to the front door (the logo), found %d", n)
	}
	if !strings.Contains(body, `<a href="/" class="logo"`) {
		t.Errorf("the Madshare wordmark should be the link to the front door")
	}
}

// TestMadnetworkTabIsPinned guards the header layout decision: the Madnetwork
// tab sits between Library and About and OUTSIDE .nav-collapse, so on a narrow
// screen it stays inline instead of folding into the ☰ overflow menu. Position
// is asserted structurally (offsets in the rendered document) because that is
// the property the CSS depends on — .nav-collapse is what the media query turns
// into the dropdown panel.
func TestMadnetworkTabIsPinned(t *testing.T) {
	r := chi.NewRouter()
	Register(r, "", "", true)
	req := httptest.NewRequest(http.MethodGet, LibraryPath, nil)
	req = req.WithContext(auth.WithIdentity(req.Context(), &auth.Identity{
		Username: "browser",
		Permissions: map[string]bool{
			auth.PermContentAccess:    true,
			auth.PermMadnetworkAccess: true,
			auth.PermFileUpload:       true,
		},
	}))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	body := rec.Body.String()

	library := strings.Index(body, `href="/library"`)
	madnet := strings.Index(body, `href="/madnetwork"`)
	about := strings.Index(body, `class="about" id="about"`)
	collapse := strings.Index(body, `class="nav-collapse"`)
	upload := strings.Index(body, `href="/upload"`)
	for name, idx := range map[string]int{
		"library": library, "madnetwork": madnet, "about": about,
		"nav-collapse": collapse, "upload": upload,
	} {
		if idx < 0 {
			t.Fatalf("header is missing %s", name)
		}
	}
	if !(library < madnet && madnet < about) {
		t.Errorf("Madnetwork should sit between Library and About (offsets %d, %d, %d)", library, madnet, about)
	}
	if madnet > collapse {
		t.Errorf("Madnetwork should be pinned outside .nav-collapse, so the ☰ menu never swallows it")
	}
	// Upload is the control: it stays inside the collapsible group.
	if upload < collapse {
		t.Errorf("Upload should still live inside .nav-collapse")
	}
	if !strings.Contains(body, `class="nav-link nav-link--pinned`) {
		t.Errorf("pinned tabs should carry .nav-link--pinned (the flex-shrink:0 rule)")
	}
}
