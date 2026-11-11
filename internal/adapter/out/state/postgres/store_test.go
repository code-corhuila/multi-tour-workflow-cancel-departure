package postgres_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/adapter/out/state/postgres"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/adapter/out/state/postgres/pgtest"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	tenant      = "11111111-1111-4111-8111-111111111111"
	other       = "22222222-2222-4222-8222-222222222222"
	departure   = "55555555-5555-4555-8555-555555555555"
	reservation = "66666666-6666-4666-8666-66666666666%s"
)

var (
	now         = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	cancelledAt = now.Add(-10 * time.Minute)
)

func rid(n string) string { return strings.Replace(reservation, "%s", n, 1) }

func newSaga(tenantID, key string) saga.Saga { return saga.New(tenantID, key, "SAL-013", now) }

// afterStepOne is a saga that passed step 1 and step 2 and listed two reservations.
func afterStepOne(s saga.Saga) saga.Saga {
	s.DepartureID, s.CancelledAt = departure, cancelledAt
	s.Done(saga.CancelDeparture, now)
	s.Done(saga.VoidProposals, now)
	s.Reservations = []saga.Reservation{{ID: rid("1"), State: saga.Pending}, {ID: rid("2"), State: saga.Pending}}
	return s
}

// RN: the state is saved and read back whole.
func TestI_a_saga_is_created_and_read_back_whole(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	created, isNew, err := store.Create(ctx, newSaga(tenant, "key-0000001"))
	if err != nil || !isNew || created.ID == "" || created.Status != saga.Running || created.Version != 0 {
		t.Fatalf("created %+v, new %v, err %v", created, isNew, err)
	}
	got, err := store.Get(ctx, tenant, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.DepartureCode != "SAL-013" || got.TenantID != tenant || got.IdempotencyKey != "key-0000001" || !got.CreatedAt.Equal(now) ||
		got.DepartureID != "" || !got.CancelledAt.IsZero() || len(got.Steps) != 0 || len(got.Reservations) != 0 {
		t.Fatalf("read back %+v: a new saga has no departure id, no instant, no step and no reservation", got)
	}
}

// AC-WF4-01.8: the same tenant and key give the same saga; another tenant has its own.
func TestI_the_same_tenant_and_key_return_the_same_saga_and_another_tenant_has_its_own(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	first, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	again, isNew, err := store.Create(ctx, newSaga(tenant, "key-0000001"))
	if err != nil || isNew || again.ID != first.ID {
		t.Fatalf("ids %s and %s, new %v, err %v", first.ID, again.ID, isNew, err)
	}
	otherSaga, isNew, err := store.Create(ctx, newSaga(other, "key-0000001"))
	if err != nil || !isNew || otherSaga.ID == first.ID {
		t.Fatalf("other tenant: id %s, new %v, err %v", otherSaga.ID, isNew, err)
	}
}

// AC-WF4-01.7, Annex E rule 2: the steps, the departure id with the instant, the reservations and the end are persisted.
func TestI_the_steps_the_instant_the_reservations_and_the_end_are_persisted(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	s, err := store.Save(ctx, afterStepOne(s))
	if err != nil || s.Version != 1 {
		t.Fatalf("version %d, err %v", s.Version, err)
	}
	s.Reservations[0].State = saga.Opened
	s.Reservations[1].State = saga.EpisodeFailed
	s.Done(saga.OpenEpisodes, now)
	s.End(saga.Failed, saga.OpenEpisodes, saga.ReservationsFailed, now.Add(time.Second))
	if s, err = store.Save(ctx, s); err != nil || s.Version != 2 {
		t.Fatalf("version %d, err %v", s.Version, err)
	}

	got, err := store.Get(ctx, tenant, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != saga.Failed || got.FailedStep != saga.OpenEpisodes || got.Reason != saga.ReservationsFailed ||
		got.DepartureID != departure || !got.CancelledAt.Equal(cancelledAt) || !got.UpdatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("read back %+v", got)
	}
	if len(got.Steps) != 3 || got.Steps[0] != saga.CancelDeparture || got.Steps[1] != saga.VoidProposals || got.Steps[2] != saga.OpenEpisodes {
		t.Fatalf("steps %v, want the three in order", got.Steps)
	}
	if len(got.Reservations) != 2 || got.Reservations[0] != (saga.Reservation{ID: rid("1"), State: saga.Opened}) ||
		got.Reservations[1] != (saga.Reservation{ID: rid("2"), State: saga.EpisodeFailed}) || got.Handled() != 1 {
		t.Fatalf("reservations %+v", got.Reservations)
	}
}

// Annex E: a save from an older copy is refused, and a saga that ended is never overwritten.
func TestI_a_save_from_an_older_copy_is_refused_and_an_ended_saga_is_not_overwritten(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	older := s
	s.DepartureID, s.CancelledAt = departure, cancelledAt
	s.Done(saga.CancelDeparture, now)
	if _, err := store.Save(ctx, s); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, older); !errors.Is(err, out.ErrStale) {
		t.Fatalf("err %v, want ErrStale for the older copy", err)
	}

	ended, _ := store.Get(ctx, tenant, s.ID)
	ended.Done(saga.VoidProposals, now)
	ended.Done(saga.OpenEpisodes, now)
	ended.End(saga.Completed, "", "", now)
	ended, err := store.Save(ctx, ended)
	if err != nil {
		t.Fatal(err)
	}
	ended.End(saga.Failed, saga.OpenEpisodes, saga.ReservationsFailed, now)
	if _, err := store.Save(ctx, ended); !errors.Is(err, out.ErrStale) {
		t.Fatalf("err %v, want ErrStale: a COMPLETED saga cannot be rewritten", err)
	}
}

// A saga is never closed COMPLETED while one of its reservations is PENDING: the resumption only looks for RUNNING sagas, so that reservation
// would be lost. It crosses two tables, so it is the store that keeps it.
func TestI_a_saga_with_a_pending_reservation_cannot_be_completed(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	s, _ = store.Save(ctx, afterStepOne(s))
	s.Reservations[0].State = saga.Opened // the second stays PENDING
	s.End(saga.Completed, "", "", now)
	if _, err := store.Save(ctx, s); !errors.Is(err, postgres.ErrPendingReservations) {
		t.Fatalf("err %v, want ErrPendingReservations", err)
	}
	got, _ := store.Get(ctx, tenant, s.ID)
	if got.Status != saga.Running || got.Reservations[0].State != saga.Pending || got.Version != 1 {
		t.Fatalf("stored %+v: a refused save changes nothing, the reservations included", got)
	}
}

// A reservation saved is a save of its saga: the version and the updated instant move, so the resumption does not claim a saga that is still
// going through its reservations.
func TestI_saving_a_reservation_also_raises_the_version_and_the_updated_instant_of_its_saga(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	s, _ = store.Save(ctx, afterStepOne(s))
	s.Reservations[0].State = saga.Opened
	s.UpdatedAt = now.Add(time.Minute)
	s, err := store.Save(ctx, s)
	if err != nil || s.Version != 2 {
		t.Fatalf("version %d, err %v", s.Version, err)
	}
	got, _ := store.Get(ctx, tenant, s.ID)
	if got.Version != 2 || !got.UpdatedAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("version %d, updated %v: the saga must move with its reservation", got.Version, got.UpdatedAt)
	}
	if stale, _ := store.ListStale(ctx, now.Add(30*time.Second), 10); len(stale) != 0 {
		t.Fatalf("%d stale sagas: one that just saved a reservation is not stopped", len(stale))
	}
}

func TestI_get_reads_only_the_saga_of_the_tenant_and_a_bad_id_is_not_found(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	if _, err := store.Get(ctx, other, s.ID); !errors.Is(err, out.ErrNotFound) {
		t.Fatalf("another tenant: err %v", err)
	}
	if _, err := store.Get(ctx, tenant, "not-a-uuid"); !errors.Is(err, out.ErrNotFound) {
		t.Fatalf("bad id: err %v", err)
	}
	if _, err := store.Get(ctx, tenant, "99999999-9999-4999-8999-999999999999"); !errors.Is(err, out.ErrNotFound) {
		t.Fatalf("unknown id: err %v", err)
	}
}

// The resumption looks for RUNNING sagas that stopped advancing.
func TestI_list_stale_returns_only_running_sagas_not_updated_since_the_cutoff_oldest_first(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	old, _, _ := store.Create(ctx, saga.New(tenant, "key-0000001", "SAL-001", now.Add(-time.Hour)))
	older, _, _ := store.Create(ctx, saga.New(tenant, "key-0000002", "SAL-002", now.Add(-2*time.Hour)))
	store.Create(ctx, saga.New(tenant, "key-0000003", "SAL-003", now)) // fresh
	done, _, _ := store.Create(ctx, saga.New(tenant, "key-0000004", "SAL-004", now.Add(-3*time.Hour)))
	done.End(saga.Compensated, saga.CancelDeparture, saga.DepartureRefused, now.Add(-3*time.Hour))
	if _, err := store.Save(ctx, done); err != nil {
		t.Fatal(err)
	}

	stale, err := store.ListStale(ctx, now.Add(-2*time.Minute), 10)
	if err != nil || len(stale) != 2 || stale[0].ID != older.ID || stale[1].ID != old.ID {
		t.Fatalf("stale %v, err %v: want the two RUNNING old ones, oldest first", len(stale), err)
	}
	if limited, _ := store.ListStale(ctx, now.Add(-2*time.Minute), 1); len(limited) != 1 {
		t.Fatalf("%d with limit 1", len(limited))
	}
}

func TestI_list_stale_with_no_room_returns_nothing(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	for _, limit := range []int{0, -1} {
		if got, err := store.ListStale(context.Background(), now, limit); err != nil || len(got) != 0 {
			t.Fatalf("limit %d: %d sagas, err %v", limit, len(got), err)
		}
	}
}

// Annex J: the service connects as its own user, which writes the tables but cannot delete from them.
func TestI_the_store_works_as_the_app_user_which_cannot_delete(t *testing.T) {
	pool := pgtest.OpenAsApp(t)
	store := postgres.NewStore(pool)
	ctx := context.Background()
	s, _, err := store.Create(ctx, newSaga(tenant, "key-0000001"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, afterStepOne(s)); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"cancel_departure_saga", "cancel_departure_saga_step", "cancel_departure_saga_reservation"} {
		if _, err := pool.Exec(ctx, `DELETE FROM `+table); err == nil {
			t.Fatalf("the app user deleted from %s", table)
		}
	}
}

// The errors of the driver quote the connection string, and it carries the password.
func TestI_open_names_the_kind_of_failure_and_never_the_connection_string(t *testing.T) {
	_, err := postgres.Open(context.Background(), "postgres://workflow_user:s3cret-pass@127.0.0.1:1/db?sslmode=disable",
		postgres.Limits{MaxConns: 2, ConnectTimeout: time.Second, StatementTimeout: time.Second})
	if err == nil {
		t.Fatal("a closed port was reported as open")
	}
	for _, leaked := range []string{"s3cret-pass", "workflow_user", "127.0.0.1"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("the error leaks %q: %v", leaked, err)
		}
	}
	if _, err := postgres.Open(context.Background(), "::not a url::", postgres.Limits{}); err == nil || strings.Contains(err.Error(), "::not") {
		t.Fatalf("a bad url: %v", err)
	}
}

// Limits left at zero would mean a connection that times out at once or no statement timeout at all: they are refused at start.
func TestI_open_refuses_limits_that_are_not_positive(t *testing.T) {
	url := "postgres://u:p@127.0.0.1:1/db?sslmode=disable"
	for _, limits := range []postgres.Limits{{}, {MaxConns: 2, ConnectTimeout: time.Second}, {MaxConns: 2, StatementTimeout: time.Second},
		{ConnectTimeout: time.Second, StatementTimeout: time.Second}} {
		if _, err := postgres.Open(context.Background(), url, limits); err == nil || !strings.Contains(err.Error(), "limits") {
			t.Fatalf("limits %+v: err %v, want a refusal that names the limits", limits, err)
		}
	}
}
