package mp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"veripago-saas/internal/domain"
)

var _ domain.MercadoPagoGateway = (*MPClient)(nil)

// ErrDemoToken se devuelve si, fuera del modo demo, se intenta usar un token de demostración.
var ErrDemoToken = fmt.Errorf("la cuenta de Mercado Pago tiene un token de demostración: volvé a vincularla por OAuth")

type MPClient struct {
	httpClient *http.Client

	// Pagos simulados: solo se usan con demoMode=true (nunca en producción)
	demoMode     bool
	mockMu       sync.RWMutex
	mockPayments map[string]*domain.MPPayment

	// Cache access_token -> user_id del titular, para filtrar solo dinero ENTRANTE
	ownerMu  sync.Mutex
	ownerIDs map[string]int64
}

func NewMPClient(demoMode bool) *MPClient {
	return &MPClient{
		demoMode:     demoMode,
		httpClient:   &http.Client{Timeout: 8 * time.Second},
		mockPayments: make(map[string]*domain.MPPayment),
		ownerIDs:     make(map[string]int64),
	}
}

type mpPaymentResponse struct {
	ID             int64   `json:"id"`
	Status         string  `json:"status"`
	OperationType  string  `json:"operation_type"`
	TransactionAmt float64 `json:"transaction_amount"`
	CurrencyID     string  `json:"currency_id"`
	DateCreated    string  `json:"date_created"`
	DateApproved   string  `json:"date_approved"`
	// GET /v1/payments/{id} devuelve collector_id; /v1/payments/search devuelve collector.id
	CollectorID int64 `json:"collector_id"`
	Collector   struct {
		ID int64 `json:"id"`
	} `json:"collector"`
	PointOfInteraction struct {
		TransactionData struct {
			E2EID string `json:"e2e_id"`
		} `json:"transaction_data"`
	} `json:"point_of_interaction"`
	Payer struct {
		FirstName      string `json:"first_name"`
		LastName       string `json:"last_name"`
		Identification struct {
			Type   string `json:"type"`
			Number string `json:"number"`
		} `json:"identification"`
	} `json:"payer"`
}

// OAuthTokenResponse representa la respuesta de OAuth 2.0 de Mercado Pago (/oauth/token)
type OAuthTokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int64  `json:"expires_in"`
	Scope        string `json:"scope"`
	UserID       int64  `json:"user_id"`
	RefreshToken string `json:"refresh_token"`
	PublicKey    string `json:"public_key"`
}

// IsDemoMode indica si el cliente acepta pagos simulados y tokens de demostración.
func (c *MPClient) IsDemoMode() bool {
	return c.demoMode
}

// RegisterMockPayment registra un pago simulado. Solo tiene efecto en modo demo.
func (c *MPClient) RegisterMockPayment(p *domain.MPPayment) {
	if !c.demoMode {
		return
	}
	copyP := *p
	c.mockMu.Lock()
	c.mockPayments[strconv.FormatInt(p.MPPaymentID, 10)] = &copyP
	c.mockMu.Unlock()
}

// checkToken resuelve si hay que golpear la API real. Devuelve skip=true si no corresponde consultar.
func (c *MPClient) checkToken(accessToken string) (skip bool, err error) {
	if accessToken == "" {
		return true, nil
	}
	if strings.HasPrefix(accessToken, "DEMO_") {
		if c.demoMode {
			return true, nil
		}
		return true, ErrDemoToken
	}
	return false, nil
}

// GetPaymentByID busca directamente un número de operación en Mercado Pago
func (c *MPClient) GetPaymentByID(ctx context.Context, accessToken string, tenantID string, paymentID string) (*domain.MPPayment, error) {
	if c.demoMode {
		c.mockMu.RLock()
		p, ok := c.mockPayments[paymentID]
		c.mockMu.RUnlock()
		if ok {
			copyP := *p
			copyP.TenantID = tenantID
			return &copyP, nil
		}
	}
	if skip, err := c.checkToken(accessToken); skip {
		return nil, err
	}

	endpoint := fmt.Sprintf("https://api.mercadopago.com/v1/payments/%s", url.PathEscape(paymentID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return nil, nil // No existe ese pago en la cuenta del comercio
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MP API devolvió status %d", resp.StatusCode)
	}

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	ownerID, err := c.getOwnerUserID(ctx, accessToken)
	if err != nil {
		return nil, err
	}
	collectorID, err := extractCollectorID(bodyBytes)
	if err != nil {
		return nil, err
	}
	if collectorID != ownerID {
		return nil, nil // Pago SALIENTE (lo hizo el titular): nunca sirve para validar un comprobante
	}

	return mapRawMPToDomain(tenantID, bodyBytes)
}

// FetchRecentApprovedPayments trae las últimas transferencias acreditadas para alimentar la tabla local
func (c *MPClient) FetchRecentApprovedPayments(ctx context.Context, accessToken string, tenantID string) ([]*domain.MPPayment, error) {
	if skip, err := c.checkToken(accessToken); skip {
		if err != nil || !c.demoMode {
			return nil, err
		}
		var list []*domain.MPPayment
		c.mockMu.RLock()
		for _, p := range c.mockPayments {
			if p.TenantID == tenantID || p.TenantID == "" {
				copyP := *p
				copyP.TenantID = tenantID
				list = append(list, &copyP)
			}
		}
		c.mockMu.RUnlock()
		return list, nil
	}

	ownerID, err := c.getOwnerUserID(ctx, accessToken)
	if err != nil {
		return nil, err
	}

	u, _ := url.Parse("https://api.mercadopago.com/v1/payments/search")
	q := u.Query()
	q.Set("collector.id", strconv.FormatInt(ownerID, 10))
	q.Set("status", "approved")
	q.Set("sort", "date_created")
	q.Set("criteria", "desc")
	q.Set("limit", "30")
	q.Set("range", "date_created")
	q.Set("begin_date", "NOW-12HOURS")
	q.Set("end_date", "NOW")
	u.RawQuery = q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("MP Search API devolvió status %d", resp.StatusCode)
	}

	var searchResult struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&searchResult); err != nil {
		return nil, err
	}

	var payments []*domain.MPPayment
	for _, raw := range searchResult.Results {
		if collectorID, err := extractCollectorID(raw); err != nil || collectorID != ownerID {
			continue // Descartamos pagos salientes aunque el filtro collector.id no se aplique
		}
		p, err := mapRawMPToDomain(tenantID, raw)
		if err == nil && p != nil {
			payments = append(payments, p)
		}
	}
	return payments, nil
}

// GetOAuthAuthorizationURL construye la URL oficial de vinculación OAuth 2.0 en 1 clic con Mercado Pago Argentina
func (c *MPClient) GetOAuthAuthorizationURL(clientID, redirectURI, stateTenantID string) string {
	u, _ := url.Parse("https://auth.mercadopago.com.ar/authorization")
	q := u.Query()
	q.Set("client_id", clientID)
	q.Set("response_type", "code")
	q.Set("platform_id", "mp")
	q.Set("state", stateTenantID)
	q.Set("redirect_uri", redirectURI)
	u.RawQuery = q.Encode()
	return u.String()
}

// ExchangeOAuthCode intercambia el authorization code por access_token y refresh_token de Mercado Pago
func (c *MPClient) ExchangeOAuthCode(ctx context.Context, clientID, clientSecret, code, redirectURI string) (*OAuthTokenResponse, error) {
	if c.demoMode && (strings.HasPrefix(code, "DEMO_CODE") || clientID == "MP_DEMO_CLIENT_ID") {
		return &OAuthTokenResponse{
			AccessToken:  "DEMO_APP_USR_ACCESS_TOKEN_" + fmt.Sprintf("%d", time.Now().Unix()),
			TokenType:    "bearer",
			ExpiresIn:    21600,
			Scope:        "read write offline_access",
			UserID:       1492837465,
			RefreshToken: "DEMO_TG_REFRESH_TOKEN_" + fmt.Sprintf("%d", time.Now().Unix()),
		}, nil
	}

	payload := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"grant_type":    "authorization_code",
		"code":          code,
		"redirect_uri":  redirectURI,
	}
	return c.postOAuthToken(ctx, payload)
}

// RefreshAccessToken renueva automáticamente el token OAuth 2.0 de Mercado Pago antes de que expire
func (c *MPClient) RefreshAccessToken(ctx context.Context, clientID, clientSecret, refreshToken string) (*OAuthTokenResponse, error) {
	if c.demoMode && (strings.HasPrefix(refreshToken, "DEMO_") || clientID == "MP_DEMO_CLIENT_ID") {
		return &OAuthTokenResponse{
			AccessToken:  "DEMO_APP_USR_REFRESHED_" + fmt.Sprintf("%d", time.Now().Unix()),
			TokenType:    "bearer",
			ExpiresIn:    21600,
			Scope:        "read write offline_access",
			UserID:       1492837465,
			RefreshToken: refreshToken,
		}, nil
	}

	payload := map[string]string{
		"client_id":     clientID,
		"client_secret": clientSecret,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	}
	return c.postOAuthToken(ctx, payload)
}

func (c *MPClient) postOAuthToken(ctx context.Context, payload map[string]string) (*OAuthTokenResponse, error) {
	bodyBytes, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.mercadopago.com/oauth/token", bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("MP OAuth devolvió status %d: %s", resp.StatusCode, string(b))
	}

	var tokenResp OAuthTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return nil, err
	}
	return &tokenResp, nil
}

// getOwnerUserID obtiene (y cachea) el user_id del titular del access_token vía /users/me
func (c *MPClient) getOwnerUserID(ctx context.Context, accessToken string) (int64, error) {
	c.ownerMu.Lock()
	id, ok := c.ownerIDs[accessToken]
	c.ownerMu.Unlock()
	if ok {
		return id, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.mercadopago.com/users/me", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("MP /users/me devolvió status %d", resp.StatusCode)
	}

	var me struct {
		ID int64 `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&me); err != nil {
		return 0, err
	}
	if me.ID == 0 {
		return 0, fmt.Errorf("MP /users/me no devolvió id")
	}

	c.ownerMu.Lock()
	c.ownerIDs[accessToken] = me.ID
	c.ownerMu.Unlock()
	return me.ID, nil
}

func extractCollectorID(rawJSON []byte) (int64, error) {
	var parsed mpPaymentResponse
	if err := json.Unmarshal(rawJSON, &parsed); err != nil {
		return 0, err
	}
	if parsed.CollectorID != 0 {
		return parsed.CollectorID, nil
	}
	return parsed.Collector.ID, nil
}

func mapRawMPToDomain(tenantID string, rawJSON []byte) (*domain.MPPayment, error) {
	var parsed mpPaymentResponse
	if err := json.Unmarshal(rawJSON, &parsed); err != nil {
		return nil, err
	}

	payDate, err := time.Parse(time.RFC3339, parsed.DateApproved)
	if err != nil {
		payDate, _ = time.Parse(time.RFC3339, parsed.DateCreated)
	}
	if payDate.IsZero() {
		payDate = time.Now()
	}

	return &domain.MPPayment{
		TenantID:             tenantID,
		MPPaymentID:          parsed.ID,
		Amount:               parsed.TransactionAmt,
		Currency:             parsed.CurrencyID,
		MPStatus:             parsed.Status,
		OperationType:        parsed.OperationType,
		PayerCUIT:            parsed.Payer.Identification.Number,
		PayerName:            strings.TrimSpace(fmt.Sprintf("%s %s", parsed.Payer.FirstName, parsed.Payer.LastName)),
		CoelsaID:             parsed.PointOfInteraction.TransactionData.E2EID,
		PaymentDate:          payDate,
		ReconciliationStatus: "AVAILABLE",
		RawJSON:              rawJSON,
	}, nil
}
