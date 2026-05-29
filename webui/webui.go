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

func Route() {
	static := http.FileServer(http.Dir("webui/static"))
	http.Handle("/static/", http.StripPrefix("/static/", static))
	http.HandleFunc("/upload", showUploadForm)
	http.HandleFunc("/library", showLibrary)
	http.HandleFunc("/", showLibrary)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
