//go:build !nowebui

package webui

import (
	"embed"
	"html/template"
	"io/fs"
	"net/http"

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
	cmusTmpl    = template.Must(template.ParseFS(htmlFS, "html/cmus.html"))
	libraryTmpl = buildPageTmpl("html/library.html")
	uploadTmpl  = buildPageTmpl("html/upload.html")
	adminTmpl   = buildPageTmpl("html/admin.html")
)

// adminSubPages are the reworked admin sub-pages, each its own routed page under
// /admin/* sharing the admin shell. The key is the route suffix and the .SubPage
// value; the value is the template/file base name. The legacy /admin page
// (adminTmpl) stays until the rework is complete (see docs/plans/admin-panel-rework.md).
var adminSubPages = map[string]*template.Template{
	"files":    buildPageTmpl("html/admin/files.html"),
	"users":    buildPageTmpl("html/admin/users.html"),
	"access":   buildPageTmpl("html/admin/access.html"),
	"prune":    buildPageTmpl("html/admin/prune.html"),
	"trash":    buildPageTmpl("html/admin/trash.html"),
	"settings": buildPageTmpl("html/admin/settings.html"),
}

// pageData is the data injected into every page. APIURL is the absolute API
// origin written into <meta name="api-url">; it is empty for the bundled,
// same-origin server so the front-end falls back to relative URLs.
// Page is the current page identifier used by the shared header partial to
// mark the active nav link ("library", "upload", "admin", "cmus").
// SubPage marks the active link in the secondary admin nav ("" = Overview,
// "files", "users", "access", "prune", "trash", "settings"); it is empty for
// non-admin pages.
type pageData struct {
	APIURL  string
	Page    string
	SubPage string
}

func makeHandler(tmpl *template.Template, tmplName string, data pageData) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.ExecuteTemplate(w, tmplName, data); err != nil {
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
// cmus view at "/cmus", and the static assets under "/static/". apiBase is the
// API origin for the page (empty = relative, same-origin). Templates and static
// assets are served from the embedded filesystem, so the binary needs no
// access to webui/ on disk.
func Register(r chi.Router, apiBase string) {
	static := noCacheStatic(http.FileServer(http.FS(staticRoot)))
	r.Handle("/static/*", http.StripPrefix("/static/", static))
	r.Get("/cmus", makeHandler(cmusTmpl, "cmus.html", pageData{APIURL: apiBase}))
	r.Get("/upload", makeHandler(uploadTmpl, "upload.html", pageData{APIURL: apiBase, Page: "upload"}))
	r.Get("/", makeHandler(libraryTmpl, "library.html", pageData{APIURL: apiBase, Page: "library"}))
}

// RegisterAdminPage mounts the /admin page. It belongs to the admin route
// group (alongside the API's /api/admin/* endpoints), not the webui group.
func RegisterAdminPage(r chi.Router, apiBase string) {
	r.Get("/admin", makeHandler(adminTmpl, "admin.html", pageData{APIURL: apiBase, Page: "admin"}))
	for sub, tmpl := range adminSubPages {
		file := sub + ".html" // template name = file base, e.g. "files.html"
		r.Get("/admin/"+sub, makeHandler(tmpl, file, pageData{APIURL: apiBase, Page: "admin", SubPage: sub}))
	}
}
