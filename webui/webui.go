//go:build !nowebui

package webui

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"

	"daemonlord.ygg/madshare/auth"
	"daemonlord.ygg/madshare/internal/version"
	"github.com/go-chi/chi/v5"
)

// Available reports whether the web UI is compiled into this binary. It is
// false in builds made with -tags nowebui (see webui_stub.go), which lets
// config validation reject listeners that ask to serve the UI.
const Available = true

// The templates and static assets are embedded at build time so the binary
// carries the UI itself — no CWD-relative file access at serve time. These
// embeds exist only in the !nowebui build, so a -tags nowebui binary ships
// without them.
//
//go:embed html/*.html html/admin/*.html
var htmlFS embed.FS

//go:embed static
var staticFS embed.FS

// staticRoot is the embedded static tree rooted at the "static" directory, so
// request paths (e.g. "js/app.js") map directly without a leading "static/".
var staticRoot = mustSub(staticFS, "static")

func mustSub(f embed.FS, dir string) fs.FS {
	sub, err := fs.Sub(f, dir)
	if err != nil {
		panic(err) // the embedded tree is fixed at build time
	}
	return sub
}

// partials is the base template set parsed from partials.html. Each page
// template is built by cloning it so that page-specific {{define}} blocks
// (e.g. "header-insert" for the library search bar) override the defaults
// without leaking between pages.
var partials = template.Must(template.ParseFS(htmlFS, "html/partials.html"))

func buildPageTmpl(file string) *template.Template {
	t, err := partials.Clone()
	if err != nil {
		panic(err)
	}
	return template.Must(t.ParseFS(htmlFS, file))
}

var (
	libraryTmpl    = buildPageTmpl("html/library.html")
	uploadTmpl     = buildPageTmpl("html/upload.html")
	playlistsTmpl  = buildPageTmpl("html/playlists.html")
	madnetworkTmpl = buildPageTmpl("html/madnetwork.html")
	// The two node surfaces of the madnetwork section (docs/ui/madnetwork-nodes.md):
	// the directory, and one node addressed by its public key. The node page is
	// the same document for every key — the key is read from the path by the page
	// module, so nothing about a node's identity passes through a template.
	mnNodesTmpl = buildPageTmpl("html/madnetwork-nodes.html")
	mnNodeTmpl  = buildPageTmpl("html/madnetwork-node.html")
	settingsTmpl   = buildPageTmpl("html/settings.html")
	adminTmpl      = buildPageTmpl("html/admin/dashboard.html") // /admin landing
)

// adminSubPages are the reworked admin sub-pages, each its own routed page under
// /admin/* sharing the admin shell. The key is the route suffix and the .SubPage
// value; the value is the template. The /admin landing is the dashboard
// (adminTmpl, SubPage ""). See docs/ui/shells.md.
var adminSubPages = map[string]*template.Template{
	// "library" is the unified file-management page (the Full Library / Review /
	// Trash scopes folded together — docs/architecture/file-management-view.md).
	// Full Library carries four lenses (By entity / All Appearances / Recordings
	// / Files); the recording-centric curation view (recording-tagsets P5) lives
	// at /admin/library#recordings.
	"library": buildPageTmpl("html/admin/library.html"),
	// "upload" is the same upload body as /upload wrapped in the admin shell, so
	// the upload page renders in whichever shell it was reached from
	// (docs/ui/shells.md).
	"upload": buildPageTmpl("html/admin/upload.html"),
	"users":  buildPageTmpl("html/admin/users.html"),
	"prune":  buildPageTmpl("html/admin/prune.html"),
	// "duplicates" lists same-audio recordings with >1 rendition (recordings P2,
	// docs/architecture/recordings.md). Moderator-accessible.
	"duplicates": buildPageTmpl("html/admin/duplicates.html"),
	// "sources" manages in-place symlink imports (data-sources P6,
	// docs/architecture/data-sources.md). Moderator-accessible.
	"sources": buildPageTmpl("html/admin/sources.html"),
	// "network" is the madnetwork friendship page (federation F1,
	// docs/architecture/federation.md): node card export/import, the trusted-peer
	// list, block/unblock. Gated on federation.manage at the API.
	"network": buildPageTmpl("html/admin/network.html"),
	// "upgrades" lists renditions the madnetwork holds that rank above ours
	// (federation F8 item 3, docs/architecture/federation.md §Quality upgrades).
	// Its API is registered only when a madnetwork store is wired, so on a node
	// with federation off the page renders and reports nothing.
	"upgrades": buildPageTmpl("html/admin/upgrades.html"),
	"settings": buildPageTmpl("html/admin/settings.html"),
}

// pageData is the data injected into every page. APIURL is the absolute API
// origin written into <meta name="api-url">; it is empty for the bundled,
// same-origin server so the front-end falls back to relative URLs.
// Page is the current page identifier used by the shared header partial to
// mark the active nav link ("library", "playlists", "upload", "admin")
// and, on the listening pages, the active library subtab ("library" = Music,
// "playlists").
// Section groups a header tab across its subtabs: the listening pages (Music +
// Playlists, future Most played / Recently / Podcasts) all carry Section
// "library", so the single "Library" header tab stays active across them while
// the in-page subtab bar shows which subtab is open. Empty for pages that are
// their own section (upload, admin).
// SubPage marks the active link in the secondary admin nav ("" = Overview,
// "library", "users", "prune", "settings"); it is empty for non-admin pages.
// GitRepo is the URL behind the header's GitRepo nav button
// (config.WebUIConfig.GitRepoURL()); empty hides the button.
// Version and Commit feed the header's About box: Version is the release tag
// (or short commit hash, or "" when unknown at build time) and Commit is the
// full commit hash; both come from internal/version and are constant per build.
// The auth fields are filled per-request from the request-context identity so the
// shared header renders its true state from the first paint — no client-side FOUC
// of the user area, and no flash of nav links the principal can't use. They are a
// UX hint only; the API still enforces every gate. SignedIn drives the user area
// vs. Sign-in button (and carries Username); Can* gate the Upload/Admin nav links
// and the Playlists subtab. Login and logout both reload the page, so the server
// re-renders on every auth-state change — the client never has to add links.
type pageData struct {
	APIURL  string
	Page    string
	Section string
	SubPage string
	GitRepo string
	Version string
	Commit  string

	SignedIn      bool
	Username      string
	CanUpload     bool
	CanAdmin      bool
	CanPlaylists  bool
	CanMadnetwork bool
}

// verInfo is resolved once: the build metadata is constant for the process.
var verInfo = version.Get()

func makeHandler(tmpl *template.Template, tmplName string, data pageData) http.HandlerFunc {
	data.Version = verInfo.Version
	data.Commit = verInfo.Commit
	return func(w http.ResponseWriter, r *http.Request) {
		// Copy the closure's data so concurrent requests don't race on the per-
		// request auth fields, then fill them from the context identity.
		d := data
		if id := auth.FromContext(r.Context()); id != nil {
			d.SignedIn = true
			d.Username = id.Username
			d.CanUpload = id.Has(auth.PermFileUpload)
			d.CanAdmin = id.Has(auth.PermFileDelete) || id.Has(auth.PermUserManage)
			d.CanPlaylists = id.Has(auth.PermContentAccess)
			d.CanMadnetwork = id.Has(auth.PermMadnetworkAccess)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		// The header now carries per-user auth state — keep shared caches from
		// serving one user's rendered header to another.
		w.Header().Set("Cache-Control", "no-store")
		if err := tmpl.ExecuteTemplate(w, tmplName, d); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
	}
}

func noCacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

// Register mounts the web UI route group on r: the library page at "/", the
// listening pages, and the static assets under
// "/static/". apiBase is the API origin for the page (empty = relative,
// same-origin); gitRepo is the header GitRepo button's URL (empty = hidden).
// Templates and static assets are served from the embedded filesystem, so the
// binary needs no access to webui/ on disk.
func Register(r chi.Router, apiBase, gitRepo string) {
	static := noCacheStatic(http.FileServer(http.FS(staticRoot)))
	r.Handle("/static/*", http.StripPrefix("/static/", static))
	r.Get("/upload", makeHandler(uploadTmpl, "upload.html", pageData{APIURL: apiBase, Page: "upload", GitRepo: gitRepo}))
	r.Get("/playlists", makeHandler(playlistsTmpl, "playlists.html", pageData{APIURL: apiBase, Page: "playlists", Section: "library", GitRepo: gitRepo}))
	// The madnetwork section (federation F2): browsing the merged catalog of
	// this node's friends. Shell-native so local playback survives browsing it;
	// the nav link is server-gated on madnetwork.access (the API enforces the
	// same gate on its data).
	r.Get("/madnetwork", makeHandler(madnetworkTmpl, "madnetwork.html", pageData{APIURL: apiBase, Page: "madnetwork", Section: "madnetwork", GitRepo: gitRepo}))
	// The section's other two pages. A node's URL carries its public key, which
	// is its identity everywhere — the catalog-source id is a local row number
	// the discovery rotation recycles, so it could not be a link
	// (docs/ui/madnetwork-nodes.md §Why a node needs an address).
	r.Get("/madnetwork/nodes", makeHandler(mnNodesTmpl, "madnetwork-nodes.html", pageData{APIURL: apiBase, Page: "madnetwork-nodes", Section: "madnetwork", GitRepo: gitRepo}))
	r.Get("/madnetwork/node/{key}", makeHandler(mnNodeTmpl, "madnetwork-node.html", pageData{APIURL: apiBase, Page: "madnetwork-node", Section: "madnetwork", GitRepo: gitRepo}))
	// Settings is its own page (not part of the Library section) reached from the
	// header's right-side user area; see docs/ui/user-settings.md.
	r.Get("/settings", makeHandler(settingsTmpl, "settings.html", pageData{APIURL: apiBase, Page: "settings", GitRepo: gitRepo}))
	r.Get("/", makeHandler(libraryTmpl, "library.html", pageData{APIURL: apiBase, Page: "library", Section: "library", GitRepo: gitRepo}))
}

// RegisterAdminPage mounts the /admin page. It belongs to the admin route
// group (alongside the API's /api/admin/* endpoints), not the webui group.
func RegisterAdminPage(r chi.Router, apiBase, gitRepo string) {
	r.Get("/admin", makeHandler(adminTmpl, "dashboard.html", pageData{APIURL: apiBase, Page: "admin", GitRepo: gitRepo}))
	for sub, tmpl := range adminSubPages {
		file := sub + ".html" // template name = file base, e.g. "files.html"
		r.Get("/admin/"+sub, makeHandler(tmpl, file, pageData{APIURL: apiBase, Page: "admin", SubPage: sub, GitRepo: gitRepo}))
	}
}
