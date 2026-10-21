package queue

import (
	"context"
	"strings"
	"testing"
	"time"

	"veripago-saas/internal/domain"
)

func TestChargeOrders_UniqueAmountsAndSingleMatch(t *testing.T) {
	ctx := context.Background()
	s := NewRedisQueueStore("", "", 0)

	seen := map[int64]bool{}
	for i := 0; i < maxUniqueDelta; i++ {
		o, err := s.CreateUniqueChargeOrder(ctx, "t1", "Caja", "549111", "ref", "alias.test", 10000)
		if err != nil {
			t.Fatalf("orden %d: %v", i, err)
		}
		if !strings.HasPrefix(o.ID, domain.ChargeOrderIDPrefix) || len(o.ID) != 36 {
			t.Fatalf("ID de orden inválido: %q", o.ID)
		}
		c := domain.ToCents(o.UniqueAmount)
		if seen[c] {
			t.Fatalf("monto único repetido: %.2f", o.UniqueAmount)
		}
		seen[c] = true
	}
	// Agotados los 29 montos posibles, no se debe repetir uno
	if _, err := s.CreateUniqueChargeOrder(ctx, "t1", "Caja", "", "ref", "alias.test", 10000); err != ErrNoUniqueAmountAvailable {
		t.Fatalf("esperaba ErrNoUniqueAmountAvailable, obtuvo %v", err)
	}

	// Un pago paga UNA sola orden
	var amount float64
	for c := range seen {
		amount = float64(c) / 100
		break
	}
	if o, _ := s.MatchChargeOrder(ctx, "t1", amount, 1); o == nil || o.Status != "PAID" || o.RequestedPhone != "549111" {
		t.Fatalf("esperaba orden pagada, obtuvo %+v", o)
	}
	if o, _ := s.MatchChargeOrder(ctx, "t1", amount, 2); o != nil {
		t.Fatalf("la misma orden no debía pagarse dos veces")
	}
}

func TestPendingReceipts_RemoveByHash(t *testing.T) {
	ctx := context.Background()
	s := NewRedisQueueStore("", "", 0)
	_ = s.EnqueuePendingReceipt(ctx, &domain.PendingReceiptQueueItem{TenantID: "t1", ValidationID: "a", FileSHA256: "h1", ExpectedAmount: 100})
	_ = s.EnqueuePendingReceipt(ctx, &domain.PendingReceiptQueueItem{TenantID: "t1", ValidationID: "b", FileSHA256: "h2", ExpectedAmount: 100})

	if err := s.RemovePendingByFileHash(ctx, "t1", "h1"); err != nil {
		t.Fatal(err)
	}
	items, _ := s.ListPendingReceipts(ctx, "t1")
	if len(items) != 1 || items[0].ValidationID != "b" {
		t.Fatalf("debía quedar solo el comprobante b, quedaron %+v", items)
	}
}

func TestTakeExpiredPendingReceipts_Memory(t *testing.T) {
	q := NewRedisQueueStore("", "", 0)
	ctx := context.Background()
	_ = q.EnqueuePendingReceipt(ctx, &domain.PendingReceiptQueueItem{ValidationID: "vencido", TenantID: "t1", ExpectedAmount: 100})
	_ = q.EnqueuePendingReceipt(ctx, &domain.PendingReceiptQueueItem{ValidationID: "vigente", TenantID: "t1", ExpectedAmount: 200})

	// Todavía nada venció
	if got, _ := q.TakeExpiredPendingReceipts(ctx); len(got) != 0 {
		t.Fatalf("no debería haber vencidos, hay %d", len(got))
	}

	q.mu.Lock()
	q.memPending["vencido"].ExpiresAt = time.Now().Add(-time.Minute)
	q.mu.Unlock()

	got, _ := q.TakeExpiredPendingReceipts(ctx)
	if len(got) != 1 || got[0].ValidationID != "vencido" {
		t.Fatalf("esperaba solo el vencido, obtuvo %+v", got)
	}
	// Se entrega una sola vez
	if again, _ := q.TakeExpiredPendingReceipts(ctx); len(again) != 0 {
		t.Fatalf("el vencido no debería devolverse dos veces")
	}
	// El vigente sigue en espera
	if list, _ := q.ListPendingReceipts(ctx, "t1"); len(list) != 1 || list[0].ValidationID != "vigente" {
		t.Fatalf("el vigente debería seguir en espera: %+v", list)
	}
}
