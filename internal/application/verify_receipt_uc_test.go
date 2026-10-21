package application

import (
	"context"
	"sync"
	"testing"
	"time"

	"veripago-saas/internal/domain"
	"veripago-saas/internal/infrastructure/ai"
	"veripago-saas/internal/infrastructure/mp"
	"veripago-saas/internal/infrastructure/persistence"
)

func fixedExtractor(d domain.ExtractedReceiptData) *ai.GeminiReceiptExtractor {
	return ai.NewMockGeminiReceiptExtractor(func(ctx context.Context, fileBytes []byte, mimeType string) (*domain.ExtractedReceiptData, error) {
		copyD := d
		return &copyD, nil
	})
}

func seedPayment(t *testing.T, repo *persistence.MySQLRepository, p *domain.MPPayment) {
	t.Helper()
	if p.Currency == "" {
		p.Currency = "ARS"
	}
	if p.MPStatus == "" {
		p.MPStatus = "approved"
	}
	if p.ReconciliationStatus == "" {
		p.ReconciliationStatus = "AVAILABLE"
	}
	if p.PaymentDate.IsZero() {
		p.PaymentDate = time.Now()
	}
	if err := repo.UpsertMPPayment(context.Background(), p); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestVerifyReceiptUseCase_AllShieldsAndConcurrency(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	mpClient := mp.NewMPClient(true)
	tenantID := "tenant-test-01"
	dest := Destination{Alias: "comercio.test.mp"}

	exactPay := &domain.MPPayment{ID: "uuid-pay-1", TenantID: tenantID, MPPaymentID: 12345678901, Amount: 15000.00, OperationType: "money_transfer", PayerName: "Juan Perez"}
	seedPayment(t, repo, exactPay)
	mpClient.RegisterMockPayment(exactPay)

	// 1. ID exacto + doble gasto concurrente (10 goroutines al mismo tiempo)
	ucExact := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 15000.00, Currency: "ARS",
		OperationNumber: "12345678901", SourceBankOrWallet: "Mercado Pago", SenderName: "Juan Perez",
		ReceiverAliasOrCVU: dest.Alias,
	}), mpClient)

	const workers = 10
	var wg sync.WaitGroup
	var mu sync.Mutex
	verifiedCount, alreadyUsedCount := 0, 0
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			fileBytes := []byte{byte(idx), 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
			out, err := ucExact.Execute(ctx, tenantID, "DEMO_TOKEN", dest, 120, fileBytes, "image/jpeg")
			if err != nil {
				t.Errorf("error inesperado: %v", err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			switch out.Status {
			case domain.StatusVerifiedExactID:
				verifiedCount++
			case domain.StatusFraudAlreadyUsed:
				alreadyUsedCount++
			default:
				t.Errorf("estado inesperado en concurrencia: %s", out.Status)
			}
		}(i)
	}
	wg.Wait()
	if verifiedCount != 1 || alreadyUsedCount != workers-1 {
		t.Fatalf("esperaba 1 verificado y %d doble uso, obtuvo %d y %d", workers-1, verifiedCount, alreadyUsedCount)
	}

	// 2. Escudo 2: transferencia incompleta
	outInc, _ := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: false, Amount: 20000, OperationNumber: "999", SourceBankOrWallet: "Brubank", ReceiverAliasOrCVU: dest.Alias,
	}), mpClient).Execute(ctx, tenantID, "DEMO_TOKEN", dest, 120, []byte("foto-en-proceso-unica"), "image/jpeg")
	if outInc.Status != domain.StatusFraudIncomplete {
		t.Fatalf("esperaba FRAUD_INCOMPLETE_TRANSFER, obtuvo %s", outInc.Status)
	}

	// 3. Escudo 3: destinatario incorrecto
	outWrong, _ := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 20000, OperationNumber: "999", SourceBankOrWallet: "Ualá", ReceiverAliasOrCVU: "alias.de.otro.comercio",
	}), mpClient).Execute(ctx, tenantID, "DEMO_TOKEN", dest, 120, []byte("foto-alias-incorrecto"), "image/jpeg")
	if outWrong.Status != domain.StatusRejectedWrongDest {
		t.Fatalf("esperaba REJECTED_WRONG_DESTINATION, obtuvo %s", outWrong.Status)
	}

	// 4. Comprobante sin ID verificable: monto + ventana de tiempo
	seedPayment(t, repo, &domain.MPPayment{ID: "uuid-pay-cvu", TenantID: tenantID, MPPaymentID: 55566677788, Amount: 10014.00, OperationType: "bank_transfer", PayerName: "Maria Gomez", PaymentDate: time.Now().Add(-3 * time.Minute)})
	outHeur, _ := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 10014.00, OperationNumber: "BNA-CVU-00192", SourceBankOrWallet: "Banco Nación", SenderName: "Maria Gomez", ReceiverAliasOrCVU: dest.Alias,
	}), mpClient).Execute(ctx, tenantID, "DEMO_TOKEN", dest, 120, []byte("foto-cvu-10014"), "image/jpeg")
	if outHeur.Status != domain.StatusVerifiedHeuristic {
		t.Fatalf("esperaba VERIFIED_HEURISTIC, obtuvo %s", outHeur.Status)
	}
}

// Un archivo solo cuenta como "repetido" si antes se usó para VERIFICAR una venta.
func TestDuplicateFile_OnlyAfterVerified(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	mpClient := mp.NewMPClient(true)
	tenantID := "tenant-dup"
	dest := Destination{Alias: "comercio.test.mp"}
	uc := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 5000, OperationNumber: "11122233344", SourceBankOrWallet: "Mercado Pago", ReceiverAliasOrCVU: dest.Alias,
	}), mpClient)
	file := []byte("misma-foto-del-cliente")

	// 1er envío: el pago todavía no impactó
	out1, err := uc.ExecuteAndAudit(ctx, tenantID, "549111", "wa_1", "DEMO_TOKEN", dest, 120, file, "image/jpeg")
	if err != nil || out1.Status != domain.StatusRejectedNotFound {
		t.Fatalf("esperaba REJECTED_NOT_FOUND, obtuvo %v / %v", out1, err)
	}
	if out1.MatchMPPaymentID != 11122233344 || out1.MatchByAmount {
		t.Fatalf("la cola debía esperar el ID exacto, obtuvo %+v", out1)
	}

	// Impacta el pago y el empleado reenvía LA MISMA foto: no es fraude, se verifica
	seedPayment(t, repo, &domain.MPPayment{TenantID: tenantID, MPPaymentID: 11122233344, Amount: 5000})
	out2, _ := uc.ExecuteAndAudit(ctx, tenantID, "549111", "wa_2", "DEMO_TOKEN", dest, 120, file, "image/jpeg")
	if out2.Status != domain.StatusVerifiedExactID {
		t.Fatalf("el reenvío de la misma foto debía verificarse, obtuvo %s", out2.Status)
	}

	// 3er envío de la misma foto, ya verificada: ahora sí es archivo repetido
	out3, _ := uc.ExecuteAndAudit(ctx, tenantID, "549111", "wa_3", "DEMO_TOKEN", dest, 120, file, "image/jpeg")
	if out3.Status != domain.StatusFraudDuplicateFile {
		t.Fatalf("esperaba FRAUD_DUPLICATE_FILE, obtuvo %s", out3.Status)
	}
}

// Un comprobante de MP con un número de operación inventado NO se valida con el pago de otro cliente.
func TestFakeMPID_DoesNotFallBackToAmount(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	mpClient := mp.NewMPClient(true)
	tenantID := "tenant-fake-id"
	dest := Destination{Alias: "comercio.test.mp"}

	// Pago real de OTRO cliente por el mismo monto
	seedPayment(t, repo, &domain.MPPayment{TenantID: tenantID, MPPaymentID: 90000000001, Amount: 12000})

	out, err := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 12000, OperationNumber: "99999999999", SourceBankOrWallet: "Mercado Pago", ReceiverAliasOrCVU: dest.Alias,
	}), mpClient).Execute(ctx, tenantID, "DEMO_TOKEN", dest, 120, []byte("comprobante-falso"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != domain.StatusRejectedNotFound {
		t.Fatalf("un ID inventado no debía validarse por monto, obtuvo %s", out.Status)
	}

	// Un COELSA inventado tampoco
	out2, _ := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 12000, CoelsaID: "AAAAAAAAAAAAAAAAAAAAAA", SourceBankOrWallet: "Brubank", ReceiverAliasOrCVU: dest.Alias,
	}), mpClient).Execute(ctx, tenantID, "DEMO_TOKEN", dest, 120, []byte("comprobante-falso-2"), "image/jpeg")
	if out2.Status != domain.StatusRejectedNotFound || out2.MatchCoelsaID == "" {
		t.Fatalf("un COELSA inventado no debía validarse por monto, obtuvo %+v", out2)
	}
}

// Si el ID existe pero por otro monto, se rechaza explícitamente.
func TestAmountMismatch_IsRejected(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	tenantID := "tenant-mismatch"
	seedPayment(t, repo, &domain.MPPayment{TenantID: tenantID, MPPaymentID: 80000000001, Amount: 1500})

	out, _ := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 15000, OperationNumber: "80000000001", SourceBankOrWallet: "Mercado Pago",
	}), mp.NewMPClient(true)).Execute(ctx, tenantID, "DEMO_TOKEN", Destination{}, 120, []byte("monto-editado"), "image/jpeg")
	if out.Status != domain.StatusRejectedAmountDiff {
		t.Fatalf("esperaba REJECTED_AMOUNT_MISMATCH, obtuvo %s", out.Status)
	}
}

// Si el comprobante y MP informan CUIT, deben coincidir para conciliar por monto.
func TestHeuristic_RequiresMatchingCUIT(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	tenantID := "tenant-cuit"
	seedPayment(t, repo, &domain.MPPayment{TenantID: tenantID, MPPaymentID: 70000000001, Amount: 3333, PayerCUIT: "20111111112"})

	mk := func(cuit, file string) domain.ValidationStatus {
		out, _ := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
			IsValidReceipt: true, IsCompletedTransfer: true, Amount: 3333, SourceBankOrWallet: "Banco X", SenderCUIT: cuit,
		}), mp.NewMPClient(true)).Execute(ctx, tenantID, "DEMO_TOKEN", Destination{}, 120, []byte(file), "image/jpeg")
		return out.Status
	}
	if s := mk("20-99999999-9", "otro-cuit"); s != domain.StatusRejectedNotFound {
		t.Fatalf("con CUIT distinto no debía conciliar, obtuvo %s", s)
	}
	if s := mk("20-11111111-2", "mismo-cuit"); s != domain.StatusVerifiedHeuristic {
		t.Fatalf("con el mismo CUIT debía conciliar, obtuvo %s", s)
	}
}

// Un pago tomado por una orden /cobrar no se informa como fraude a secas.
func TestAlreadyUsedByChargeOrder(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	tenantID := "tenant-cobrar"
	seedPayment(t, repo, &domain.MPPayment{TenantID: tenantID, MPPaymentID: 60000000001, Amount: 10014})
	if _, err := repo.ClaimPaymentByExactID(ctx, tenantID, 60000000001, 10014, domain.ChargeOrderIDPrefix+"0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	out, _ := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 10014, OperationNumber: "60000000001", SourceBankOrWallet: "Mercado Pago",
	}), mp.NewMPClient(true)).Execute(ctx, tenantID, "DEMO_TOKEN", Destination{}, 120, []byte("comprobante-cobrar"), "image/jpeg")
	if out.Status != domain.StatusFraudAlreadyUsed || !out.ConsumedByChargeOrder {
		t.Fatalf("esperaba doble uso marcado como orden /cobrar, obtuvo %+v", out)
	}
}

func TestVerifyReceiptUseCase_CoelsaExactMatch(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	mpClient := mp.NewMPClient(true)
	tenantID := "tenant-test-coelsa"

	// Pago real desde Brubank: MP informa el ID COELSA como e2e_id
	coelsaPay := &domain.MPPayment{ID: "uuid-pay-coelsa", TenantID: tenantID, MPPaymentID: 185275791908, Amount: 1896.00, OperationType: "account_fund", CoelsaID: "1LMP68NKK4DW4WEPNR7OEV", PaymentDate: time.Now().Add(-2 * time.Minute)}
	// Señuelo: mismo monto, más reciente y sin COELSA. La heurística por monto elegiría este.
	decoyPay := &domain.MPPayment{ID: "uuid-pay-decoy", TenantID: tenantID, MPPaymentID: 185275799999, Amount: 1896.00, OperationType: "money_transfer"}
	seedPayment(t, repo, coelsaPay)
	seedPayment(t, repo, decoyPay)

	uc := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 1896.00, Currency: "ARS",
		OperationNumber:    "2027683208",              // "Número de transacción" interno de Brubank
		CoelsaID:           "1lmp68nkk4dw4 WEPNR7OEV", // partido en dos renglones y en minúsculas
		SourceBankOrWallet: "Brubank",
	}), mpClient)

	out, err := uc.Execute(ctx, tenantID, "DEMO_TOKEN", Destination{}, 120, []byte("comprobante-brubank-1"), "image/png")
	if err != nil {
		t.Fatalf("error inesperado: %v", err)
	}
	if out.Status != domain.StatusVerifiedExactID || out.MatchedPayment == nil || out.MatchedPayment.MPPaymentID != coelsaPay.MPPaymentID {
		t.Fatalf("debía consumir el pago con COELSA, obtuvo %+v", out)
	}

	outReuse, _ := uc.Execute(ctx, tenantID, "DEMO_TOKEN", Destination{}, 120, []byte("comprobante-brubank-editado"), "image/png")
	if outReuse.Status != domain.StatusFraudAlreadyUsed {
		t.Fatalf("esperaba FRAUD_ALREADY_USED al reutilizar el COELSA, obtuvo %s", outReuse.Status)
	}
}

func TestMatchDestination(t *testing.T) {
	dest := Destination{Alias: "don.mateo.mp", CVU: "0000003100084920194821"}
	cases := []struct {
		receiver    string
		wantChecked bool
		wantOK      bool
	}{
		{"don.mateo.mp", true, true},
		{"DON.MATEO.MP", true, true},
		{"Alias: don.mateo.mp", true, true},
		{"0000003100084920194821", true, true}, // banco que muestra solo el CVU
		{"CVU 0000003100****4821", true, true}, // CVU enmascarado
		{"Pizzeria Don Mateo SRL / don.mateo.mp", true, true},
		{"otro.comercio.mp", true, false},
		{"0000003100099999999999", true, false},
		{"mateo", false, true}, // texto parcial: no alcanza para comparar ni para aprobar
		{"", false, true},
	}
	for _, c := range cases {
		checked, ok := MatchDestination(dest, c.receiver)
		if checked != c.wantChecked || ok != c.wantOK {
			t.Errorf("MatchDestination(%q) = (%v,%v), esperaba (%v,%v)", c.receiver, checked, ok, c.wantChecked, c.wantOK)
		}
	}
	// Sin CVU configurado, un CVU en el comprobante no se puede comparar (no se rechaza)
	if checked, ok := MatchDestination(Destination{Alias: "don.mateo.mp"}, "0000003100084920194821"); checked || !ok {
		t.Errorf("sin CVU configurado no debía rechazar")
	}
}

// Caso real de Ualá: el comprobante muestra un ID interno tipo UUID que Mercado Pago no conoce.
// No debe tratarse como COELSA inexistente: se concilia por monto + CUIT dentro de la ventana.
func TestUalaUUIDIsNotCoelsa_FallsBackToAmountAndCUIT(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	tenantID := "tenant-uala"
	seedPayment(t, repo, &domain.MPPayment{TenantID: tenantID, MPPaymentID: 184254817047, Amount: 149, PayerCUIT: "20385452668", CoelsaID: "R7ZG0QNDK4PMJDL09EXYPO", PaymentDate: time.Now().Add(-10 * time.Minute)})

	out, err := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 149, SourceBankOrWallet: "Ualá",
		OperationNumber: "74caca65-b680-bdb0-fe3c-9fe207dd3ff0", CoelsaID: "74caca65-b680-bdb0-fe3c-9fe207dd3ff0",
		SenderCUIT: "20385452668",
	}), mp.NewMPClient(true)).Execute(ctx, tenantID, "DEMO_TOKEN", Destination{}, 120, []byte("uala-149"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != domain.StatusVerifiedHeuristic || out.MatchedPayment == nil || out.MatchedPayment.MPPaymentID != 184254817047 {
		t.Fatalf("esperaba VERIFIED_HEURISTIC con el pago 184254817047, obtuvo %+v", out)
	}
}

func TestIsCoelsaFormat(t *testing.T) {
	for id, want := range map[string]bool{
		"1LMP68NKK4DW4WEPNR7OEV":               true,
		"R7ZG0QNDK4PMJDL09EXYPO":               true,
		"2410200000149281746391":               true,
		"1234567890123456789":                  true,
		"74CACA65-B680-BDB0-FE3C-9FE207DD3FF0": false,
		"74CACA65B680BDB0FE3C9FE207DD3FF0":     false,
		"2027683208":                           false,
	} {
		if got := IsCoelsaFormat(id); got != want {
			t.Errorf("IsCoelsaFormat(%q) = %v, esperaba %v", id, got, want)
		}
	}
}

// El mismo comprobante reenviado como otra imagen (WhatsApp recomprime y cambia el hash)
// se detecta por número de operación + monto y se rechaza al instante, sin quedar pendiente.
func TestSameReceiptDifferentImage_IsDuplicate(t *testing.T) {
	ctx := context.Background()
	repo := persistence.NewMySQLRepository(nil)
	tenantID := "tenant-dup-contenido"
	seedPayment(t, repo, &domain.MPPayment{TenantID: tenantID, MPPaymentID: 185291610592, Amount: 500, CoelsaID: "PDX4OGNY47RYVLQY20L6EY", PaymentDate: time.Now().Add(-3 * time.Minute)})

	uc := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 500, OperationNumber: "95261521", SourceBankOrWallet: "Santander",
	}), mp.NewMPClient(true))

	first, err := uc.ExecuteAndAudit(ctx, tenantID, "5491100000000", "wamid.1", "DEMO_TOKEN", Destination{}, 120, []byte("foto-original"), "image/jpeg")
	if err != nil || first.Status != domain.StatusVerifiedHeuristic {
		t.Fatalf("el primer envío debía verificarse, obtuvo %+v (%v)", first, err)
	}
	second, err := uc.ExecuteAndAudit(ctx, tenantID, "5491100000000", "wamid.2", "DEMO_TOKEN", Destination{}, 120, []byte("foto-recomprimida-por-whatsapp"), "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if second.FileSHA256 == first.FileSHA256 {
		t.Fatal("el test necesita dos archivos distintos")
	}
	if second.Status != domain.StatusFraudDuplicateFile {
		t.Fatalf("el reenvío debía detectarse como repetido, obtuvo %s", second.Status)
	}

	// Otro comprobante distinto por el mismo monto NO es un repetido
	other := NewVerifyReceiptUseCase(repo, fixedExtractor(domain.ExtractedReceiptData{
		IsValidReceipt: true, IsCompletedTransfer: true, Amount: 500, OperationNumber: "11112222", SourceBankOrWallet: "Santander",
	}), mp.NewMPClient(true))
	third, _ := other.Execute(ctx, tenantID, "DEMO_TOKEN", Destination{}, 120, []byte("otro-comprobante"), "image/jpeg")
	if third.Status == domain.StatusFraudDuplicateFile {
		t.Fatal("un comprobante con otro número de operación no es repetido")
	}
}
