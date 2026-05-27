package webui

import (
	"html/template"
	"log"
	"net/http"
)

var tmpl = template.Must(template.ParseFiles("webui/html/upload.html"))

func showUploadForm(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	if err := tmpl.Execute(w, nil); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func Route() {
	static := http.FileServer(http.Dir("webui/static"))
	http.Handle("/static/", http.StripPrefix("/static/", static))
	http.HandleFunc("/", showUploadForm)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
