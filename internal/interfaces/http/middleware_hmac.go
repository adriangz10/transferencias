package http

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
)

// buildMPManifest arma el texto firmado por Mercado Pago. Según la documentación oficial,
// si data.id no viene en la URL esa parte se omite, y si es alfanumérico va en minúsculas.
func buildMPManifest(dataID, requestID, ts string) string {
	var b strings.Builder
	if dataID != "" {
		b.WriteString("id:")
		b.WriteString(strings.ToLower(dataID))
		b.WriteString(";")
	}
	if requestID != "" {
		b.WriteString("request-id:")
		b.WriteString(requestID)
		b.WriteString(";")
	}
	b.WriteString("ts:")
	b.WriteString(ts)
	b.WriteString(";")
	return b.String()
}

// ValidateMPWebhookSignature verifica la cabecera x-signature oficial de Mercado Pago.
func ValidateMPWebhookSignature(webhookSecret string, r *http.Request) bool {
	if webhookSecret == "" {
		return false
	}
	xSignature := r.Header.Get("x-signature")
	xRequestID := r.Header.Get("x-request-id")
	dataID := r.URL.Query().Get("data.id")

	if xSignature == "" {
		return false
	}

	var ts, v1Hash string
	for _, part := range strings.Split(xSignature, ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		switch strings.TrimSpace(kv[0]) {
		case "ts":
			ts = strings.TrimSpace(kv[1])
		case "v1":
			v1Hash = strings.TrimSpace(kv[1])
		}
	}
	if ts == "" || v1Hash == "" {
		return false
	}

	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(buildMPManifest(dataID, xRequestID, ts)))
	expectedHash := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expectedHash), []byte(strings.ToLower(v1Hash)))
}

// ComputeMPWebhookSignature genera la cabecera x-signature válida para pruebas de Webhooks de Mercado Pago.
func ComputeMPWebhookSignature(webhookSecret, dataID, requestID, ts string) string {
	mac := hmac.New(sha256.New, []byte(webhookSecret))
	mac.Write([]byte(buildMPManifest(dataID, requestID, ts)))
	return "ts=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// ValidateWhatsAppSignature verifica la firma X-Hub-Signature-256 de Meta WhatsApp Cloud API.
// Sin app secret configurado NO se acepta nada.
func ValidateWhatsAppSignature(appSecret string, signatureHeader string, rawBody []byte) bool {
	if appSecret == "" || !strings.HasPrefix(signatureHeader, "sha256=") {
		return false
	}
	providedHex := strings.ToLower(strings.TrimPrefix(signatureHeader, "sha256="))

	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(rawBody)
	expectedHex := hex.EncodeToString(mac.Sum(nil))

	return hmac.Equal([]byte(expectedHex), []byte(providedHex))
}

// ComputeWhatsAppSignature genera la cabecera X-Hub-Signature-256 (para tests).
func ComputeWhatsAppSignature(appSecret string, rawBody []byte) string {
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
