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
	if _, err := store.Save(ctx, s); !errors.Is(err, out.ErrPendingReservations) {
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
	if _, err := postgres.Open(context.Background(), "postgres://workflow_user:s3cret-pass@[::1/db", postgres.Limits{MaxConns: 2, ConnectTimeout: time.Second, StatementTimeout: time.Second}); err == nil || strings.Contains(err.Error(), "s3cret-pass") {
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

// A copy of the saga that is not the stored one writes nothing: no step, no reservation, not even a changed instant.
func TestI_a_stale_copy_with_other_steps_and_reservations_writes_nothing(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	older := s
	s, _ = store.Save(ctx, afterStepOne(s))

	older.DepartureID, older.CancelledAt = departure, cancelledAt
	older.Done(saga.CancelDeparture, now)
	older.Reservations = []saga.Reservation{{ID: rid("9"), State: saga.Opened}}
	if _, err := store.Save(ctx, older); !errors.Is(err, out.ErrStale) {
		t.Fatalf("err %v, want ErrStale", err)
	}
	got, _ := store.Get(ctx, tenant, s.ID)
	if len(got.Steps) != 2 || len(got.Reservations) != 2 || got.Version != 1 {
		t.Fatalf("stored %+v: the stale copy changed something", got)
	}
}

// A saga is completed when every reservation is open; one refused for good is not pending, so the store lets the use case end it FAILED.
func TestI_a_saga_with_every_reservation_open_is_completed(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	s, _ = store.Save(ctx, afterStepOne(s))
	s.Reservations[0].State, s.Reservations[1].State = saga.Opened, saga.Opened
	s.Done(saga.OpenEpisodes, now)
	s.End(saga.Completed, "", "", now)
	if _, err := store.Save(ctx, s); err != nil {
		t.Fatalf("err %v", err)
	}
	if got, _ := store.Get(ctx, tenant, s.ID); got.Status != saga.Completed || got.Handled() != 2 {
		t.Fatalf("stored %+v", got)
	}
}

// OPENED and FAILED are final (plan D-06): a copy with the right version that sends a reservation back to PENDING is refused and nothing is saved.
func TestI_a_reservation_that_is_opened_or_failed_does_not_go_back(t *testing.T) {
	for _, final := range []saga.ReservationState{saga.Opened, saga.EpisodeFailed} {
		store := postgres.NewStore(pgtest.Open(t))
		ctx := context.Background()
		s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
		s, _ = store.Save(ctx, afterStepOne(s))
		s.Reservations[0].State = final
		s, err := store.Save(ctx, s)
		if err != nil {
			t.Fatal(err)
		}
		s.Reservations[0].State = saga.Pending
		s.Reservations[1].State = saga.Opened
		if _, err := store.Save(ctx, s); !errors.Is(err, out.ErrStale) {
			t.Fatalf("%s: err %v, want a refusal", final, err)
		}
		got, _ := store.Get(ctx, tenant, s.ID)
		if got.Reservations[0].State != final || got.Reservations[1].State != saga.Pending || got.Version != 2 {
			t.Fatalf("%s: stored %+v: the refused save changed something", final, got)
		}
	}
}

// The steps only grow, and the departure id with the instant of the cancellation are written once: a later copy cannot take them away or change them,
// because a resumption must send the same instant.
func TestI_the_steps_only_grow_and_the_departure_id_and_the_instant_never_change(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	s, _ = store.Save(ctx, afterStepOne(s))

	fewer := s
	fewer.Steps = fewer.Steps[:1]
	if _, err := store.Save(ctx, fewer); !errors.Is(err, out.ErrStale) {
		t.Fatalf("fewer steps: err %v, want a refusal", err)
	}
	changed := s
	changed.CancelledAt = cancelledAt.Add(time.Hour)
	if _, err := store.Save(ctx, changed); !errors.Is(err, out.ErrStale) {
		t.Fatalf("another instant: err %v, want a refusal", err)
	}
	blank := s
	blank.DepartureID, blank.CancelledAt = "", time.Time{}
	saved, err := store.Save(ctx, blank)
	if err != nil || saved.DepartureID != departure || !saved.CancelledAt.Equal(cancelledAt) {
		t.Fatalf("a copy without them: saved %+v, err %v: the stored ones are kept and returned", saved, err)
	}
	got, _ := store.Get(ctx, tenant, s.ID)
	if got.DepartureID != departure || !got.CancelledAt.Equal(cancelledAt) {
		t.Fatalf("stored %+v", got)
	}
}

// PostgreSQL keeps microseconds: what Save returns is what a resumption reads, in UTC, so the instant that goes to the participants is the same.
func TestI_the_instant_of_the_cancellation_is_kept_to_the_microsecond_in_utc(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	s, _, _ := store.Create(ctx, newSaga(tenant, "key-0000001"))
	s = afterStepOne(s)
	s.CancelledAt = time.Date(2026, 11, 11, 3, 50, 0, 123456789, time.FixedZone("COT", -5*3600))
	saved, err := store.Save(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := store.Get(ctx, tenant, s.ID)
	if !got.CancelledAt.Equal(saved.CancelledAt) || got.CancelledAt.Location() != time.UTC || got.CancelledAt.Nanosecond() != 123456000 {
		t.Fatalf("returned %v, read %v: want the same instant, truncated to the microsecond, in UTC", saved.CancelledAt, got.CancelledAt)
	}
}

func TestI_a_save_with_an_unknown_or_bad_id_is_stale_and_a_bad_tenant_is_not_created(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	for _, id := range []string{"not-an-id", "99999999-9999-4999-8999-999999999999"} {
		s := newSaga(tenant, "key-0000001")
		s.ID = id
		if _, err := store.Save(ctx, s); !errors.Is(err, out.ErrStale) {
			t.Fatalf("id %q: err %v, want ErrStale", id, err)
		}
	}
	if _, _, err := store.Create(ctx, newSaga("not-a-tenant", "key-0000001")); err == nil || strings.Contains(err.Error(), "not-a-tenant") {
		t.Fatalf("err %v: a tenant that is not an id is refused without quoting it", err)
	}
}

// The listing hands out the steps and the reservations of each saga, each with its own.
func TestI_the_listing_gives_each_saga_its_own_steps_and_reservations(t *testing.T) {
	store := postgres.NewStore(pgtest.Open(t))
	ctx := context.Background()
	a, _, _ := store.Create(ctx, saga.New(tenant, "key-0000001", "SAL-001", now.Add(-2*time.Hour)))
	b, _, _ := store.Create(ctx, saga.New(tenant, "key-0000002", "SAL-002", now.Add(-time.Hour)))
	a = afterStepOne(a)
	a.UpdatedAt = now.Add(-2 * time.Hour)
	b.DepartureID, b.CancelledAt = departure, cancelledAt
	b.Done(saga.CancelDeparture, now)
	b.UpdatedAt = now.Add(-time.Hour)
	b.Reservations = []saga.Reservation{{ID: rid("3"), State: saga.Pending}}
	if _, err := store.Save(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Save(ctx, b); err != nil {
		t.Fatal(err)
	}
	stale, err := store.ListStale(ctx, now, 10)
	if err != nil || len(stale) != 2 || len(stale[0].Steps) != 2 || len(stale[0].Reservations) != 2 || len(stale[1].Steps) != 1 ||
		len(stale[1].Reservations) != 1 || stale[1].Reservations[0].ID != rid("3") {
		t.Fatalf("stale %+v, err %v", stale, err)
	}
}
