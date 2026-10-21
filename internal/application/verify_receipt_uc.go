package application

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"veripago-saas/internal/domain"
)

// numericRegex reconoce un "Número de operación" de Mercado Pago (10 a 13 dígitos).
var numericRegex = regexp.MustCompile(`^\d{10,13}$`)

// Destination es la cuenta del comercio a la que debe haberse transferido.
type Destination struct {
	Alias string
	CVU   string
}

type VerifyReceiptUseCase struct {
	repo      domain.PaymentLedger
	extractor domain.ReceiptExtractor
	mpClient  domain.MercadoPagoGateway
}

func NewVerifyReceiptUseCase(
	repo domain.PaymentLedger,
	extractor domain.ReceiptExtractor,
	mpClient domain.MercadoPagoGateway,
) *VerifyReceiptUseCase {
	return &VerifyReceiptUseCase{
		repo:      repo,
		extractor: extractor,
		mpClient:  mpClient,
	}
}

func (uc *VerifyReceiptUseCase) Execute(
	ctx context.Context,
	tenantID string,
	mpAccessToken string,
	dest Destination,
	timeWindowMinutes int,
	fileBytes []byte,
	mimeType string,
) (*domain.VerificationOutcome, error) {
	start := time.Now()
	hash := sha256.Sum256(fileBytes)
	out := &domain.VerificationOutcome{
		ValidationID: uuid.NewString(),
		FileSHA256:   hex.EncodeToString(hash[:]),
	}
	finish := func(status domain.ValidationStatus, msg string) (*domain.VerificationOutcome, error) {
		out.Status = status
		out.MessageForUser = msg
		out.ProcessingTime = time.Since(start)
		out.ProcessingMs = out.ProcessingTime.Milliseconds()
		return out, nil
	}

	// 1. ESCUDO 1: el mismo archivo ya se usó para validar una venta (solo cuentan las VERIFICADAS,
	// así reenviar la misma foto tras un "no impactó" no dispara una falsa alarma)
	isDup, prevTime, err := uc.repo.CheckFileDuplicate(ctx, tenantID, out.FileSHA256)
	if err != nil {
		return nil, err
	}
	if isDup {
		return finish(domain.StatusFraudDuplicateFile, fmt.Sprintf(
			"🚨 *ALERTA DE FRAUDE (ARCHIVO REPETIDO)*\n\nEsta misma imagen/PDF ya se usó para validar una venta el *%s*.\n❌ *NO ENTREGAR EL PEDIDO.*",
			prevTime.Format("02/01/2006 a las 15:04 hs"),
		))
	}

	// 2. ESCUDO 2: Extracción forense con Gemini
	extracted, err := uc.extractor.ExtractFromMedia(ctx, fileBytes, mimeType)
	if err != nil {
		return nil, err
	}
	out.ExtractedData = extracted

	if !extracted.IsValidReceipt {
		return finish(domain.StatusInvalidReceiptImage,
			"⚠️ *Imagen no válida*\nNo pudimos detectar un comprobante de transferencia legible en la foto enviada.")
	}
	// 2.B: mismo comprobante ya usado, aunque la imagen sea distinta (WhatsApp recomprime las
	// fotos, así que el hash del archivo cambia). Se compara número de operación/COELSA + monto.
	if prev, found, err := uc.findVerifiedSameReceipt(ctx, tenantID, extracted); err != nil {
		return nil, err
	} else if found {
		return finish(domain.StatusFraudDuplicateFile, fmt.Sprintf(
			"🚨 *COMPROBANTE REPETIDO*\n\nEste comprobante (operación *%s* por *$%.2f*) ya se usó para validar una venta el *%s*.\n❌ *NO ENTREGAR EL PEDIDO.*",
			receiptIDFor(extracted), extracted.Amount, prev.Format("02/01/2006 a las 15:04 hs"),
		))
	}

	if !extracted.IsCompletedTransfer {
		return finish(domain.StatusFraudIncomplete,
			"🚨 *CUIDADO: TRANSFERENCIA NO FINALIZADA*\n\nLa captura muestra una transferencia *programada, en proceso o en la pantalla previa a confirmar*.\n❌ *El dinero NO fue enviado.*")
	}

	// 3. ESCUDO 3: el destinatario del comprobante debe ser el comercio (alias o CVU)
	if checked, ok := MatchDestination(dest, extracted.ReceiverAliasOrCVU); checked && !ok {
		return finish(domain.StatusRejectedWrongDest, fmt.Sprintf(
			"❌ *DESTINATARIO INCORRECTO*\n\nEl comprobante indica que se transfirió a *%s*, que no coincide con tu cuenta configurada (*%s*).",
			extracted.ReceiverAliasOrCVU, describeDestination(dest),
		))
	}

	// 4. ESCUDO 4: Conciliación contra Mercado Pago + bloqueo transaccional
	synced := false
	syncRecentPayments := func() {
		if synced {
			return
		}
		synced = true
		recent, err := uc.mpClient.FetchRecentApprovedPayments(ctx, mpAccessToken, tenantID)
		if err != nil {
			log.Printf("⚠️ No se pudieron sincronizar pagos recientes de Mercado Pago: %v", err)
			return
		}
		for _, p := range recent {
			if err := uc.repo.UpsertMPPayment(ctx, p); err != nil {
				log.Printf("⚠️ No se pudo guardar el pago MP #%d: %v", p.MPPaymentID, err)
			}
		}
	}

	coelsa := NormalizeCoelsa(extracted.CoelsaID)
	opNumber := strings.TrimSpace(extracted.OperationNumber)
	opIsNumeric := numericRegex.MatchString(opNumber)
	fromMP := isMercadoPagoSource(extracted.SourceBankOrWallet)

	// Un comprobante con ID "fuerte" (COELSA, o número de operación de un comprobante de MP) solo se
	// puede validar con ESE pago. Si no aparece, NO se busca otro pago por monto: así un comprobante
	// falso con un ID inventado no se valida con el pago de otro cliente.
	// Solo cuenta como COELSA si tiene ese formato: varios bancos (ej. Ualá) muestran un ID interno
	// tipo UUID que Mercado Pago nunca conoce; ese comprobante se concilia por monto + CUIT + horario.
	coelsaIsReal := IsCoelsaFormat(coelsa)
	strongID := coelsaIsReal || (opIsNumeric && fromMP)

	// 4.A: ID COELSA (transferencias desde otros bancos; MP lo informa como e2e_id)
	if coelsa != "" {
		syncRecentPayments()
		p, err := uc.repo.ClaimPaymentByCoelsaID(ctx, tenantID, coelsa, extracted.Amount, out.ValidationID)
		if done, err := uc.resolveClaim(out, finish, p, err, "ID COELSA *"+coelsa+"*", extracted); done || err != nil {
			return out, err
		}
		if coelsaIsReal {
			out.MatchCoelsaID = coelsa
		}
	}

	// 4.B: Número de operación de Mercado Pago
	if opIsNumeric {
		mpID, _ := strconv.ParseInt(opNumber, 10, 64)
		remote, err := uc.mpClient.GetPaymentByID(ctx, mpAccessToken, tenantID, opNumber)
		if err != nil {
			log.Printf("⚠️ No se pudo consultar el pago MP #%s: %v", opNumber, err)
		} else if remote != nil {
			if err := uc.repo.UpsertMPPayment(ctx, remote); err != nil {
				log.Printf("⚠️ No se pudo guardar el pago MP #%s: %v", opNumber, err)
			}
		}
		p, err := uc.repo.ClaimPaymentByExactID(ctx, tenantID, mpID, extracted.Amount, out.ValidationID)
		if done, err := uc.resolveClaim(out, finish, p, err, fmt.Sprintf("operación *#%d*", mpID), extracted); done || err != nil {
			return out, err
		}
		if fromMP {
			out.MatchMPPaymentID = mpID
		}
	}

	if strongID {
		idDesc := opNumber
		if coelsa != "" {
			idDesc = "COELSA " + coelsa
		}
		return finish(domain.StatusRejectedNotFound, fmt.Sprintf(
			"❌ *NO IMPACTÓ EN MERCADO PAGO*\n\nLa operación del comprobante (*%s*) por *$%.2f* no figura todavía en tu cuenta de Mercado Pago.\n\n• *Origen declarado:* %s\n⏳ _Si el cliente acaba de transferir, te avisaremos automáticamente si impacta en los próximos 3 minutos._",
			idDesc, extracted.Amount, extracted.SourceBankOrWallet,
		))
	}

	// 4.C: Comprobante SIN ID verificable: monto exacto + ventana de tiempo (+ CUIT si ambos lo informan)
	syncRecentPayments()
	out.MatchByAmount = true
	p, err := uc.repo.ClaimPaymentByHeuristic(ctx, tenantID, extracted.Amount, timeWindowMinutes, extracted.SenderCUIT, out.ValidationID)
	if err == nil && p != nil {
		out.MatchedPayment = p
		return finish(domain.StatusVerifiedHeuristic, fmt.Sprintf(
			"✅ *TRANSFERENCIA BANCARIA ACREDITADA*\n\n💰 *Monto:* $%.2f\n🏦 *Banco origen (Comprobante):* %s\n👤 *Emisor en comprobante:* %s\n🔢 *ID interno MP:* #%d\n🕒 *Impactó a las:* %s\n🔒 *Estado:* Pago consumido y asignado.",
			p.Amount, extracted.SourceBankOrWallet, extracted.SenderName, p.MPPaymentID, p.PaymentDate.Format("15:04:05 hs"),
		))
	}
	if err != nil && !errors.Is(err, domain.ErrPaymentNotFound) {
		return nil, err
	}

	return finish(domain.StatusRejectedNotFound, fmt.Sprintf(
		"❌ *NO IMPACTÓ EN MERCADO PAGO*\n\nNo se encontró ningún ingreso disponible por *$%.2f* en tu cuenta de Mercado Pago.\n\n• *Origen declarado:* %s\n• *Operación:* %s\n⏳ _Si el cliente acaba de transferir desde otro banco, te avisaremos automáticamente si impacta en los próximos 3 minutos._",
		extracted.Amount, extracted.SourceBankOrWallet, extracted.OperationNumber,
	))
}

// resolveClaim traduce el resultado de un intento de conciliación por ID.
// done=true significa que el outcome quedó resuelto (verificado, doble uso o monto distinto).
func (uc *VerifyReceiptUseCase) resolveClaim(
	out *domain.VerificationOutcome,
	finish func(domain.ValidationStatus, string) (*domain.VerificationOutcome, error),
	p *domain.MPPayment,
	err error,
	idDesc string,
	extracted *domain.ExtractedReceiptData,
) (bool, error) {
	switch {
	case err == nil && p != nil:
		out.MatchedPayment = p
		_, _ = finish(domain.StatusVerifiedExactID, fmt.Sprintf(
			"✅ *TRANSFERENCIA VERIFICADA (100%% ID exacto)*\n\n💰 *Monto acreditado:* $%.2f\n🔑 *Comprobante:* %s\n🔢 *ID Mercado Pago:* #%d\n🏦 *Origen:* %s\n🕒 *Hora acreditación:* %s\n🔒 *Estado:* Bloqueado y asignado a esta venta.",
			p.Amount, idDesc, p.MPPaymentID, extracted.SourceBankOrWallet, p.PaymentDate.Format("15:04:05 hs"),
		))
		return true, nil

	case errors.Is(err, domain.ErrPaymentAlreadyConsumed):
		byCharge := p != nil && strings.HasPrefix(p.ConsumedByReceiptID, domain.ChargeOrderIDPrefix)
		out.ConsumedByChargeOrder = byCharge
		_, _ = finish(domain.StatusFraudAlreadyUsed, AlreadyUsedMessage(idDesc, extracted.Amount, byCharge))
		return true, nil

	case errors.Is(err, domain.ErrPaymentAmountMismatch):
		realAmount := 0.0
		if p != nil {
			realAmount = p.Amount
		}
		_, _ = finish(domain.StatusRejectedAmountDiff, fmt.Sprintf(
			"❌ *EL MONTO NO COINCIDE*\n\nEl comprobante dice *$%.2f*, pero la %s acreditó *$%.2f* en Mercado Pago.\n❌ *NO ENTREGAR* hasta revisar el pago.",
			extracted.Amount, idDesc, realAmount,
		))
		return true, nil

	case errors.Is(err, domain.ErrPaymentNotFound):
		return false, nil

	default:
		return false, err
	}
}

// AlreadyUsedMessage arma el aviso de un pago real que ya fue asignado a otra venta.
func AlreadyUsedMessage(idDesc string, amount float64, consumedByChargeOrder bool) string {
	if consumedByChargeOrder {
		return fmt.Sprintf(
			"ℹ️ *PAGO YA CONCILIADO CON UNA ORDEN /cobrar*\n\nLa %s por *$%.2f* es real y ya se asignó automáticamente a una orden de cobro.\n• Si es *el mismo pedido*, ya está cobrado.\n• Si es *otro pedido*, ❌ *NO ENTREGAR*.",
			idDesc, amount,
		)
	}
	return fmt.Sprintf(
		"🚨 *INTENTO DE DOBLE USO DETECTADO*\n\nLa %s por *$%.2f* es real, pero *YA FUE UTILIZADA* para validar otro pedido anteriormente.\n❌ *NO ENTREGAR.*",
		idDesc, amount,
	)
}

// ExecuteAndAudit ejecuta la verificación, guarda la auditoría en receipt_validations y suma la cuota del comercio.
func (uc *VerifyReceiptUseCase) ExecuteAndAudit(
	ctx context.Context,
	tenantID string,
	senderPhone string,
	waMessageID string,
	mpAccessToken string,
	dest Destination,
	timeWindowMinutes int,
	fileBytes []byte,
	mimeType string,
) (*domain.VerificationOutcome, error) {
	outcome, err := uc.Execute(ctx, tenantID, mpAccessToken, dest, timeWindowMinutes, fileBytes, mimeType)
	if err != nil {
		return nil, err
	}

	valRecord := &domain.ReceiptValidation{
		ID:               outcome.ValidationID,
		TenantID:         tenantID,
		SenderPhone:      senderPhone,
		WAMessageID:      waMessageID,
		FileSHA256:       outcome.FileSHA256,
		ValidationResult: outcome.Status,
		ProcessingTimeMs: int(outcome.ProcessingMs),
		CreatedAt:        time.Now(),
	}

	if d := outcome.ExtractedData; d != nil {
		valRecord.ExtractedAmount = d.Amount
		valRecord.ExtractedOpNumber = d.OperationNumber
		valRecord.ExtractedCoelsaID = d.CoelsaID
		valRecord.ExtractedSenderName = d.SenderName
		valRecord.ExtractedSenderCUIT = d.SenderCUIT
		valRecord.ExtractedReceiverAliasOrCBU = d.ReceiverAliasOrCVU
		valRecord.ExtractedSourceBank = d.SourceBankOrWallet
		valRecord.AIFraudWarnings = d.VisualManipulationNotes
		if d.ReceiptTimestampISO != "" {
			if t, err := time.Parse(time.RFC3339, d.ReceiptTimestampISO); err == nil {
				valRecord.ExtractedDate = &t
			}
		}
	}

	if outcome.MatchedPayment != nil {
		valRecord.MatchedMPPaymentID = outcome.MatchedPayment.ID
	}

	// El archivo duplicado no se audita ni consume cuota (se corta antes de gastar IA)
	if outcome.Status != domain.StatusFraudDuplicateFile {
		if err := uc.repo.SaveReceiptValidation(ctx, valRecord); err != nil {
			log.Printf("⚠️ No se pudo guardar la auditoría %s: %v", valRecord.ID, err)
		}
		if err := uc.repo.IncrementTenantQuota(ctx, tenantID); err != nil {
			log.Printf("⚠️ No se pudo actualizar la cuota del comercio %s: %v", tenantID, err)
		}
	}

	return outcome, nil
}

// NormalizeCoelsa quita espacios/saltos de línea y pasa a mayúsculas un ID COELSA.
func NormalizeCoelsa(s string) string {
	return strings.ToUpper(strings.Join(strings.Fields(s), ""))
}

// minReceiptIDLen evita comparar números de operación demasiado cortos (podrían repetirse).
const minReceiptIDLen = 6

// receiptIDFor devuelve el identificador del comprobante que se usa para detectar repetidos.
func receiptIDFor(e *domain.ExtractedReceiptData) string {
	if strings.TrimSpace(e.OperationNumber) != "" {
		return strings.TrimSpace(e.OperationNumber)
	}
	return strings.TrimSpace(e.CoelsaID)
}

// findVerifiedSameReceipt busca una venta ya verificada con el mismo número de operación o
// ID COELSA y el mismo monto.
func (uc *VerifyReceiptUseCase) findVerifiedSameReceipt(ctx context.Context, tenantID string, e *domain.ExtractedReceiptData) (time.Time, bool, error) {
	if e.Amount <= 0 {
		return time.Time{}, false, nil
	}
	seen := map[string]bool{}
	for _, raw := range []string{e.OperationNumber, e.CoelsaID} {
		id := normalizeReceiptID(raw)
		if len(id) < minReceiptIDLen || seen[id] {
			continue
		}
		seen[id] = true
		found, at, err := uc.repo.FindVerifiedByReceiptID(ctx, tenantID, id, e.Amount)
		if err != nil || found {
			return at, found, err
		}
	}
	return time.Time{}, false, nil
}

// normalizeReceiptID deja solo letras y números en mayúsculas.
func normalizeReceiptID(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// coelsaRegex: el ID interbancario tiene 22 caracteres alfanuméricos en mayúsculas
// (ej. 1LMP68NKK4DW4WEPNR7OEV) o entre 19 y 22 dígitos.
var coelsaRegex = regexp.MustCompile(`^(?:[A-Z0-9]{22}|[0-9]{19,22})$`)

// IsCoelsaFormat indica si un ID ya normalizado tiene formato de ID COELSA.
// Un UUID (ej. 74caca65-b680-bdb0-fe3c-9fe207dd3ff0) no lo es.
func IsCoelsaFormat(normalized string) bool {
	return coelsaRegex.MatchString(normalized)
}

func isMercadoPagoSource(source string) bool {
	s := strings.ToLower(strings.ReplaceAll(source, " ", ""))
	return strings.Contains(s, "mercadopago")
}

func describeDestination(d Destination) string {
	switch {
	case d.Alias != "" && d.CVU != "":
		return d.Alias + " / " + d.CVU
	case d.Alias != "":
		return d.Alias
	default:
		return d.CVU
	}
}

// ---------------------------------------------------------------------------
// Escudo 3: comparación del destinatario
// ---------------------------------------------------------------------------

func isMaskRune(r rune) bool {
	return r == '*' || r == 'x' || r == '•' || r == '#'
}

func digitsOnly(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// isCVUToken reconoce un CBU/CVU completo o enmascarado (ej: "0000003100****4821").
func isCVUToken(tok string) bool {
	digits, n := 0, 0
	for _, r := range tok {
		n++
		switch {
		case r >= '0' && r <= '9':
			digits++
		case isMaskRune(r) || r == '-':
		default:
			return false
		}
	}
	return n >= 8 && digits >= 4
}

func cvuMatches(tok, cvu string) bool {
	runes := []rune(strings.ReplaceAll(tok, "-", ""))
	masked := false
	for _, r := range runes {
		if isMaskRune(r) {
			masked = true
			break
		}
	}
	if !masked {
		return string(runes) == cvu
	}
	// Mismo largo: comparación posición por posición con comodines
	if len(runes) == len(cvu) {
		for i, r := range runes {
			if !isMaskRune(r) && byte(r) != cvu[i] {
				return false
			}
		}
		return true
	}
	// Largo distinto: comparamos los dígitos visibles del principio y del final
	var prefix, suffix []rune
	i := 0
	for i < len(runes) && !isMaskRune(runes[i]) {
		prefix = append(prefix, runes[i])
		i++
	}
	j := len(runes) - 1
	for j >= 0 && !isMaskRune(runes[j]) {
		suffix = append([]rune{runes[j]}, suffix...)
		j--
	}
	if len(prefix)+len(suffix) < 4 {
		return false
	}
	return strings.HasPrefix(cvu, string(prefix)) && strings.HasSuffix(cvu, string(suffix))
}

var destinationLabels = map[string]bool{"alias": true, "cbu": true, "cvu": true, "destino": true, "cuenta": true}

// MatchDestination compara el alias/CBU/CVU del comprobante con la cuenta del comercio.
// checked=false significa que no hay datos suficientes para comparar (no se rechaza).
func MatchDestination(dest Destination, receiver string) (checked bool, ok bool) {
	alias := strings.ToLower(strings.TrimSpace(dest.Alias))
	cvu := digitsOnly(dest.CVU)
	raw := strings.ToLower(strings.TrimSpace(receiver))
	if raw == "" || (alias == "" && cvu == "") {
		return false, true
	}

	tokens := strings.FieldsFunc(raw, func(r rune) bool {
		return unicode.IsSpace(r) || strings.ContainsRune(",;/|:()[]", r)
	})
	singleToken := len(tokens) == 1

	for _, tok := range tokens {
		tok = strings.Trim(tok, ".")
		if tok == "" || destinationLabels[tok] {
			continue
		}
		if isCVUToken(tok) {
			if cvu == "" {
				continue // no tenemos CVU configurado para comparar
			}
			checked = true
			if cvuMatches(tok, cvu) {
				return true, true
			}
			continue
		}
		if alias == "" {
			continue
		}
		// Un alias bancario tiene 6 a 20 caracteres (aceptamos hasta 40 para detectar alias ajenos) (letras, números, puntos y guiones). Para no confundir
		// el nombre del titular con un alias, exigimos punto/guion salvo que sea el único dato.
		if isAliasLike(tok) && (singleToken || strings.ContainsAny(tok, ".-")) {
			checked = true
			if tok == alias {
				return true, true
			}
		}
	}
	return checked, !checked
}

func isAliasLike(tok string) bool {
	if len(tok) < 6 || len(tok) > 40 {
		return false
	}
	hasLetter := false
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z':
			hasLetter = true
		case r >= '0' && r <= '9', r == '.', r == '-':
		default:
			return false
		}
	}
	return hasLetter
}
