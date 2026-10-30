package http

import (
	"encoding/csv"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"veripago-saas/internal/domain"
)

// Categorías con las que el panel agrupa los resultados.
const (
	categoryOK       = "ok"        // pago confirmado
	categoryFraud    = "fraud"     // comprobante falso, editado, repetido o pago ya usado
	categoryRejected = "rejected"  // otra cuenta o no es un comprobante (no necesariamente fraude)
	categoryNotFound = "not_found" // el pago no ingresó (o está en espera)
)

var categoryStatuses = map[string][]domain.ValidationStatus{
	categoryOK: {domain.StatusVerifiedExactID, domain.StatusVerifiedHeuristic},
	categoryFraud: {
		domain.StatusFraudTampered, domain.StatusFraudDuplicateFile, domain.StatusFraudAlreadyUsed,
		domain.StatusFraudIncomplete, domain.StatusRejectedAmountDiff,
	},
	categoryRejected: {domain.StatusRejectedWrongDest, domain.StatusInvalidReceiptImage},
	categoryNotFound: {domain.StatusRejectedNotFound},
}

func categoryOf(s domain.ValidationStatus) string {
	for cat, list := range categoryStatuses {
		for _, x := range list {
			if x == s {
				return cat
			}
		}
	}
	return categoryRejected
}

// validationItem es un comprobante del historial con su categoría y el resumen de su cliente.
type validationItem struct {
	*domain.ReceiptValidation
	Category string         `json:"category"`
	Customer *customerBrief `json:"customer,omitempty"`
}

// customerBrief es lo que se muestra del cliente en cada fila del historial.
type customerBrief struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	CUIT     string `json:"cuit,omitempty"`
	Receipts int    `json:"receipts"`
	Verified int    `json:"verified"`
	Fraud    int    `json:"fraud"`
}

// parseValidationFilter lee date (AAAA-MM-DD, día local) y category de la URL.
func parseValidationFilter(r *http.Request) (domain.ValidationFilter, error) {
	q := r.URL.Query()
	var f domain.ValidationFilter
	if d := strings.TrimSpace(q.Get("date")); d != "" {
		day, err := time.ParseInLocation("2006-01-02", d, time.Local)
		if err != nil {
			return f, errors.New("Fecha inválida (usá AAAA-MM-DD)")
		}
		next := day.AddDate(0, 0, 1)
		f.From, f.To = &day, &next
	}
	f.CustomerID = strings.TrimSpace(q.Get("customer_id"))
	if c := strings.TrimSpace(q.Get("category")); c != "" && c != "all" {
		statuses, ok := categoryStatuses[c]
		if !ok {
			return f, errors.New("Categoría inválida")
		}
		f.Statuses = statuses
	}
	return f, nil
}

// handleValidations lista el historial paginado (GET) o borra un comprobante (DELETE).
func (h *WebhooksHandler) handleValidations(w http.ResponseWriter, r *http.Request) {
	tenantID := h.activeTenantID(r)
	switch r.Method {
	case http.MethodGet:
		f, err := parseValidationFilter(r)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if page < 1 {
			page = 1
		}
		if size < 1 || size > 100 {
			size = 20
		}
		f.Limit, f.Offset = size, (page-1)*size

		list, total, err := h.repo.SearchReceiptValidations(r.Context(), tenantID, f)
		if err != nil {
			log.Printf("⚠️ Error listando comprobantes: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo cargar el historial"})
			return
		}
		// Resumen del cliente de cada fila (una consulta para toda la página)
		var ids []string
		seen := map[string]bool{}
		for _, v := range list {
			if v.CustomerID != "" && !seen[v.CustomerID] {
				seen[v.CustomerID] = true
				ids = append(ids, v.CustomerID)
			}
		}
		customers, err := h.repo.CustomerSummariesByIDs(r.Context(), tenantID, ids)
		if err != nil {
			log.Printf("⚠️ Error cargando los clientes del historial: %v", err)
		}
		items := make([]validationItem, 0, len(list))
		for _, v := range list {
			item := validationItem{ReceiptValidation: v, Category: categoryOf(v.ValidationResult)}
			if c, ok := customers[v.CustomerID]; ok {
				item.Customer = &customerBrief{ID: c.ID, Name: c.Name, CUIT: c.CUIT, Receipts: c.Receipts, Verified: c.Verified, Fraud: c.Fraud}
			}
			items = append(items, item)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"items":     items,
			"total":     total,
			"page":      page,
			"page_size": size,
		})

	case http.MethodDelete:
		id := strings.TrimSpace(r.URL.Query().Get("id"))
		released, err := h.repo.DeleteReceiptValidation(r.Context(), tenantID, id)
		if errors.Is(err, domain.ErrValidationNotFound) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Ese comprobante ya no existe"})
			return
		}
		if err != nil {
			log.Printf("⚠️ Error borrando el comprobante %s: %v", id, err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo eliminar el comprobante"})
			return
		}
		// Si estaba esperando que entre el pago, deja de esperar
		_ = h.queue.RemovePendingReceipt(r.Context(), tenantID, id)
		writeJSON(w, http.StatusOK, map[string]interface{}{"status": "deleted", "payment_released": released})

	default:
		w.Header().Set("Allow", "GET, DELETE")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
	}
}

// todayStats cuenta los comprobantes de hoy por categoría.
func (h *WebhooksHandler) todayStats(r *http.Request, tenantID string) map[string]int {
	now := time.Now()
	start := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local)
	out := map[string]int{categoryOK: 0, categoryFraud: 0, categoryRejected: 0, categoryNotFound: 0}
	counts, err := h.repo.CountValidationsByStatus(r.Context(), tenantID, start, start.AddDate(0, 0, 1))
	if err != nil {
		log.Printf("⚠️ Error contando comprobantes de hoy: %v", err)
		return out
	}
	for status, n := range counts {
		out[categoryOf(status)] += n
	}
	return out
}

// csvExportMax limita la cantidad de filas de una exportación.
const csvExportMax = 10000

// handleExportValidationsCSV exporta la auditoría de comprobantes (con los mismos filtros del
// panel) en CSV compatible con Excel.
func (h *WebhooksHandler) handleExportValidationsCSV(w http.ResponseWriter, r *http.Request) {
	f, err := parseValidationFilter(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tenantID := h.activeTenantID(r)
	var all []*domain.ReceiptValidation
	const pageSize = 200
	for offset := 0; offset < csvExportMax; offset += pageSize {
		f.Limit, f.Offset = pageSize, offset
		page, total, err := h.repo.SearchReceiptValidations(r.Context(), tenantID, f)
		if err != nil {
			http.Error(w, "No se pudo exportar", http.StatusInternalServerError)
			return
		}
		all = append(all, page...)
		if len(page) < pageSize || offset+pageSize >= total {
			break
		}
	}

	name := "veripago_comprobantes"
	if d := r.URL.Query().Get("date"); d != "" && f.From != nil {
		name += "_" + f.From.Format("2006-01-02")
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`.csv"`)
	_, _ = w.Write([]byte{0xEF, 0xBB, 0xBF}) // UTF-8 BOM para Excel

	writer := csv.NewWriter(w)
	_ = writer.Write([]string{
		"ID Validacion", "Fecha y Hora", "Telefono Empleado", "Resultado Conciliacion", "Categoria",
		"Monto Extraido (ARS)", "Nro Operacion", "Banco / Billetera Origen",
		"Nombre Emisor", "CUIT Emisor", "Alias/CVU Destino", "Hash SHA-256 Archivo", "Tiempo Procesamiento (ms)",
	})
	for _, v := range all {
		_ = writer.Write([]string{
			v.ID,
			v.CreatedAt.Format("2006-01-02 15:04:05"),
			csvSafe(v.SenderPhone),
			string(v.ValidationResult),
			categoryOf(v.ValidationResult),
			fmt.Sprintf("%.2f", v.ExtractedAmount),
			csvSafe(v.ExtractedOpNumber),
			csvSafe(v.ExtractedSourceBank),
			csvSafe(v.ExtractedSenderName),
			csvSafe(v.ExtractedSenderCUIT),
			csvSafe(v.ExtractedReceiverAliasOrCBU),
			v.FileSHA256,
			strconv.Itoa(v.ProcessingTimeMs),
		})
	}
	writer.Flush()
}
