package server

import (
	"embed"
	"net/http"
)

//go:embed app_assets/manifest.json app_assets/favicon.ico app_assets/icon-192.png app_assets/logo.svg
var appAssetsFS embed.FS

func (a *App) registerAppManifestRoutes(mux *http.ServeMux) {
	serveAsset := func(name, contentType string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data, err := appAssetsFS.ReadFile("app_assets/" + name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			// Allow BSV Desktop / Metanet Desktop Electron renderer to fetch app manifest & icon.
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Content-Type", contentType)
			w.WriteHeader(http.StatusOK)
			if r.Method != http.MethodHead {
				_, _ = w.Write(data)
			}
		}
	}
	mux.HandleFunc("GET /app.webmanifest", serveAsset("manifest.json", "application/manifest+json; charset=utf-8"))
	if a.cfg.Hub != nil {
		hubHandler := a.cfg.Hub.Handler()
		mux.HandleFunc("GET /manifest.json", func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("format") == "pwa" {
				serveAsset("manifest.json", "application/manifest+json; charset=utf-8")(w, r)
				return
			}
			r2 := r.Clone(r.Context())
			r2.Header.Set("X-Nektar-Hub-Prefix", "/hub")
			hubHandler.ServeHTTP(w, r2)
		})
		mux.HandleFunc("GET /.well-known/metanet-handles/resolve", func(w http.ResponseWriter, r *http.Request) {
			hubHandler.ServeHTTP(w, r)
		})
	} else {
		mux.HandleFunc("GET /manifest.json", serveAsset("manifest.json", "application/manifest+json; charset=utf-8"))
	}
	mux.HandleFunc("GET /favicon.ico", serveAsset("favicon.ico", "image/x-icon"))
	mux.HandleFunc("GET /icon-192.png", serveAsset("icon-192.png", "image/png"))
	mux.HandleFunc("GET /logo.svg", serveAsset("logo.svg", "image/svg+xml"))
}
