//go:build !nowebui

package webui

import (
	"html/template"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// Available reports whether the web UI is compiled into this binary. It is
// false in builds made with -tags nowebui (see webui_stub.go), which lets
// config validation reject listeners that ask to serve the UI.
const Available = true

var (
	cmusTmpl    = template.Must(template.ParseFiles("webui/html/cmus.html"))
	libraryTmpl = template.Must(template.ParseFiles("webui/html/library.html"))
	adminTmpl   = template.Must(template.ParseFiles("webui/html/admin.html"))
)

// pageData is the data injected into every page. APIURL is the absolute API
// origin written into <meta name="api-url">; it is empty for the bundled,
// same-origin server so the front-end falls back to relative URLs.
type pageData struct {
	APIURL string
}

func makeHandler(tmpl *template.Template, apiBase string) http.HandlerFunc {
	data := pageData{APIURL: apiBase}
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err := tmpl.Execute(w, data); err != nil {
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
// files are read relative to the working directory, so the binary must be run
// from the project root.
func Register(r chi.Router, apiBase string) {
	static := noCacheStatic(http.FileServer(http.Dir("webui/static")))
	r.Handle("/static/*", http.StripPrefix("/static/", static))
	r.Get("/cmus", makeHandler(cmusTmpl, apiBase))
	r.Get("/", makeHandler(libraryTmpl, apiBase))
}

// RegisterAdminPage mounts the /admin page. It belongs to the admin route
// group (alongside the API's /api/admin/* endpoints), not the webui group.
func RegisterAdminPage(r chi.Router, apiBase string) {
	r.Get("/admin", makeHandler(adminTmpl, apiBase))
}
