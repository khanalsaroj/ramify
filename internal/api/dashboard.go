package api

import (
	"embed"
	"encoding/json"
	"io/fs"
	"net/http"
)

// dashboardAssets is embedded into ramifyd so the operational UI has no separate
// deployment or runtime dependency.
//
//go:embed dashboard/*
var dashboardAssets embed.FS

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/dashboard" {
		http.Redirect(w, r, "/dashboard/", http.StatusPermanentRedirect)
		return
	}
	assets, _ := fs.Sub(dashboardAssets, "dashboard")
	http.StripPrefix("/dashboard", http.FileServer(http.FS(assets))).ServeHTTP(w, r)
}

func (s *Server) handleDashboardConfig(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]string{"base_domain": s.baseDomain})
}
