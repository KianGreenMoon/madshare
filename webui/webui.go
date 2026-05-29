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

func showCmus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := cmusTmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func showLibrary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := libraryTmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func noCacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
}

func Route() {
	static := noCacheStatic(http.FileServer(http.Dir("webui/static")))
	http.Handle("/static/", http.StripPrefix("/static/", static))
	http.HandleFunc("/cmus", showCmus)
	http.HandleFunc("/", showLibrary)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
