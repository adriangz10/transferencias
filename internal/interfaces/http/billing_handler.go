package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"veripago-saas/internal/domain"
)

// ---------------------------------------------------------------------------
// Facturación y Suscripciones de Comercios (Checkout Mercado Pago / Demo / Manual)
// ---------------------------------------------------------------------------

// handleBillingOrders lista las órdenes de suscripción del comercio actual.
func (h *WebhooksHandler) handleBillingOrders(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	tenantID := h.activeTenantID(r)
	orders, err := h.repo.ListSubscriptionOrders(r.Context(), tenantID, 20)
	if err != nil {
		log.Printf("⚠️ Error listando órdenes de suscripción: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudieron cargar las suscripciones"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"orders":       orders,
		"usd_ars_rate": h.cfg.USDToARSRate,
	})
}

// handleBillingCheckout inicia la contratación o upgrade de un plan comercial.
func (h *WebhooksHandler) handleBillingCheckout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		Plan string `json:"plan"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}

	plan := domain.PlanType(strings.ToUpper(strings.TrimSpace(req.Plan)))
	spec, ok := domain.GetPlanSpecs()[plan]
	if !ok || plan == domain.PlanFreeTrial {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Seleccioná un plan comercial válido (Emprendedor, Comercio Pro o Full)"})
		return
	}

	ctx := r.Context()
	tenantID := h.activeTenantID(r)
	tenant, err := h.repo.GetTenantByID(ctx, tenantID)
	if err != nil || tenant == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Comercio no encontrado"})
		return
	}

	rate := h.cfg.USDToARSRate
	if rate <= 0 {
		rate = 1200
	}
	amountARS := float64(spec.PriceUSD) * rate
	orderID := uuid.NewString()
	title := fmt.Sprintf("VeriPago SaaS · Plan %s (Mensual)", spec.Name)

	order := &domain.SubscriptionOrder{
		ID:           orderID,
		TenantID:     tenantID,
		BusinessName: tenant.BusinessName,
		TenantEmail:  tenant.Email,
		PlanType:     plan,
		PriceUSD:     spec.PriceUSD,
		AmountARS:    amountARS,
		Status:       "PENDING",
		PaymentMode:  "MANUAL",
	}

	if h.cfg.DemoMode {
		order.PaymentMode = "DEMO"
		order.MPPreferenceID = "PREF_DEMO_" + orderID[:8]
		order.CheckoutURL = "/configuracion?sub_demo=" + orderID
	} else if h.cfg.MPPlatformAccessToken != "" {
		prefID, initPoint, prefErr := h.mpClient.CreateSubscriptionPreference(
			ctx, h.cfg.MPPlatformAccessToken, orderID, title, amountARS, tenant.Email, h.cfg.PublicBaseURL,
		)
		if prefErr != nil {
			log.Printf("⚠️ Error creando preferencia MP para suscripción %s: %v", orderID, prefErr)
		} else {
			order.PaymentMode = "MP_CHECKOUT"
			order.MPPreferenceID = prefID
			order.CheckoutURL = initPoint
		}
	}

	if err := h.repo.CreateSubscriptionOrder(ctx, order); err != nil {
		log.Printf("⚠️ Error guardando orden de suscripción: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo iniciar la suscripción"})
		return
	}

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"order":        order,
		"checkout_url": order.CheckoutURL,
		"payment_mode": order.PaymentMode,
	})
}

// handleBillingSimulatePay permite acreditar el pago de una orden de suscripción en modo demo (o por Super-Admin).
func (h *WebhooksHandler) handleBillingSimulatePay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	if !h.cfg.DemoMode && !h.isSuperAdmin(r) {
		http.NotFound(w, r)
		return
	}

	var req struct {
		OrderID string `json:"order_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || strings.TrimSpace(req.OrderID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id requerido"})
		return
	}

	ctx := r.Context()
	order, err := h.repo.GetSubscriptionOrder(ctx, strings.TrimSpace(req.OrderID))
	if errors.Is(err, domain.ErrSubscriptionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Orden de suscripción no encontrada"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Error consultando la orden"})
		return
	}
	// Un comercio solo puede simular sus propias órdenes
	if !h.isSuperAdmin(r) && order.TenantID != h.activeTenantID(r) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "No tenés permiso sobre esta orden"})
		return
	}

	approved, err := h.repo.ApproveSubscriptionOrder(ctx, order.ID, randomDemoMPID(8))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo activar la suscripción"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "approved",
		"order":  approved,
	})
}

// handleMPPlatformWebhook recibe notificaciones de pago de la cuenta de Mercado Pago de la plataforma SaaS
// para activar automáticamente las suscripciones pagadas por Checkout Pro.
func (h *WebhooksHandler) handleMPPlatformWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var body struct {
		Type string `json:"type"`
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, mpBodyLimit)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	dataID := r.URL.Query().Get("data.id")
	if dataID == "" {
		dataID = body.Data.ID
	}
	if !ValidateMPWebhookSignature(h.cfg.MPWebhookSecret, r) {
		http.Error(w, "Invalid x-signature HMAC", http.StatusUnauthorized)
		return
	}
	if dataID == "" || h.cfg.MPPlatformAccessToken == "" {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ignored"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})

	if !h.acquireWorker() {
		return
	}
	h.runInBackground(mpProcessTimeout, func(ctx context.Context) {
		info, err := h.mpClient.GetPlatformPayment(ctx, h.cfg.MPPlatformAccessToken, dataID)
		if err != nil || info == nil || info.Status != "approved" {
			return
		}
		if strings.HasPrefix(info.ExternalReference, "sub_") {
			orderID := strings.TrimPrefix(info.ExternalReference, "sub_")
			if _, err := h.repo.ApproveSubscriptionOrder(ctx, orderID, info.ID); err != nil {
				log.Printf("⚠️ Webhook plataforma MP: no se pudo aprobar suscripción %s: %v", orderID, err)
			}
		}
	})
}

// ---------------------------------------------------------------------------
// Panel y API de Super-Admin (/api/v1/superadmin/*)
// ---------------------------------------------------------------------------

// handleSuperAdminTenants lista todos los comercios con métricas globales (GET) o crea/edita un comercio (POST).
func (h *WebhooksHandler) handleSuperAdminTenants(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	switch r.Method {
	case http.MethodGet:
		q := strings.TrimSpace(r.URL.Query().Get("q"))
		tenants, stats, err := h.repo.ListTenantsAdmin(ctx, q)
		if err != nil {
			log.Printf("⚠️ Super-Admin: error listando comercios: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo cargar la lista de comercios"})
			return
		}
		orders, _ := h.repo.ListSubscriptionOrders(ctx, "", 50)
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"tenants":             tenants,
			"stats":               stats,
			"subscription_orders": orders,
			"plans":               domain.GetPlanSpecs(),
			"active_tenant_id":    h.activeTenantID(r),
			"impersonating":       h.isImpersonating(r),
		})

	case http.MethodPost:
		var req struct {
			TenantID           string `json:"tenant_id"`
			BusinessName       string `json:"business_name"`
			CUIT               string `json:"cuit"`
			Email              string `json:"email"`
			Password           string `json:"password"`
			PlanType           string `json:"plan_type"`
			MonthlyQuota       int    `json:"monthly_quota"`
			IsActive           *bool  `json:"is_active"`
			SubscriptionStatus string `json:"subscription_status"`
			ResetQuota         bool   `json:"reset_quota"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
			return
		}

		plan := domain.PlanType(strings.ToUpper(strings.TrimSpace(req.PlanType)))
		spec, ok := domain.GetPlanSpecs()[plan]
		if !ok {
			plan = domain.PlanFreeTrial
			spec = domain.GetPlanSpecs()[plan]
		}
		quota := req.MonthlyQuota
		if quota <= 0 {
			quota = spec.MonthlyQuota
		}

		// a) Edición de un comercio existente
		if strings.TrimSpace(req.TenantID) != "" {
			tenantID := strings.TrimSpace(req.TenantID)
			existing, err := h.repo.GetTenantByID(ctx, tenantID)
			if err != nil || existing == nil {
				writeJSON(w, http.StatusNotFound, map[string]string{"error": "El comercio no existe"})
				return
			}
			isActive := existing.IsActive
			if req.IsActive != nil {
				isActive = *req.IsActive
			}
			subStatus := strings.ToUpper(strings.TrimSpace(req.SubscriptionStatus))
			if subStatus == "" {
				subStatus = existing.SubscriptionStatus
				if plan != domain.PlanFreeTrial && subStatus == "TRIAL" {
					subStatus = "ACTIVE"
				}
			}
			var expiresAt *time.Time
			if plan != existing.PlanType && plan != domain.PlanFreeTrial {
				exp := time.Now().AddDate(0, 1, 0)
				expiresAt = &exp
			}
			if err := h.repo.UpdateTenantAdmin(ctx, tenantID, plan, quota, isActive, subStatus, expiresAt, req.ResetQuota); err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo actualizar el comercio"})
				return
			}
			if strings.TrimSpace(req.BusinessName) != "" || strings.TrimSpace(req.Email) != "" || strings.TrimSpace(req.CUIT) != "" {
				name := strings.TrimSpace(req.BusinessName)
				if name == "" {
					name = existing.BusinessName
				}
				cuit := strings.TrimSpace(req.CUIT)
				if cuit == "" {
					cuit = existing.CUIT
				}
				email := strings.TrimSpace(req.Email)
				if email == "" {
					email = existing.Email
				}
				if err := h.repo.UpdateTenantProfile(ctx, tenantID, name, cuit, email); err != nil {
					writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
					return
				}
			}
			if strings.TrimSpace(req.Password) != "" {
				if len(req.Password) < 8 {
					writeJSON(w, http.StatusBadRequest, map[string]string{"error": "La contraseña debe tener al menos 8 caracteres"})
					return
				}
				hash, _ := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
				_ = h.repo.UpdateTenantPassword(ctx, tenantID, string(hash))
			}
			writeJSON(w, http.StatusOK, map[string]string{"status": "updated"})
			return
		}

		// b) Alta manual de un comercio nuevo desde Super-Admin
		name := strings.TrimSpace(req.BusinessName)
		if name == "" || utf8.RuneCountInString(name) > 150 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ingresá el nombre del comercio"})
			return
		}
		email := strings.ToLower(strings.TrimSpace(req.Email))
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Ingresá un email válido"})
			return
		}
		if len(req.Password) < 8 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "La contraseña debe tener al menos 8 caracteres"})
			return
		}
		cuit := strings.TrimSpace(req.CUIT)
		if cuit != "" && domain.NormalizeCUIT(cuit) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "El CUIT debe tener 11 dígitos"})
			return
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "Error procesando contraseña"})
			return
		}
		subStatus := "TRIAL"
		exp := time.Now().AddDate(0, 0, 14)
		if plan != domain.PlanFreeTrial {
			subStatus = "ACTIVE"
			exp = time.Now().AddDate(0, 1, 0)
		}
		newTenant := &domain.Tenant{
			BusinessName:       name,
			CUIT:               domain.NormalizeCUIT(cuit),
			Email:              email,
			PasswordHash:       string(hash),
			PlanType:           plan,
			MonthlyQuota:       quota,
			TimeWindowMinutes:  domain.DefaultTimeWindowMinutes,
			SubscriptionStatus: subStatus,
			PlanExpiresAt:      &exp,
			IsActive:           true,
		}
		if err := h.repo.CreateTenant(ctx, newTenant); err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, domain.ErrTenantEmailTaken) || errors.Is(err, domain.ErrTenantCUITTaken) {
				status = http.StatusConflict
			}
			writeJSON(w, status, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, newTenant)

	default:
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
	}
}

// handleSuperAdminImpersonate permite al Super-Admin seleccionar qué comercio inspeccionar en el panel.
func (h *WebhooksHandler) handleSuperAdminImpersonate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		TenantID string `json:"tenant_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "JSON inválido"})
		return
	}
	targetID := strings.TrimSpace(req.TenantID)
	impersonating := true
	if targetID == "" || targetID == "clear" {
		targetID = h.tenantID
		impersonating = false
	} else {
		if t, err := h.repo.GetTenantByID(r.Context(), targetID); err != nil || t == nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Comercio no encontrado"})
			return
		}
	}

	if c, err := r.Cookie(sessionCookieName); err == nil {
		h.sessions.setTenant(c.Value, targetID, impersonating)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":           "ok",
		"active_tenant_id": targetID,
		"impersonating":    impersonating,
	})
}

// handleSuperAdminApproveSubscription aprueba manualmente una orden de suscripción pendiente.
func (h *WebhooksHandler) handleSuperAdminApproveSubscription(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Método no permitido", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		OrderID string `json:"order_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil || strings.TrimSpace(req.OrderID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "order_id requerido"})
		return
	}
	approved, err := h.repo.ApproveSubscriptionOrder(r.Context(), strings.TrimSpace(req.OrderID), 0)
	if errors.Is(err, domain.ErrSubscriptionNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "La orden de suscripción no existe"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "No se pudo aprobar la suscripción"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status": "approved",
		"order":  approved,
	})
}
