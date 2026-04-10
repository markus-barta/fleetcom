package api

import (
	_ "embed"
	"net/http"
)

//go:embed LICENSE
var licenseText string

func License(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Write([]byte(licenseText))
}
