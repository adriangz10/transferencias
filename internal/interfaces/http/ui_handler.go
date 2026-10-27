package http

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

// Panel web (Angular + Tailwind). Se compila desde web/ con `npm run build`, que deja los
// archivos en ./panel; quedan incluidos dentro del binario.
//
//go:embed all:panel
var panelEmbed embed.FS

var panelFS = func() fs.FS {
	sub, err := fs.Sub(panelEmbed, "panel")
	if err != nil {
		panic(err)
	}
	return sub
}()

// handleWebUI sirve el panel:
//   - los archivos estáticos (JS, CSS, íconos) son públicos: no tienen datos del comercio;
//   - cualquier otra ruta (/, /cobrar, /configuracion…) es una página de la app y pide sesión.
func (h *WebhooksHandler) handleWebUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	name := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if name != "" && name != "index.html" {
		if st, err := fs.Stat(panelFS, name); err == nil && !st.IsDir() {
			// Los nombres de JS y CSS llevan un hash: se pueden guardar en caché para siempre
			if ext := path.Ext(name); ext == ".js" || ext == ".css" {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=3600")
			}
			http.ServeFileFS(w, r, panelFS, name)
			return
		}
		// Un archivo que no existe (ej. /algo.js) no es una página de la app
		if path.Ext(name) != "" {
			http.NotFound(w, r)
			return
		}
	}
	h.requireAdmin(servePanelIndex)(w, r)
}

// servePanelIndex devuelve la app; el router de Angular decide qué pantalla mostrar.
func servePanelIndex(w http.ResponseWriter, r *http.Request) {
	index, err := fs.ReadFile(panelFS, "index.html")
	if err != nil {
		http.Error(w, "El panel no está compilado: corré `npm run build` en web/", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "same-origin")
	_, _ = w.Write(index)
}
