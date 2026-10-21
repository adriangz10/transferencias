package http

import (
	_ "embed"
	"net/http"
)

// Páginas del panel, incluidas dentro del binario.
var (
	//go:embed dashboard.html
	dashboardHTML []byte

	//go:embed login.html
	loginHTML []byte
)

func (h *WebhooksHandler) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(dashboardHTML)
}
