package http

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"unicode/utf8"

	"veripago-saas/internal/domain"
	"veripago-saas/internal/infrastructure/persistence"
)

// handleCustomers lista los clientes (con búsqueda y filtro de fraude) o devuelve uno (?id=).
func (h *WebhooksHandler) handleCustomers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	q := r.URL.Query()

	if id := strings.TrimSpace(q.Get("id")); id != "" {
		c, err := h.repo.GetCustomerSummary(r.Context(), h.tenantID, id)
		if errors.Is(err, persistence.ErrCustomerNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "El cliente no existe"})
			return
		}
		if err != nil {
			log.Printf("⚠️ Error leyendo el cliente %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo cargar el cliente"})
			return
		}
		writeJSON(w, http.StatusOK, c)
		return
	}

	page, _ := strconv.Atoi(q.Get("page"))
	size, _ := strconv.Atoi(q.Get("page_size"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	query := strings.TrimSpace(q.Get("q"))
	if utf8.RuneCountInString(query) > 100 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Búsqueda demasiado larga"})
		return
	}
	f := domain.CustomerFilter{Query: query, OnlyFraud: q.Get("risk") == "fraud", Limit: size, Offset: (page - 1) * size}
	items, total, err := h.repo.SearchCustomers(r.Context(), h.tenantID, f)
	if err != nil {
		log.Printf("⚠️ Error buscando clientes: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo cargar la lista de clientes"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": items, "total": total, "page": page, "page_size": size})
}

// handleBusiness guarda los datos del comercio (nombre, CUIT y email) cargados en el panel.
func (h *WebhooksHandler) handleBusiness(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		BusinessName string `json:"business_name"`
		CUIT         string `json:"cuit"`
		Email        string `json:"email"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	name := strings.TrimSpace(req.BusinessName)
	if name == "" || utf8.RuneCountInString(name) > 150 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ingresá el nombre del comercio (hasta 150 caracteres)"})
		return
	}
	cuit := strings.TrimSpace(req.CUIT)
	if cuit != "" && domain.NormalizeCUIT(cuit) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "El CUIT debe tener 11 números"})
		return
	}
	email := strings.TrimSpace(req.Email)
	if email != "" {
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email || len(email) > 150 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "El email no es válido"})
			return
		}
	}
	if err := h.repo.UpdateTenantProfile(r.Context(), h.tenantID, name, domain.NormalizeCUIT(cuit), email); err != nil {
		log.Printf("⚠️ Error guardando los datos del comercio: %v", err)
		msg := "No se pudieron guardar los datos"
		if strings.Contains(err.Error(), "Duplicate") {
			msg = "Ese CUIT o email ya está registrado"
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
