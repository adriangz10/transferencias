package http

import (
	"math"
	"strconv"
	"strings"

	"veripago-saas/internal/domain"
)

// Respuestas por WhatsApp sobre un comprobante: siempre uno de estos 4 resultados.
// El motivo detallado (comprobante repetido, otra cuenta, etc.) queda en el panel.
const (
	replyCorrect    = "✅ Transferencia *correcta*"
	replyPending    = "⏳ Transferencia *pendiente*"
	replyFake       = "🚫 Transferencia *falsa*"
	replyNotArrived = "❌ La transferencia *no ingresó*"

	// Cuando el sistema no pudo revisar el comprobante (Gemini caído, sin cuota, MP sin vincular)
	// no podemos afirmar ninguno de los 4 resultados.
	replyCouldNotCheck = "⚠️ No se pudo verificar el comprobante. Reenvialo en unos minutos."
)

// transferReply arma el mensaje: resultado + monto (si se conoce).
func transferReply(base string, amount float64) string {
	if amount > 0 {
		return base + " — " + formatARS(amount)
	}
	return base
}

// pendingFrom agrega de quién era el comprobante que estaba en espera, para que el
// empleado sepa a qué venta corresponde el aviso.
func pendingFrom(item *domain.PendingReceiptQueueItem) string {
	if name := strings.TrimSpace(item.SenderName); name != "" {
		return "\nComprobante de " + name + "."
	}
	return ""
}

// replyForOutcome traduce el resultado de la verificación a uno de los 4 mensajes.
func replyForOutcome(o *domain.VerificationOutcome) string {
	amount := 0.0
	if o.ExtractedData != nil {
		amount = o.ExtractedData.Amount
	}
	switch o.Status {
	case domain.StatusVerifiedExactID, domain.StatusVerifiedHeuristic:
		if o.MatchedPayment != nil && o.MatchedPayment.Amount > 0 {
			amount = o.MatchedPayment.Amount
		}
		return transferReply(replyCorrect, amount)
	case domain.StatusRejectedNotFound:
		if o.QueuedForRetry {
			return transferReply(replyPending, amount) + "\nTe aviso apenas entre."
		}
		return transferReply(replyNotArrived, amount)
	case domain.StatusInvalidReceiptImage:
		// No se pudo leer un comprobante: no hay forma de confirmar que el dinero entró
		return transferReply(replyNotArrived, amount)
	case domain.StatusFraudDuplicateFile, domain.StatusFraudAlreadyUsed:
		return transferReply(replyFake, amount) + "\nEste comprobante ya se usó."
	case domain.StatusFraudIncomplete,
		domain.StatusRejectedWrongDest,
		domain.StatusRejectedAmountDiff:
		return transferReply(replyFake, amount)
	default:
		return replyCouldNotCheck
	}
}

// formatARS formatea un monto al estilo argentino: $ 12.500,50
func formatARS(amount float64) string {
	cents := int64(math.Round(amount * 100))
	neg := cents < 0
	if neg {
		cents = -cents
	}
	intPart := strconv.FormatInt(cents/100, 10)
	var b strings.Builder
	for i, r := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte('.')
		}
		b.WriteRune(r)
	}
	dec := cents % 100
	out := "$ " + b.String() + "," + string('0'+byte(dec/10)) + string('0'+byte(dec%10))
	if neg {
		return "-" + out
	}
	return out
}
