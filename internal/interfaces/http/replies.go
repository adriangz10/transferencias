package http

import (
	"math"
	"strconv"
	"strings"
	"time"

	"veripago-saas/internal/application"
	"veripago-saas/internal/domain"
)

// Respuestas por WhatsApp sobre un comprobante: siempre uno de estos 4 resultados.
// El motivo detallado (comprobante repetido, otra cuenta, etc.) queda en el panel.
const (
	replyCorrect    = "✅ Transferencia *correcta*"
	replyPending    = "⏳ Transferencia *pendiente*"
	replyFake       = "🚫 Transferencia *falsa*"
	replyNotArrived = "❌ La transferencia *no ingresó*"
	// Mismo comprobante reenviado (por ejemplo, primero el PDF y después una captura).
	replyAlreadySent = "🔁 Comprobante *ya enviado*"

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

// verifiedReply arma el mensaje de transferencia verificada agregando, si están disponibles,
// el nombre del cliente, el banco/billetera de origen y la hora a la que llegó el pago.
func verifiedReply(amount float64, clientName, bank string, arrivedAt time.Time) string {
	var b strings.Builder
	b.WriteString(transferReply(replyCorrect, amount))
	if name := strings.TrimSpace(clientName); name != "" {
		b.WriteString("\n👤 Cliente: ")
		b.WriteString(name)
	}
	if src := strings.TrimSpace(bank); src != "" {
		b.WriteString("\n🏦 Banco: ")
		b.WriteString(src)
	}
	if !arrivedAt.IsZero() {
		b.WriteString("\n🕒 Hora: ")
		b.WriteString(arrivedAt.Local().Format("15:04 hs"))
	}
	return b.String()
}

// alreadySentReply avisa que el comprobante ya se envió y validó antes (mismo archivo, o el
// mismo comprobante en otro formato: PDF y captura). No es un pago nuevo.
func alreadySentReply(amount float64, at time.Time, bySenderOnly bool) string {
	msg := transferReply(replyAlreadySent, amount) +
		"\nEste comprobante ya se envió y se validó el " + at.Local().Format("02/01 a las 15:04 hs") + ". No es un pago nuevo."
	if bySenderOnly {
		msg += "\nSi es otra transferencia por el mismo monto, reenvialo cuando se acredite."
	}
	return msg
}

// pendingFrom agrega de quién era el comprobante que estaba en espera, para que el
// empleado sepa a qué venta corresponde el aviso.
func pendingFrom(item *domain.PendingReceiptQueueItem) string {
	if name := strings.TrimSpace(item.SenderName); name != "" {
		return "\nComprobante de " + name + "."
	}
	return ""
}

// replyForOutcome traduce el resultado de la verificación a uno de los mensajes y le suma las
// alertas del cruce con el historial del cliente (fraudes previos, CUIT con otro nombre, etc.).
func replyForOutcome(o *domain.VerificationOutcome) string {
	msg := baseReply(o)
	if lines := application.RiskWarnings(o.CustomerRisk); len(lines) > 0 {
		msg += "\n" + strings.Join(lines, "\n")
	}
	return msg
}

func baseReply(o *domain.VerificationOutcome) string {
	amount := 0.0
	if o.ExtractedData != nil {
		amount = o.ExtractedData.Amount
	}
	switch o.Status {
	case domain.StatusVerifiedExactID, domain.StatusVerifiedHeuristic:
		var clientName, bank string
		var arrivedAt time.Time
		if o.ExtractedData != nil {
			clientName = o.ExtractedData.SenderName
			bank = o.ExtractedData.SourceBankOrWallet
			if t, ok := domain.ParseReceiptTimestamp(o.ExtractedData.ReceiptTimestampISO); ok {
				arrivedAt = t
			}
		}
		if o.MatchedPayment != nil {
			if o.MatchedPayment.Amount > 0 {
				amount = o.MatchedPayment.Amount
			}
			if strings.TrimSpace(clientName) == "" {
				clientName = o.MatchedPayment.PayerName
			}
			if !o.MatchedPayment.PaymentDate.IsZero() {
				arrivedAt = o.MatchedPayment.PaymentDate
			}
		}
		return verifiedReply(amount, clientName, bank, arrivedAt)
	case domain.StatusRejectedNotFound:
		if o.QueuedForRetry {
			return transferReply(replyPending, amount) + "\nTe aviso apenas entre."
		}
		return transferReply(replyNotArrived, amount)
	case domain.StatusInvalidReceiptImage:
		// No se pudo leer un comprobante: no hay forma de confirmar que el dinero entró
		return transferReply(replyNotArrived, amount)
	case domain.StatusFraudDuplicateFile, domain.StatusFraudAlreadyUsed:
		if o.AlreadySentAt != nil {
			return alreadySentReply(amount, *o.AlreadySentAt, o.AlreadySentBySender)
		}
		return transferReply(replyFake, amount) + "\nEste comprobante ya se usó."
	case domain.StatusFraudIncomplete,
		domain.StatusFraudTampered,
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
