package webui

import (
	"html/template"
	"log"
	"net/http"
)

var (
	libraryTmpl = template.Must(template.ParseFiles("webui/html/library.html"))
	uploadTmpl  = template.Must(template.ParseFiles("webui/html/upload.html"))
)

func showLibrary(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := libraryTmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func showUploadForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := uploadTmpl.Execute(w, nil); err != nil {
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
	http.HandleFunc("/library", showLibrary)
	http.HandleFunc("/", showUploadForm)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
