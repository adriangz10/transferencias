package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"veripago-saas/internal/domain"
)

func doWithCookie(mux *http.ServeMux, method, path, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func extractSessionCookie(rec *httptest.ResponseRecorder) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookieName {
			return c
		}
	}
	return nil
}

func TestMultiTenantRegistrationIsolationAndPlanLimits(t *testing.T) {
	env := newTestEnv(t, true, "super-admin-pass-123")

	// 1. GET /registro es público
	if rec := env.do(http.MethodGet, "/registro", "", false); rec.Code != http.StatusOK {
		t.Fatalf("GET /registro: esperaba 200, obtuvo %d", rec.Code)
	}

	// 2. Registrar Comercio A (Panadería) con WhatsApp de dueño
	regA := `{"business_name":"Panadería Don José","email":"jose@panaderia.com","password":"password123","cuit":"30-71111111-9","owner_phone":"5491160001111"}`
	recA := doWithCookie(env.mux, http.MethodPost, "/api/v1/register", regA, nil)
	if recA.Code != http.StatusCreated {
		t.Fatalf("registro A: esperaba 201, obtuvo %d: %s", recA.Code, recA.Body.String())
	}
	cookieA := extractSessionCookie(recA)
	if cookieA == nil {
		t.Fatal("registro A debía devolver cookie de sesión")
	}

	// 3. Intentar registrar otro comercio con el mismo email o mismo CUIT -> 409 Conflict
	recDupEmail := doWithCookie(env.mux, http.MethodPost, "/api/v1/register", `{"business_name":"Otro","email":"JOSE@panaderia.com","password":"password123"}`, nil)
	if recDupEmail.Code != http.StatusConflict {
		t.Fatalf("email duplicado: esperaba 409, obtuvo %d", recDupEmail.Code)
	}
	recDupCUIT := doWithCookie(env.mux, http.MethodPost, "/api/v1/register", `{"business_name":"Otro","email":"otro@panaderia.com","password":"password123","cuit":"30711111119"}`, nil)
	if recDupCUIT.Code != http.StatusConflict {
		t.Fatalf("CUIT duplicado: esperaba 409, obtuvo %d", recDupCUIT.Code)
	}

	// 4. Registrar Comercio B (Ferretería)
	regB := `{"business_name":"Ferretería El Tornillo","email":"ana@ferreteria.com","password":"password456","cuit":"30-72222222-9"}`
	recB := doWithCookie(env.mux, http.MethodPost, "/api/v1/register", regB, nil)
	if recB.Code != http.StatusCreated {
		t.Fatalf("registro B: esperaba 201, obtuvo %d: %s", recB.Code, recB.Body.String())
	}
	cookieB := extractSessionCookie(recB)
	if cookieB == nil {
		t.Fatal("registro B debía devolver cookie de sesión")
	}

	// 5. Verificar aislamiento de sesión en /api/v1/dashboard
	dashARec := doWithCookie(env.mux, http.MethodGet, "/api/v1/dashboard", "", cookieA)
	var dashA struct {
		Tenant  domain.Tenant              `json:"tenant"`
		Senders []*domain.AuthorizedSender `json:"senders"`
	}
	_ = json.Unmarshal(dashARec.Body.Bytes(), &dashA)
	if dashA.Tenant.BusinessName != "Panadería Don José" || dashA.Tenant.PlanType != domain.PlanFreeTrial || len(dashA.Senders) != 1 {
		t.Fatalf("dashboard A inesperado: %+v senders=%d", dashA.Tenant, len(dashA.Senders))
	}

	dashBRec := doWithCookie(env.mux, http.MethodGet, "/api/v1/dashboard", "", cookieB)
	var dashB struct {
		Tenant  domain.Tenant              `json:"tenant"`
		Senders []*domain.AuthorizedSender `json:"senders"`
	}
	_ = json.Unmarshal(dashBRec.Body.Bytes(), &dashB)
	if dashB.Tenant.BusinessName != "Ferretería El Tornillo" || dashB.Tenant.ID == dashA.Tenant.ID || len(dashB.Senders) != 0 {
		t.Fatalf("dashboard B inesperado: %+v senders=%d", dashB.Tenant, len(dashB.Senders))
	}

	// 6. Comercio B no puede registrar el número de WhatsApp que ya usa el Comercio A -> 409 Conflict
	recPhoneConflict := doWithCookie(env.mux, http.MethodPost, "/api/v1/senders", `{"employee_name":"Intruso","phone_number":"5491160001111","role":"CASHIER"}`, cookieB)
	if recPhoneConflict.Code != http.StatusConflict {
		t.Fatalf("número de WhatsApp de otro comercio: esperaba 409, obtuvo %d: %s", recPhoneConflict.Code, recPhoneConflict.Body.String())
	}

	// 7. Comercio A está en FREE_TRIAL (máx 2 WhatsApp) y ya tiene 1: puede agregar un segundo, pero el tercero da 403 Forbidden
	recSecond := doWithCookie(env.mux, http.MethodPost, "/api/v1/senders", `{"employee_name":"Segundo","phone_number":"5491160002222","role":"CASHIER"}`, cookieA)
	if recSecond.Code != http.StatusOK {
		t.Fatalf("segundo WhatsApp en FREE_TRIAL: esperaba 200, obtuvo %d: %s", recSecond.Code, recSecond.Body.String())
	}
	recPlanLimit := doWithCookie(env.mux, http.MethodPost, "/api/v1/senders", `{"employee_name":"Tercero","phone_number":"5491160003333","role":"CASHIER"}`, cookieA)
	if recPlanLimit.Code != http.StatusForbidden {
		t.Fatalf("límite de WhatsApp del plan Free Trial: esperaba 403, obtuvo %d: %s", recPlanLimit.Code, recPlanLimit.Body.String())
	}
}

func TestMultiTenantLoginPasswordAndSubscriptions(t *testing.T) {
	env := newTestEnv(t, true, "super-admin-pass-123")

	// Registrar comercio
	reg := `{"business_name":"Cafetería Central","email":"cafe@central.com","password":"mipassword1","cuit":"30-73333333-9"}`
	recReg := doWithCookie(env.mux, http.MethodPost, "/api/v1/register", reg, nil)
	if recReg.Code != http.StatusCreated {
		t.Fatalf("registro: %d %s", recReg.Code, recReg.Body.String())
	}

	// Login con contraseña incorrecta
	recBad := env.login("cafe@central.com", "incorrecta", "10.1.1.1:1234")
	if recBad.Code != http.StatusSeeOther || recBad.Header().Get("Location") != "/login?error=1" {
		t.Fatalf("login incorrecto: esperaba redirect a /login?error=1, obtuvo %d %q", recBad.Code, recBad.Header().Get("Location"))
	}

	// Login con email y contraseña correctos
	recLogin := env.login("CAFE@central.com", "mipassword1", "10.1.1.1:1234")
	if recLogin.Code != http.StatusSeeOther || recLogin.Header().Get("Location") != "/" {
		t.Fatalf("login comercio válido: esperaba 303 a /, obtuvo %d %q", recLogin.Code, recLogin.Header().Get("Location"))
	}
	cookie := extractSessionCookie(recLogin)

	// Cambiar contraseña
	recPass := doWithCookie(env.mux, http.MethodPost, "/api/v1/account/password", `{"current_password":"mipassword1","new_password":"nuevapassword2"}`, cookie)
	if recPass.Code != http.StatusOK {
		t.Fatalf("cambio de contraseña: %d %s", recPass.Code, recPass.Body.String())
	}
	// La contraseña vieja ya no entra y la nueva sí
	if r := env.login("cafe@central.com", "mipassword1", "10.1.1.2:1234"); r.Header().Get("Location") != "/login?error=1" {
		t.Fatalf("la clave anterior debía fallar")
	}
	if r := env.login("cafe@central.com", "nuevapassword2", "10.1.1.2:1234"); r.Header().Get("Location") != "/" {
		t.Fatalf("la clave nueva debía entrar")
	}

	// Checkout de suscripción a PRO y pago simulado
	recCheckout := doWithCookie(env.mux, http.MethodPost, "/api/v1/billing/checkout", `{"plan":"PRO"}`, cookie)
	if recCheckout.Code != http.StatusCreated {
		t.Fatalf("checkout: %d %s", recCheckout.Code, recCheckout.Body.String())
	}
	var checkoutResp struct {
		Order domain.SubscriptionOrder `json:"order"`
	}
	_ = json.Unmarshal(recCheckout.Body.Bytes(), &checkoutResp)
	if checkoutResp.Order.ID == "" || checkoutResp.Order.Status != "PENDING" || checkoutResp.Order.PlanType != domain.PlanPro {
		t.Fatalf("orden de suscripción inesperada: %+v", checkoutResp.Order)
	}

	recSimPay := doWithCookie(env.mux, http.MethodPost, "/api/v1/billing/simulate-pay", `{"order_id":"`+checkoutResp.Order.ID+`"}`, cookie)
	if recSimPay.Code != http.StatusOK {
		t.Fatalf("simulate-pay: %d %s", recSimPay.Code, recSimPay.Body.String())
	}
	var simResp struct {
		Order domain.SubscriptionOrder `json:"order"`
	}
	_ = json.Unmarshal(recSimPay.Body.Bytes(), &simResp)
	if simResp.Order.Status != "APPROVED" {
		t.Fatalf("la orden debía quedar APPROVED, obtuvo %+v", simResp.Order)
	}

	afterTenant, err := env.handler.repo.GetTenantByID(context.Background(), checkoutResp.Order.TenantID)
	if err != nil || afterTenant.PlanType != domain.PlanPro || afterTenant.SubscriptionStatus != "ACTIVE" || afterTenant.MonthlyQuota != 1500 {
		t.Fatalf("el comercio debía actualizarse a PRO ACTIVE (1500 cupo): tenant=%+v err=%v", afterTenant, err)
	}
}

func TestSuperAdminManagementAndImpersonation(t *testing.T) {
	env := newTestEnv(t, false, "super-admin-pass-123")

	// Registrar un comercio normal
	recReg := doWithCookie(env.mux, http.MethodPost, "/api/v1/register", `{"business_name":" Librería Sur ","email":"sur@libreria.com","password":"password123"}`, nil)
	if recReg.Code != http.StatusCreated {
		t.Fatalf("registro: %d %s", recReg.Code, recReg.Body.String())
	}
	tenantCookie := extractSessionCookie(recReg)

	// Un comercio normal NO puede acceder a rutas de Super-Admin -> 403 Forbidden
	if rec := doWithCookie(env.mux, http.MethodGet, "/api/v1/superadmin/tenants", "", tenantCookie); rec.Code != http.StatusForbidden {
		t.Fatalf("comercio normal en /superadmin/tenants: esperaba 403, obtuvo %d", rec.Code)
	}

	// Super-Admin inicia sesión
	adminCookie := env.sessionCookie()
	recList := doWithCookie(env.mux, http.MethodGet, "/api/v1/superadmin/tenants", "", adminCookie)
	if recList.Code != http.StatusOK {
		t.Fatalf("superadmin list: %d %s", recList.Code, recList.Body.String())
	}
	var saData struct {
		Stats   domain.PlatformStats         `json:"stats"`
		Tenants []*domain.TenantAdminSummary `json:"tenants"`
	}
	_ = json.Unmarshal(recList.Body.Bytes(), &saData)
	if saData.Stats.TotalTenants < 2 || len(saData.Tenants) < 2 {
		t.Fatalf("esperaba al menos 2 comercios en Super-Admin, obtuvo %+v", saData.Stats)
	}

	// Encontrar ID de Librería Sur
	var surID string
	for _, item := range saData.Tenants {
		if item.Email == "sur@libreria.com" {
			surID = item.ID
		}
	}
	if surID == "" {
		t.Fatal("no se encontró Librería Sur en el listado de Super-Admin")
	}

	// Super-Admin entra en modo impersonación de Librería Sur
	recImp := doWithCookie(env.mux, http.MethodPost, "/api/v1/superadmin/impersonate", `{"tenant_id":"`+surID+`"}`, adminCookie)
	if recImp.Code != http.StatusOK {
		t.Fatalf("impersonate: %d %s", recImp.Code, recImp.Body.String())
	}
	recDash := doWithCookie(env.mux, http.MethodGet, "/api/v1/dashboard", "", adminCookie)
	var dash struct {
		Tenant        domain.Tenant `json:"tenant"`
		IsSuperAdmin  bool          `json:"is_super_admin"`
		Impersonating bool          `json:"impersonating"`
	}
	_ = json.Unmarshal(recDash.Body.Bytes(), &dash)
	if dash.Tenant.ID != surID || !dash.IsSuperAdmin || !dash.Impersonating {
		t.Fatalf("dashboard impersonado inesperado: %+v", dash)
	}

	// Salir de impersonación
	_ = doWithCookie(env.mux, http.MethodPost, "/api/v1/superadmin/impersonate", `{"tenant_id":""}`, adminCookie)

	// Suspender a Librería Sur desde Super-Admin
	bodySuspend, _ := json.Marshal(map[string]any{
		"tenant_id":           surID,
		"business_name":       "Librería Sur",
		"email":               "sur@libreria.com",
		"plan_type":           domain.PlanFreeTrial,
		"subscription_status": "TRIAL",
		"monthly_quota":       50,
		"is_active":           false,
	})
	recSusp := httptest.NewRequest(http.MethodPost, "/api/v1/superadmin/tenants", bytes.NewReader(bodySuspend))
	recSusp.Header.Set("Content-Type", "application/json")
	recSusp.AddCookie(adminCookie)
	rrSusp := httptest.NewRecorder()
	env.mux.ServeHTTP(rrSusp, recSusp)
	if rrSusp.Code != http.StatusOK {
		t.Fatalf("suspender comercio: %d %s", rrSusp.Code, rrSusp.Body.String())
	}

	// Un comercio suspendido no puede iniciar sesión -> /login?error=suspended
	recSuspendedLogin := env.login("sur@libreria.com", "password123", "10.2.2.2:1234")
	if recSuspendedLogin.Header().Get("Location") != "/login?error=suspended" {
		t.Fatalf("comercio suspendido debía redirigir a /login?error=suspended, obtuvo %q", recSuspendedLogin.Header().Get("Location"))
	}

	// Aprobar manualmente una orden de suscripción desde Super-Admin
	ord := &domain.SubscriptionOrder{
		ID:        uuid.NewString(),
		TenantID:  surID,
		PlanType:  domain.PlanFull,
		AmountARS: 78000,
		PriceUSD:  65,
		Status:    "PENDING",
	}
	if err := env.handler.repo.CreateSubscriptionOrder(context.Background(), ord); err != nil {
		t.Fatal(err)
	}
	recApprove := doWithCookie(env.mux, http.MethodPost, "/api/v1/superadmin/subscriptions/approve", `{"order_id":"`+ord.ID+`"}`, adminCookie)
	if recApprove.Code != http.StatusOK {
		t.Fatalf("aprobar suscripción desde superadmin: %d %s", recApprove.Code, recApprove.Body.String())
	}
	updatedTenant, _ := env.handler.repo.GetTenantByID(context.Background(), surID)
	if updatedTenant.PlanType != domain.PlanFull || updatedTenant.SubscriptionStatus != "ACTIVE" || updatedTenant.MonthlyQuota != 5000 {
		t.Fatalf("tras aprobar la orden debía tener plan FULL ACTIVE (5000), tiene %+v", updatedTenant)
	}
}
