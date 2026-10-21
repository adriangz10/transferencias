package queue

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"veripago-saas/internal/domain"
)

const (
	ReactiveQueueTTL = 3 * time.Minute // 3 minutos en espera reactiva (Sección 1.4.3)
	// Después de vencer, el comprobante se conserva un rato más para poder avisar "no ingresó".
	pendingNotifyGrace = 10 * time.Minute
	// Margen tras el vencimiento antes de avisar "no ingresó", para no pisar un pago que se
	// esté asignando justo en ese momento.
	expiredSafetyMargin = 20 * time.Second
	ChargeOrderTTL      = 15 * time.Minute // vigencia de un monto único /cobrar
	ChargeHistoryTTL    = 24 * time.Hour   // cuánto se conservan las órdenes para mostrarlas en el panel

	maxUniqueDelta = 29 // pesos extra posibles (1..29) sobre el monto base
)

var ErrNoUniqueAmountAvailable = errors.New("no hay montos únicos libres para ese importe: esperá que venza o se pague alguna orden")

var _ domain.ReactiveQueue = (*RedisQueueStore)(nil)

// RedisQueueStore gestiona la Cola de Espera Reactiva (3 min) y las órdenes /cobrar.
// Con Redis disponible todo el estado vive en Redis (compartido entre instancias y reinicios);
// sin Redis se usa un almacenamiento en memoria con la misma semántica.
type RedisQueueStore struct {
	client *redis.Client

	mu         sync.Mutex
	memPending map[string]*domain.PendingReceiptQueueItem // validationID -> item
	memOrders  map[string]*domain.UniqueChargeOrder       // orderID -> orden
	memLocks   map[string]memAmountLock                   // tenant:centavos -> orden que reservó el monto
}

type memAmountLock struct {
	orderID   string
	expiresAt time.Time
}

func NewRedisQueueStore(addr, password string, db int) *RedisQueueStore {
	var rdb *redis.Client
	if addr != "" {
		candidate := redis.NewClient(&redis.Options{
			Addr:         addr,
			Password:     password,
			DB:           db,
			DialTimeout:  1500 * time.Millisecond,
			ReadTimeout:  1500 * time.Millisecond,
			WriteTimeout: 1500 * time.Millisecond,
		})
		ctx, cancel := context.WithTimeout(context.Background(), 1200*time.Millisecond)
		defer cancel()
		if err := candidate.Ping(ctx).Err(); err == nil {
			rdb = candidate
		} else {
			_ = candidate.Close()
		}
	}

	return &RedisQueueStore{
		client:     rdb,
		memPending: make(map[string]*domain.PendingReceiptQueueItem),
		memOrders:  make(map[string]*domain.UniqueChargeOrder),
		memLocks:   make(map[string]memAmountLock),
	}
}

func (s *RedisQueueStore) IsUsingRedis() bool {
	return s.client != nil
}

func pendingKey(tenantID, validationID string) string {
	return fmt.Sprintf("veripago:pending:%s:%s", tenantID, validationID)
}

func orderKey(tenantID, orderID string) string {
	return fmt.Sprintf("veripago:charge:%s:%s", tenantID, orderID)
}

func amountLockKey(tenantID string, amount float64) string {
	return fmt.Sprintf("veripago:chargeamt:%s:%d", tenantID, domain.ToCents(amount))
}

// newChargeOrderID genera un ID de 36 caracteres con prefijo "cob-" (entra en consumed_by_receipt_id).
func newChargeOrderID() string {
	u := uuid.New()
	return domain.ChargeOrderIDPrefix + hex.EncodeToString(u[:])
}

// scanKeys recorre claves con SCAN (no bloquea Redis como KEYS).
func (s *RedisQueueStore) scanKeys(ctx context.Context, pattern string) ([]string, error) {
	var keys []string
	iter := s.client.Scan(ctx, 0, pattern, 200).Iterator()
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	return keys, iter.Err()
}

// ---------------------------------------------------------------------------
// Cola de Espera Reactiva
// ---------------------------------------------------------------------------

// EnqueuePendingReceipt guarda un comprobante no encontrado en la Cola de Espera Reactiva durante 3 minutos.
func (s *RedisQueueStore) EnqueuePendingReceipt(ctx context.Context, item *domain.PendingReceiptQueueItem) error {
	now := time.Now()
	if item.ValidationID == "" {
		item.ValidationID = uuid.NewString()
	}
	item.CreatedAt = now
	item.ExpiresAt = now.Add(ReactiveQueueTTL)

	if s.client != nil {
		data, err := json.Marshal(item)
		if err != nil {
			return err
		}
		return s.client.Set(ctx, pendingKey(item.TenantID, item.ValidationID), data, ReactiveQueueTTL+pendingNotifyGrace).Err()
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked()
	copyItem := *item
	s.memPending[item.ValidationID] = &copyItem
	return nil
}

// ListPendingReceipts devuelve los comprobantes en espera del comercio (más recientes primero).
func (s *RedisQueueStore) ListPendingReceipts(ctx context.Context, tenantID string) ([]*domain.PendingReceiptQueueItem, error) {
	now := time.Now()
	var result []*domain.PendingReceiptQueueItem

	if s.client != nil {
		keys, err := s.scanKeys(ctx, fmt.Sprintf("veripago:pending:%s:*", tenantID))
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			raw, err := s.client.Get(ctx, k).Bytes()
			if err != nil {
				continue // expiró entre el SCAN y el GET
			}
			var item domain.PendingReceiptQueueItem
			if json.Unmarshal(raw, &item) == nil && item.ExpiresAt.After(now) {
				result = append(result, &item)
			}
		}
	} else {
		s.mu.Lock()
		s.cleanupExpiredLocked()
		for _, item := range s.memPending {
			if item.TenantID == tenantID && item.ExpiresAt.After(now) {
				copyItem := *item
				result = append(result, &copyItem)
			}
		}
		s.mu.Unlock()
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].CreatedAt.After(result[j].CreatedAt)
	})
	return result, nil
}

// TakeExpiredPendingReceipts retira y devuelve los comprobantes cuya espera ya venció
// (y no se resolvieron), para avisar que la transferencia no ingresó. Cada comprobante se
// devuelve una sola vez, aunque haya varias instancias del servidor (GETDEL es atómico).
func (s *RedisQueueStore) TakeExpiredPendingReceipts(ctx context.Context) ([]*domain.PendingReceiptQueueItem, error) {
	now := time.Now()
	var expired []*domain.PendingReceiptQueueItem

	if s.client != nil {
		keys, err := s.scanKeys(ctx, "veripago:pending:*")
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			raw, err := s.client.Get(ctx, k).Bytes()
			if err != nil {
				continue
			}
			var item domain.PendingReceiptQueueItem
			if json.Unmarshal(raw, &item) != nil || item.ExpiresAt.Add(expiredSafetyMargin).After(now) {
				continue
			}
			taken, err := s.client.GetDel(ctx, k).Bytes()
			if err != nil || len(taken) == 0 {
				continue // otra instancia lo tomó o se resolvió recién
			}
			expired = append(expired, &item)
		}
		return expired, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.memPending {
		if !item.ExpiresAt.Add(expiredSafetyMargin).After(now) {
			copyItem := *item
			expired = append(expired, &copyItem)
			delete(s.memPending, id)
		}
	}
	return expired, nil
}

// RemovePendingReceipt retira un comprobante de la cola (cuando ya se resolvió).
func (s *RedisQueueStore) RemovePendingReceipt(ctx context.Context, tenantID, validationID string) error {
	if s.client != nil {
		return s.client.Del(ctx, pendingKey(tenantID, validationID)).Err()
	}
	s.mu.Lock()
	delete(s.memPending, validationID)
	s.mu.Unlock()
	return nil
}

// RemovePendingByFileHash retira de la cola los comprobantes con el mismo archivo
// (por ejemplo, cuando el empleado reenvió la misma foto y esta vez se verificó).
func (s *RedisQueueStore) RemovePendingByFileHash(ctx context.Context, tenantID, fileSHA256 string) error {
	if fileSHA256 == "" {
		return nil
	}
	items, err := s.ListPendingReceipts(ctx, tenantID)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.FileSHA256 == fileSHA256 {
			if err := s.RemovePendingReceipt(ctx, tenantID, it.ValidationID); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Órdenes /cobrar con monto único (Innovación 1.4.4)
// ---------------------------------------------------------------------------

// CreateUniqueChargeOrder genera un monto único (ej: base $10.000 -> $10.014) reservándolo de forma
// atómica, así dos órdenes vigentes del mismo comercio nunca comparten importe.
func (s *RedisQueueStore) CreateUniqueChargeOrder(ctx context.Context, tenantID, requestedBy, requestedPhone, reference, aliasOrCVU string, baseAmount float64) (*domain.UniqueChargeOrder, error) {
	now := time.Now()
	order := &domain.UniqueChargeOrder{
		ID:             newChargeOrderID(),
		TenantID:       tenantID,
		RequestedBy:    requestedBy,
		RequestedPhone: requestedPhone,
		Reference:      reference,
		BaseAmount:     baseAmount,
		AliasOrCVU:     aliasOrCVU,
		Status:         "PENDING",
		CreatedAt:      now,
		ExpiresAt:      now.Add(ChargeOrderTTL),
	}

	// Probamos los 29 incrementos posibles en orden aleatorio
	deltas := rand.New(rand.NewSource(now.UnixNano())).Perm(maxUniqueDelta)
	baseCents := domain.ToCents(baseAmount)

	if s.client != nil {
		for _, d := range deltas {
			candidate := float64(baseCents+int64(d+1)*100) / 100
			ok, err := s.client.SetNX(ctx, amountLockKey(tenantID, candidate), order.ID, ChargeOrderTTL).Result()
			if err != nil {
				return nil, err
			}
			if ok {
				order.UniqueAmount = candidate
				break
			}
		}
		if order.UniqueAmount == 0 {
			return nil, ErrNoUniqueAmountAvailable
		}
		data, err := json.Marshal(order)
		if err != nil {
			return nil, err
		}
		if err := s.client.Set(ctx, orderKey(tenantID, order.ID), data, ChargeHistoryTTL).Err(); err != nil {
			return nil, err
		}
		copyOrd := *order
		return &copyOrd, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked()
	for _, d := range deltas {
		candidate := float64(baseCents+int64(d+1)*100) / 100
		lk := amountLockKey(tenantID, candidate)
		if _, taken := s.memLocks[lk]; !taken {
			s.memLocks[lk] = memAmountLock{orderID: order.ID, expiresAt: order.ExpiresAt}
			order.UniqueAmount = candidate
			break
		}
	}
	if order.UniqueAmount == 0 {
		return nil, ErrNoUniqueAmountAvailable
	}
	s.memOrders[order.ID] = order
	copyOrd := *order
	return &copyOrd, nil
}

// MatchChargeOrder marca como pagada la orden vigente cuyo monto único coincide exactamente.
// La reserva del monto se toma de forma atómica, así un mismo pago no puede pagar dos órdenes.
func (s *RedisQueueStore) MatchChargeOrder(ctx context.Context, tenantID string, amount float64, mpPaymentID int64) (*domain.UniqueChargeOrder, error) {
	now := time.Now()

	if s.client != nil {
		orderID, err := s.client.GetDel(ctx, amountLockKey(tenantID, amount)).Result()
		if errors.Is(err, redis.Nil) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		raw, err := s.client.Get(ctx, orderKey(tenantID, orderID)).Bytes()
		if err != nil {
			return nil, nil // la orden expiró del historial
		}
		var ord domain.UniqueChargeOrder
		if err := json.Unmarshal(raw, &ord); err != nil {
			return nil, err
		}
		if ord.Status != "PENDING" || now.After(ord.ExpiresAt) {
			return nil, nil
		}
		ord.Status = "PAID"
		ord.MatchedMPID = mpPaymentID
		ord.PaidAt = &now
		data, _ := json.Marshal(&ord)
		if err := s.client.Set(ctx, orderKey(tenantID, ord.ID), data, redis.KeepTTL).Err(); err != nil {
			return nil, err
		}
		return &ord, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.cleanupExpiredLocked()
	lk := amountLockKey(tenantID, amount)
	lock, ok := s.memLocks[lk]
	if !ok {
		return nil, nil
	}
	delete(s.memLocks, lk)
	ord, ok := s.memOrders[lock.orderID]
	if !ok || ord.Status != "PENDING" {
		return nil, nil
	}
	ord.Status = "PAID"
	ord.MatchedMPID = mpPaymentID
	ord.PaidAt = &now
	copyOrd := *ord
	return &copyOrd, nil
}

// ListChargeOrders devuelve las órdenes del comercio (las vencidas aparecen como EXPIRED).
func (s *RedisQueueStore) ListChargeOrders(ctx context.Context, tenantID string) ([]*domain.UniqueChargeOrder, error) {
	now := time.Now()
	var list []*domain.UniqueChargeOrder

	if s.client != nil {
		keys, err := s.scanKeys(ctx, fmt.Sprintf("veripago:charge:%s:*", tenantID))
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			raw, err := s.client.Get(ctx, k).Bytes()
			if err != nil {
				continue
			}
			var ord domain.UniqueChargeOrder
			if json.Unmarshal(raw, &ord) != nil {
				continue
			}
			if ord.Status == "PENDING" && now.After(ord.ExpiresAt) {
				ord.Status = "EXPIRED"
			}
			list = append(list, &ord)
		}
	} else {
		s.mu.Lock()
		s.cleanupExpiredLocked()
		for _, ord := range s.memOrders {
			if ord.TenantID == tenantID {
				copyOrd := *ord
				list = append(list, &copyOrd)
			}
		}
		s.mu.Unlock()
	}

	sort.Slice(list, func(i, j int) bool {
		return list[i].CreatedAt.After(list[j].CreatedAt)
	})
	return list, nil
}

func (s *RedisQueueStore) cleanupExpiredLocked() {
	now := time.Now()
	for id, item := range s.memPending {
		if now.After(item.ExpiresAt.Add(pendingNotifyGrace)) {
			delete(s.memPending, id)
		}
	}
	for k, lock := range s.memLocks {
		if now.After(lock.expiresAt) {
			delete(s.memLocks, k)
		}
	}
	for id, ord := range s.memOrders {
		if ord.Status == "PENDING" && now.After(ord.ExpiresAt) {
			ord.Status = "EXPIRED"
		}
		if now.Sub(ord.CreatedAt) > ChargeHistoryTTL {
			delete(s.memOrders, id)
		}
	}
}
