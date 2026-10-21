package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"google.golang.org/genai"
	"veripago-saas/internal/domain"
)

var _ domain.ReceiptExtractor = (*GeminiReceiptExtractor)(nil)

// ErrNoGeminiKey se devuelve fuera del modo demo si no hay GEMINI_API_KEY configurada.
var ErrNoGeminiKey = errors.New("GEMINI_API_KEY no configurada: no se pueden analizar comprobantes")

const defaultGeminiModel = "gemini-3.8-flash"

const geminiTimeout = 45 * time.Second

// simulatedPrefix marca los comprobantes armados por el Simulador. Solo se aceptan en modo demo.
const simulatedPrefix = "SIMULATED_RECEIPT_JSON:"

// allowedMimeTypes son los formatos de comprobante que se envían a Gemini.
var allowedMimeTypes = map[string]bool{
	"image/jpeg":      true,
	"image/png":       true,
	"image/webp":      true,
	"image/heic":      true,
	"image/heif":      true,
	"application/pdf": true,
}

type GeminiReceiptExtractor struct {
	client   *genai.Client
	model    string
	demoMode bool
	mockFunc func(ctx context.Context, fileBytes []byte, mimeType string) (*domain.ExtractedReceiptData, error)
}

// NewGeminiReceiptExtractor crea el extractor. Sin apiKey solo funciona en modo demo
// (con el simulador y un analizador local de prueba).
func NewGeminiReceiptExtractor(ctx context.Context, apiKey, model string, demoMode bool) (*GeminiReceiptExtractor, error) {
	if strings.TrimSpace(model) == "" {
		model = defaultGeminiModel
	}
	if strings.TrimSpace(apiKey) == "" {
		return &GeminiReceiptExtractor{
			client:   nil,
			model:    model,
			demoMode: demoMode,
		}, nil
	}

	client, err := genai.NewClient(ctx, &genai.ClientConfig{
		APIKey:  apiKey,
		Backend: genai.BackendGeminiAPI,
	})
	if err != nil {
		return nil, err
	}
	return &GeminiReceiptExtractor{
		client:   client,
		model:    model,
		demoMode: demoMode,
	}, nil
}

// NewMockGeminiReceiptExtractor permite inyectar respuestas determinísticas en tests unitarios.
func NewMockGeminiReceiptExtractor(fn func(ctx context.Context, fileBytes []byte, mimeType string) (*domain.ExtractedReceiptData, error)) *GeminiReceiptExtractor {
	return &GeminiReceiptExtractor{
		model:    defaultGeminiModel,
		mockFunc: fn,
	}
}

// HasLiveAPIKey indica si el extractor cuenta con un cliente real de Gemini API configurado.
func (g *GeminiReceiptExtractor) HasLiveAPIKey() bool {
	return g != nil && g.client != nil
}

const forensicSystemPrompt = `Eres un perito informático antifraude especializado en comprobantes de transferencia bancaria y billeteras virtuales de Argentina (Mercado Pago, Brubank, Ualá, Naranja X, MODO, Santander, Galicia, BBVA, Banco Nación, Cuenta DNI, Macro, etc.).
Tu tarea es extraer con precisión quirúrgica los datos del comprobante adjunto y detectar intentos de estafa.

REGLAS CRÍTICAS DE EXTRACCIÓN Y DETECCIÓN DE FRAUDE:
1. is_completed_transfer: Ponlo en FALSE si la imagen es una pantalla previa de confirmación (ej: "¿Confirmás la transferencia?", botón "Transferir" abajo), o si dice "Transferencia programada", "En proceso", "Pendiente" o "Rechazada". Solo es TRUE si indica claramente que el envío ya se realizó.
2. monto (amount): Extrae el número decimal exacto. En Argentina el punto (.) suele separar miles y la coma (,) o superíndice los centavos (ej: "$ 15.450,50" -> 15450.50).
3. operation_number: Si es un comprobante de Mercado Pago, extrae el "Número de operación" (suele tener 10 a 12 dígitos numéricos). Si es de otro banco, extrae el número de comprobante u operación principal.
4. coelsa_id: Es el identificador interbancario de la transferencia. Según el banco figura como "ID Operación" (Brubank), "Código de identificación", "ID COELSA" o "ID de transacción". Suele tener 22 caracteres alfanuméricos en mayúsculas (ej: "1LMP68NKK4DW4WEPNR7OEV") o 19 dígitos. Extráelo completo, sin espacios ni saltos de línea aunque esté partido en dos renglones. NO lo confundas con el "Número de transacción" interno del banco (ej: 10 dígitos en Brubank). Un código con guiones tipo UUID (ej: "74caca65-b680-bdb0-fe3c-9fe207dd3ff0", habitual en Ualá) NUNCA es COELSA: en ese caso deja coelsa_id vacío y pon ese código en operation_number.
5. sender_cuit: Extrae el CUIT/CUIL de quien envía el dinero solo con dígitos (sin guiones).
6. receiver_alias_or_cvu: Extrae el Alias, CBU o CVU del destinatario para verificar que le hayan transferido al comercio correcto.
7. visual_manipulation_notes: Si notas tipografías de distinto tamaño o fuente en el monto respecto del resto del texto, recortes sospechosos o bordes desalineados típicos de edición o apps falsas ("MP Fake"), descríbelo en este array.`

func (g *GeminiReceiptExtractor) ExtractFromMedia(ctx context.Context, fileBytes []byte, mimeType string) (*domain.ExtractedReceiptData, error) {
	if g.mockFunc != nil {
		return g.mockFunc(ctx, fileBytes, mimeType)
	}

	// Payloads del Simulador Forense: SOLO en modo demo. En producción un archivo con este
	// prefijo es un intento de saltear el análisis y se rechaza.
	if bytes.HasPrefix(fileBytes, []byte(simulatedPrefix)) {
		if !g.demoMode {
			return &domain.ExtractedReceiptData{IsValidReceipt: false}, nil
		}
		rawJSON := bytes.TrimPrefix(fileBytes, []byte(simulatedPrefix))
		// Si trae un sufijo de nonce para diferenciar hashes, cortamos hasta el cierre del objeto JSON
		if idx := bytes.LastIndexByte(rawJSON, '}'); idx != -1 {
			rawJSON = rawJSON[:idx+1]
		}
		var simulated domain.ExtractedReceiptData
		if err := json.Unmarshal(rawJSON, &simulated); err == nil {
			return &simulated, nil
		}
	}

	// Sin GEMINI_API_KEY: analizador local de prueba solo en modo demo
	if g.client == nil {
		if g.demoMode {
			return fallbackExtractFromBytes(fileBytes, mimeType), nil
		}
		return nil, ErrNoGeminiKey
	}

	// Solo imágenes y PDF: cualquier otro tipo de archivo no es un comprobante
	mimeType = normalizeMime(mimeType)
	if !allowedMimeTypes[mimeType] {
		return &domain.ExtractedReceiptData{IsValidReceipt: false}, nil
	}

	schema := &genai.Schema{
		Type: genai.TypeObject,
		Properties: map[string]*genai.Schema{
			"is_valid_receipt":      {Type: genai.TypeBoolean},
			"is_completed_transfer": {Type: genai.TypeBoolean},
			"amount":                {Type: genai.TypeNumber},
			"currency":              {Type: genai.TypeString},
			"operation_number":      {Type: genai.TypeString},
			"coelsa_id":             {Type: genai.TypeString},
			"source_bank_or_wallet": {Type: genai.TypeString},
			"sender_name":           {Type: genai.TypeString},
			"sender_cuit":           {Type: genai.TypeString},
			"receiver_name":         {Type: genai.TypeString},
			"receiver_alias_or_cvu": {Type: genai.TypeString},
			"receipt_timestamp_iso": {Type: genai.TypeString},
			"visual_manipulation_notes": {
				Type:  genai.TypeArray,
				Items: &genai.Schema{Type: genai.TypeString},
			},
		},
		Required: []string{
			"is_valid_receipt", "is_completed_transfer", "amount",
			"operation_number", "source_bank_or_wallet",
			"receiver_alias_or_cvu",
		},
	}

	temp := float32(0.0)
	config := &genai.GenerateContentConfig{
		SystemInstruction: &genai.Content{
			Parts: []*genai.Part{{Text: forensicSystemPrompt}},
		},
		Temperature:      &temp,
		ResponseMIMEType: "application/json",
		ResponseSchema:   schema,
	}

	contents := []*genai.Content{
		{
			Role: "user",
			Parts: []*genai.Part{
				{InlineData: &genai.Blob{Data: fileBytes, MIMEType: mimeType}},
				{Text: "Analiza este comprobante y devuelve el JSON estructurado."},
			},
		},
	}

	// Sin límite, una demora de Google deja colgado el pedido del panel o del webhook
	ctx, cancel := context.WithTimeout(ctx, geminiTimeout)
	defer cancel()
	resp, err := g.generateWithRetry(ctx, contents, config)
	if err != nil {
		return nil, fmt.Errorf("error llamando a Gemini API: %w", err)
	}

	var extracted domain.ExtractedReceiptData
	if err := json.Unmarshal([]byte(resp.Text()), &extracted); err != nil {
		return nil, fmt.Errorf("error parseando JSON de Gemini: %w", err)
	}

	return &extracted, nil
}

// geminiBackoff son las esperas entre intentos cuando Google responde "saturado" (429/5xx).
var geminiBackoff = []time.Duration{1500 * time.Millisecond, 4 * time.Second}

// geminiFallbackModel se usa en el último intento si el modelo principal sigue saturado.
const geminiFallbackModel = "gemini-flash-latest"

func (g *GeminiReceiptExtractor) generateWithRetry(ctx context.Context, contents []*genai.Content, config *genai.GenerateContentConfig) (*genai.GenerateContentResponse, error) {
	var lastErr error
	for attempt := 0; attempt <= len(geminiBackoff); attempt++ {
		model := g.model
		if attempt == len(geminiBackoff) && g.model != geminiFallbackModel {
			model = geminiFallbackModel
		}
		resp, err := g.client.Models.GenerateContent(ctx, model, contents, config)
		if err == nil {
			return resp, nil
		}
		lastErr = err
		if !isRetryableGeminiError(err) || attempt == len(geminiBackoff) {
			break
		}
		select {
		case <-ctx.Done():
			return nil, lastErr
		case <-time.After(geminiBackoff[attempt]):
		}
	}
	return nil, lastErr
}

// isRetryableGeminiError indica si conviene reintentar: límite de uso o servicio saturado.
func isRetryableGeminiError(err error) bool {
	code := 0
	var apiErr genai.APIError
	var apiErrPtr *genai.APIError
	switch {
	case errors.As(err, &apiErr):
		code = apiErr.Code
	case errors.As(err, &apiErrPtr):
		code = apiErrPtr.Code
	default:
		return false
	}
	return code == 429 || code == 500 || code == 502 || code == 503 || code == 504
}

func normalizeMime(m string) string {
	if i := strings.IndexByte(m, ';'); i >= 0 {
		m = m[:i]
	}
	m = strings.ToLower(strings.TrimSpace(m))
	if m == "image/jpg" {
		m = "image/jpeg"
	}
	return m
}

// fallbackExtractFromBytes permite probar la subida de archivos en entorno local cuando no se configuró GEMINI_API_KEY.
func fallbackExtractFromBytes(fileBytes []byte, mimeType string) *domain.ExtractedReceiptData {
	var parsed domain.ExtractedReceiptData
	if err := json.Unmarshal(fileBytes, &parsed); err == nil && (parsed.Amount > 0 || parsed.OperationNumber != "") {
		return &parsed
	}

	if len(fileBytes) < 16 {
		return &domain.ExtractedReceiptData{
			IsValidReceipt:      false,
			IsCompletedTransfer: false,
		}
	}

	return &domain.ExtractedReceiptData{
		IsValidReceipt:          true,
		IsCompletedTransfer:     true,
		Amount:                  15450.50,
		Currency:                "ARS",
		OperationNumber:         "88492019482",
		CoelsaID:                "2410200000149281746391",
		SourceBankOrWallet:      "Mercado Pago (Modo Demo Local)",
		SenderName:              "Juan Ignacio Perez",
		SenderCUIT:              "20348912349",
		ReceiverName:            "Pizzeria Don Mateo SRL",
		ReceiverAliasOrCVU:      "don.mateo.mp",
		ReceiptTimestampISO:     time.Now().Format(time.RFC3339),
		VisualManipulationNotes: []string{},
	}
}
