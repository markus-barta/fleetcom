package api

import (
	"net/http"
	"os"
)

func LoginPage(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, "static/login.html")
}

func Dashboard(w http.ResponseWriter, r *http.Request) {
	// Serve index.html; if it doesn't exist yet, return a placeholder
	if _, err := os.Stat("static/index.html"); err != nil {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<!DOCTYPE html><html><body><h1>FleetCom</h1><p>Dashboard coming soon.</p></body></html>`))
		return
	}
	http.ServeFile(w, r, "static/index.html")
}
