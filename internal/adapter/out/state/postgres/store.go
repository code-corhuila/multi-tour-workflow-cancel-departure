// Package postgres is the saga store over the schema workflow_cancel_departure of the single PostgreSQL instance.
package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var _ out.Store = (*Store)(nil)

// Store keeps what the tables hold: ids, the departure and its id, the instant of the cancellation, the status, the steps, the reservations
// with the state of their episode and a short reason code. No column holds the text of a participant. Ids go in as text and are cast in SQL,
// so the driver never parses them.
type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

const (
	sagaTable        = "workflow_cancel_departure.cancel_departure_saga"
	stepTable        = "workflow_cancel_departure.cancel_departure_saga_step"
	reservationTable = "workflow_cancel_departure.cancel_departure_saga_reservation"

	sagaColumns = `saga_id::text, idempotency_key, tenant_id::text, departure_code, coalesce(departure_id::text, ''), cancelled_at,
		saga_status, coalesce(failed_step, ''), coalesce(failure_reason, ''), version, created_at, updated_at`
)

// Create inserts the saga as RUNNING and gives it its id. When the tenant already used the key, nothing is inserted and the existing saga is
// returned with created false; the unique key makes two callers with one key take turns, so only one inserts.
func (s *Store) Create(ctx context.Context, sg saga.Saga) (saga.Saga, bool, error) {
	if !saga.ValidID(sg.TenantID) {
		return saga.Saga{}, false, errors.New("the tenant is not an id")
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO `+sagaTable+` (idempotency_key, tenant_id, departure_code, saga_status, created_at, updated_at)
		VALUES ($1, $2::text::uuid, $3, 'RUNNING', $4, $5)
		ON CONFLICT ON CONSTRAINT uq_cancel_departure_saga_idempotency_key DO NOTHING`,
		sg.IdempotencyKey, sg.TenantID, sg.DepartureCode, sg.CreatedAt, sg.UpdatedAt)
	if err != nil {
		return saga.Saga{}, false, err
	}
	found, err := s.load(ctx, `WHERE tenant_id = $1::text::uuid AND idempotency_key = $2`, sg.TenantID, sg.IdempotencyKey)
	if err != nil || len(found) == 0 {
		return saga.Saga{}, false, errors.Join(err, errors.New("the saga of the idempotency key vanished"))
	}
	return found[0], tag.RowsAffected() == 1, nil
}

// Save writes the saga, its steps and its reservations in one transaction, so a reservation saved is a save of its saga: the version and the
// updated instant move with it. Only a RUNNING saga is written, and only from the copy that is still the stored one (compare and swap on the
// version): a saga that ended is never overwritten and one moved by another instance is refused, both as out.ErrStale, and then nothing is
// written. The same refusal for a copy that would undo what the state keeps: fewer steps than stored, a reservation that is OPENED or FAILED
// going back, or another departure id or instant of the cancellation (these are written once, and a copy that lacks them gets the stored
// ones back). A saga that would end COMPLETED with a reservation still PENDING is refused as out.ErrPendingReservations. It returns the
// saga as it is stored, with its new version.
func (s *Store) Save(ctx context.Context, sg saga.Saga) (saga.Saga, error) {
	if !saga.ValidID(sg.ID) {
		return saga.Saga{}, out.ErrStale
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return saga.Saga{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// the saga row first: it holds the lock and the version check, so a stale copy writes no step and no reservation. The departure id and the
	// instant are kept if already set: the 24 hours of the customer count from that instant, so it must not change; PostgreSQL keeps
	// microseconds, so the instant is cut to them here and what comes back is what a resumption reads.
	var storedID *string
	var storedAt *time.Time
	err = tx.QueryRow(ctx, `UPDATE `+sagaTable+` SET saga_status = $2,
		departure_id = coalesce(departure_id, nullif($3::text, '')::uuid), cancelled_at = coalesce(cancelled_at, $4),
		failed_step = nullif($5::text, ''), failure_reason = nullif($6::text, ''), updated_at = $7, version = version + 1
		WHERE saga_id = $1::text::uuid AND saga_status = 'RUNNING' AND version = $8
		RETURNING departure_id::text, cancelled_at`,
		sg.ID, string(sg.Status), sg.DepartureID, nullableTime(sg.CancelledAt), string(sg.FailedStep), string(sg.Reason), sg.UpdatedAt, sg.Version).
		Scan(&storedID, &storedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return saga.Saga{}, out.ErrStale
	}
	if err != nil {
		return saga.Saga{}, err
	}
	if (sg.DepartureID != "" && (storedID == nil || *storedID != sg.DepartureID)) ||
		(!sg.CancelledAt.IsZero() && (storedAt == nil || !storedAt.Equal(sg.CancelledAt.Truncate(time.Microsecond)))) {
		return saga.Saga{}, out.ErrStale // another departure id or instant than the one stored; the rollback leaves it as it was
	}
	var stored int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM `+stepTable+` WHERE saga_id = $1::text::uuid`, sg.ID).Scan(&stored); err != nil {
		return saga.Saga{}, err
	}
	if stored > len(sg.Steps) {
		return saga.Saga{}, out.ErrStale // the steps only grow
	}

	batch := &pgx.Batch{}
	for i, st := range sg.Steps {
		batch.Queue(`INSERT INTO `+stepTable+` (saga_id, step_number, step_name) VALUES ($1::text::uuid, $2, $3::text)
			ON CONFLICT ON CONSTRAINT pk_cancel_departure_saga_step DO UPDATE SET step_name = EXCLUDED.step_name`,
			sg.ID, int16(i+1), string(st))
	}
	// a reservation moves from PENDING to its final state and stays there: the update is refused (no row) for one that would go back, and its
	// updated instant changes only when its state does
	for _, r := range sg.Reservations {
		batch.Queue(`INSERT INTO `+reservationTable+` AS r (saga_id, reservation_id, reservation_state, updated_at)
			VALUES ($1::text::uuid, $2::text::uuid, $3::text, $4)
			ON CONFLICT ON CONSTRAINT pk_cancel_departure_saga_reservation DO UPDATE SET reservation_state = EXCLUDED.reservation_state,
				updated_at = CASE WHEN r.reservation_state = EXCLUDED.reservation_state THEN r.updated_at ELSE EXCLUDED.updated_at END
			WHERE r.reservation_state = 'PENDING' OR r.reservation_state = EXCLUDED.reservation_state`,
			sg.ID, r.ID, string(r.State), sg.UpdatedAt)
	}
	results := tx.SendBatch(ctx, batch)
	for range sg.Steps {
		if _, err := results.Exec(); err != nil {
			_ = results.Close()
			return saga.Saga{}, err
		}
	}
	for range sg.Reservations {
		tag, err := results.Exec()
		if err != nil {
			_ = results.Close()
			return saga.Saga{}, err
		}
		if tag.RowsAffected() == 0 {
			_ = results.Close()
			return saga.Saga{}, out.ErrStale // a reservation that is final going back
		}
	}
	if err := results.Close(); err != nil {
		return saga.Saga{}, err
	}
	if sg.Status == saga.Completed {
		var pending bool
		err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM `+reservationTable+` WHERE saga_id = $1::text::uuid AND reservation_state = 'PENDING')`, sg.ID).Scan(&pending)
		if err != nil {
			return saga.Saga{}, err
		}
		if pending {
			return saga.Saga{}, out.ErrPendingReservations // the deferred rollback leaves the stored saga as it was
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return saga.Saga{}, err
	}
	sg.Version++
	if storedID != nil {
		sg.DepartureID = *storedID
	}
	if storedAt != nil {
		sg.CancelledAt = storedAt.UTC()
	}
	return sg, nil
}

// Get reads a saga of the tenant; one of another tenant, an unknown id or a text that is not an id is out.ErrNotFound.
func (s *Store) Get(ctx context.Context, tenantID, id string) (saga.Saga, error) {
	if !saga.ValidID(id) || !saga.ValidID(tenantID) {
		return saga.Saga{}, out.ErrNotFound
	}
	found, err := s.load(ctx, `WHERE saga_id = $1::text::uuid AND tenant_id = $2::text::uuid`, id, tenantID)
	if err != nil {
		return saga.Saga{}, err
	}
	if len(found) == 0 {
		return saga.Saga{}, out.ErrNotFound
	}
	return found[0], nil
}

// ListStale returns up to limit RUNNING sagas not updated since before, the one that stopped first at the front. Taking one is the Save of the
// use case, which raises the version: two instances never run the same saga.
func (s *Store) ListStale(ctx context.Context, before time.Time, limit int) ([]saga.Saga, error) {
	if limit <= 0 {
		return nil, nil
	}
	return s.load(ctx, `WHERE saga_status = 'RUNNING' AND updated_at < $1 ORDER BY updated_at, saga_id LIMIT $2`, before, limit)
}

// load reads the sagas the WHERE clause selects, then their steps in step order and their reservations in id order.
func (s *Store) load(ctx context.Context, where string, args ...any) ([]saga.Saga, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+sagaColumns+` FROM `+sagaTable+` `+where, args...)
	if err != nil {
		return nil, err
	}
	sagas, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (saga.Saga, error) {
		var sg saga.Saga
		var cancelledAt *time.Time
		var status, failed, reason string
		err := r.Scan(&sg.ID, &sg.IdempotencyKey, &sg.TenantID, &sg.DepartureCode, &sg.DepartureID, &cancelledAt,
			&status, &failed, &reason, &sg.Version, &sg.CreatedAt, &sg.UpdatedAt)
		if cancelledAt != nil {
			sg.CancelledAt = cancelledAt.UTC()
		}
		sg.Status, sg.FailedStep, sg.Reason = saga.Status(status), saga.Step(failed), saga.Reason(reason)
		return sg, err
	})
	if err != nil || len(sagas) == 0 {
		return nil, err
	}
	index := make(map[string]int, len(sagas))
	ids := make([]string, len(sagas))
	for i, sg := range sagas {
		index[sg.ID], ids[i] = i, sg.ID
	}
	if err := s.loadSteps(ctx, sagas, index, ids); err != nil {
		return nil, err
	}
	return sagas, s.loadReservations(ctx, sagas, index, ids)
}

func (s *Store) loadSteps(ctx context.Context, sagas []saga.Saga, index map[string]int, ids []string) error {
	rows, err := s.pool.Query(ctx, `SELECT saga_id::text, step_name FROM `+stepTable+`
		WHERE saga_id IN (SELECT unnest($1::text[])::uuid) ORDER BY saga_id, step_number`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return err
		}
		sg := &sagas[index[id]]
		sg.Steps = append(sg.Steps, saga.Step(name))
	}
	return rows.Err()
}

func (s *Store) loadReservations(ctx context.Context, sagas []saga.Saga, index map[string]int, ids []string) error {
	rows, err := s.pool.Query(ctx, `SELECT saga_id::text, reservation_id::text, reservation_state FROM `+reservationTable+`
		WHERE saga_id IN (SELECT unnest($1::text[])::uuid) ORDER BY saga_id, reservation_id`, ids)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, reservationID, state string
		if err := rows.Scan(&id, &reservationID, &state); err != nil {
			return err
		}
		sg := &sagas[index[id]]
		sg.Reservations = append(sg.Reservations, saga.Reservation{ID: reservationID, State: saga.ReservationState(state)})
	}
	return rows.Err()
}

// nullableTime is NULL for the instant that has not been learned yet.
func nullableTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}
