package webui

import (
	"html/template"
	"log"
	"net/http"
)

var (
	cmusTmpl    = template.Must(template.ParseFiles("webui/html/cmus.html"))
	libraryTmpl = template.Must(template.ParseFiles("webui/html/library.html"))
)

type pageData struct {
	APIURL string
}

func makeHandler(tmpl *template.Template, apiURL string) http.HandlerFunc {
	data := pageData{APIURL: apiURL}
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

func Route(addr, apiURL string) {
	static := noCacheStatic(http.FileServer(http.Dir("webui/static")))
	http.Handle("/static/", http.StripPrefix("/static/", static))
	http.HandleFunc("/cmus", makeHandler(cmusTmpl, apiURL))
	http.HandleFunc("/", makeHandler(libraryTmpl, apiURL))
	log.Fatal(http.ListenAndServe(addr, nil))
}
