package whatsapp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"veripago-saas/internal/domain"
)

// OutboundMessage registra un mensaje enviado por el Bot de WhatsApp (para auditoría y simulador en tiempo real)
type OutboundMessage struct {
	ID        string    `json:"id"`
	ToPhone   string    `json:"to_phone"`
	Direction string    `json:"direction"` // "INBOUND" | "OUTBOUND"
	Sender    string    `json:"sender"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

var _ domain.WhatsAppGateway = (*Client)(nil)

// Client gestiona la comunicación con Meta WhatsApp Cloud API (envío de mensajes y descarga de comprobantes).
type Client struct {
	accessToken   string
	phoneNumberID string
	httpClient    *http.Client

	mu      sync.Mutex
	history []OutboundMessage
}

func NewClient(accessToken, phoneNumberID string) *Client {
	return &Client{
		accessToken:   strings.TrimSpace(accessToken),
		phoneNumberID: strings.TrimSpace(phoneNumberID),
		httpClient:    &http.Client{Timeout: 10 * time.Second},
		history:       make([]OutboundMessage, 0, 100),
	}
}

// RecordInboundMessage guarda el mensaje entrante en el historial de conversación para visualización en el Dashboard/Simulador.
func (c *Client) RecordInboundMessage(fromPhone, senderName, body string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	msg := OutboundMessage{
		ID:        uuid.NewString(),
		ToPhone:   fromPhone,
		Direction: "INBOUND",
		Sender:    senderName,
		Body:      body,
		CreatedAt: time.Now(),
	}
	c.history = append(c.history, msg)
	if len(c.history) > 100 {
		c.history = c.history[len(c.history)-100:]
	}
}

// SendTextMessage envía una respuesta o notificación proactiva al número de WhatsApp del empleado o dueño.
func (c *Client) SendTextMessage(ctx context.Context, toPhone string, body string) error {
	c.mu.Lock()
	msg := OutboundMessage{
		ID:        uuid.NewString(),
		ToPhone:   toPhone,
		Direction: "OUTBOUND",
		Sender:    "VeriPago Bot",
		Body:      body,
		CreatedAt: time.Now(),
	}
	c.history = append(c.history, msg)
	if len(c.history) > 100 {
		c.history = c.history[len(c.history)-100:]
	}
	c.mu.Unlock()

	// Si no hay credenciales de Meta WhatsApp Cloud API configuradas, queda registrado en el historial local
	if c.accessToken == "" || c.phoneNumberID == "" {
		return nil
	}

	endpoint := fmt.Sprintf("https://graph.facebook.com/v20.0/%s/messages", c.phoneNumberID)
	payload := map[string]interface{}{
		"messaging_product": "whatsapp",
		"recipient_type":    "individual",
		"to":                toPhone,
		"type":              "text",
		"text": map[string]interface{}{
			"preview_url": false,
			"body":        body,
		},
	}

	rawBytes, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBytes))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("WhatsApp Cloud API devolvió status %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}

// DownloadMedia descarga los bytes de una imagen o PDF desde WhatsApp Cloud API a partir de su media_id.
func (c *Client) DownloadMedia(ctx context.Context, mediaID string) ([]byte, string, error) {
	if c.accessToken == "" {
		return nil, "", fmt.Errorf("WA_ACCESS_TOKEN no configurado para descargar media %s", mediaID)
	}

	metaEndpoint := fmt.Sprintf("https://graph.facebook.com/v20.0/%s", mediaID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, metaEndpoint, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.accessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	var mediaMeta struct {
		URL      string `json:"url"`
		MimeType string `json:"mime_type"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&mediaMeta); err != nil {
		return nil, "", err
	}
	if mediaMeta.URL == "" {
		return nil, "", fmt.Errorf("WhatsApp Media API no devolvió URL para media %s", mediaID)
	}

	fileReq, err := http.NewRequestWithContext(ctx, http.MethodGet, mediaMeta.URL, nil)
	if err != nil {
		return nil, "", err
	}
	fileReq.Header.Set("Authorization", "Bearer "+c.accessToken)

	fileResp, err := c.httpClient.Do(fileReq)
	if err != nil {
		return nil, "", err
	}
	defer fileResp.Body.Close()

	fileBytes, err := io.ReadAll(fileResp.Body)
	if err != nil {
		return nil, "", err
	}
	return fileBytes, mediaMeta.MimeType, nil
}

// ListMessages devuelve el historial reciente de mensajes de WhatsApp para el Simulador/Dashboard.
func (c *Client) ListMessages() []OutboundMessage {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]OutboundMessage, len(c.history))
	copy(out, c.history)
	return out
}
