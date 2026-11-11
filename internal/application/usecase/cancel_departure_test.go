package usecase_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/in"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/usecase"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	tenant      = "11111111-1111-4111-8111-111111111111"
	otherTenant = "22222222-2222-4222-8222-222222222222"
	departure   = "55555555-5555-4555-8555-555555555555"
	reservation = "66666666-6666-4666-8666-66666666666%d"
	code        = "SAL-013"
)

var (
	now = time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)
	// the instant of the cancellation that operations-cost answers: never the clock of the saga
	cancelledAt = now.Add(-10 * time.Minute)
)

func rid(n int) string { return fmt.Sprintf(reservation, n) }

// log is the order of the calls to the participants, across both.
type fakeOperations struct {
	log     *[]string
	cancels int
	answer  func(n int) (out.Cancellation, error)
}

func (f *fakeOperations) CancelDeparture(_ context.Context, tenantID, departureCode, key string) (out.Cancellation, error) {
	f.cancels++
	*f.log = append(*f.log, "cancel:"+tenantID+"/"+departureCode+"/"+key)
	if f.answer != nil {
		return f.answer(f.cancels - 1)
	}
	return out.Cancellation{DepartureID: departure, CancelledAt: cancelledAt}, nil
}

type voidCall struct {
	Tenant, Departure string
	CancelledAt       time.Time
	Key               string
}

type openCall struct {
	Tenant, Reservation, Departure string
	CancelledAt                    time.Time
	Key                            string
}

// fakeReservations is commercial-reservation: it answers as told and records every call.
type fakeReservations struct {
	log     *[]string
	ids     []string
	listErr error
	lists   int
	voidErr error
	voids   []voidCall
	opens   []openCall
	// open decides each call to open an episode by its position (0 is the first)
	open func(n int, c openCall) error
}

func (f *fakeReservations) ListAffected(_ context.Context, tenantID, departureID string) ([]string, error) {
	f.lists++
	*f.log = append(*f.log, "list")
	return f.ids, f.listErr
}

func (f *fakeReservations) VoidProposals(_ context.Context, tenantID, departureID string, at time.Time, key string) error {
	f.voids = append(f.voids, voidCall{tenantID, departureID, at, key})
	*f.log = append(*f.log, "void")
	return f.voidErr
}

func (f *fakeReservations) OpenEpisode(_ context.Context, tenantID, reservationID, departureID string, at time.Time, key string) error {
	c := openCall{tenantID, reservationID, departureID, at, key}
	f.opens = append(f.opens, c)
	*f.log = append(*f.log, "open:"+reservationID)
	if f.open == nil {
		return nil
	}
	return f.open(len(f.opens)-1, c)
}

// memStore keeps the sagas in memory with the version rule of the real store.
type memStore struct {
	byID   map[string]saga.Saga
	saves  int
	nextID int
	// listing, when set, is what ListStale answers: the copies another instance may have already changed
	listing []saga.Saga
	// failSave, when above 0, is the number of the Save call (1 is the first) that is refused as stale
	failSave, attempts int
}

func newStore() *memStore { return &memStore{byID: map[string]saga.Saga{}} }

// copyOf gives the slices of a saga their own memory: a database hands out copies, so a change of the orchestrator that is not saved must not show.
func copyOf(s saga.Saga) saga.Saga {
	s.Steps, s.Reservations = slices.Clone(s.Steps), slices.Clone(s.Reservations)
	return s
}

func (m *memStore) Create(_ context.Context, s saga.Saga) (saga.Saga, bool, error) {
	for _, existing := range m.byID {
		if existing.TenantID == s.TenantID && existing.IdempotencyKey == s.IdempotencyKey {
			return copyOf(existing), false, nil
		}
	}
	m.nextID++
	s.ID = fmt.Sprintf("saga-%d", m.nextID)
	m.byID[s.ID] = copyOf(s)
	return copyOf(s), true, nil
}

func (m *memStore) Save(_ context.Context, s saga.Saga) (saga.Saga, error) {
	m.attempts++
	if m.attempts == m.failSave || m.byID[s.ID].Version != s.Version {
		return saga.Saga{}, out.ErrStale
	}
	s.Version++
	m.byID[s.ID] = copyOf(s)
	m.saves++
	return copyOf(s), nil
}

func (m *memStore) Get(_ context.Context, tenantID, id string) (saga.Saga, error) {
	if s, ok := m.byID[id]; ok && s.TenantID == tenantID {
		return copyOf(s), nil
	}
	return saga.Saga{}, out.ErrNotFound
}

func (m *memStore) ListStale(_ context.Context, before time.Time, limit int) ([]saga.Saga, error) {
	if m.listing != nil {
		return slices.Clone(m.listing), nil
	}
	var stale []saga.Saga
	for _, s := range m.byID {
		if s.Status == saga.Running && s.UpdatedAt.Before(before) {
			stale = append(stale, copyOf(s))
		}
	}
	return stale, nil
}

type rig struct {
	ops   *fakeOperations
	res   *fakeReservations
	store *memStore
	log   *[]string
	uc    *usecase.CancelDeparture
}

// newRig starts with a departure that has two reservations.
func newRig() *rig {
	log := &[]string{}
	r := &rig{ops: &fakeOperations{log: log}, res: &fakeReservations{log: log, ids: []string{rid(1), rid(2)}}, store: newStore(), log: log}
	r.uc = usecase.NewCancelDeparture(r.ops, r.res, r.store, func() time.Time { return now }, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	return r
}

func cmd(key string) in.StartCommand {
	return in.StartCommand{TenantID: tenant, IdempotencyKey: key, DepartureCode: code}
}

func steps(s saga.Saga) string {
	var names []string
	for _, st := range s.Steps {
		names = append(names, string(st))
	}
	return strings.Join(names, ",")
}

// stale makes the stored saga look as if it had stopped moving long ago, and created that long ago.
func (r *rig) stale(id string, age time.Duration) {
	s := r.store.byID[id]
	s.CreatedAt, s.UpdatedAt = now.Add(-age), now.Add(-age)
	r.store.byID[id] = s
}

func (r *rig) resume() (int, error) {
	return r.uc.Resume(context.Background(), now.Add(-2*time.Minute))
}

func (r *rig) calls(prefix string) int {
	n := 0
	for _, c := range *r.log {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

// AC-WF4-01.1
func TestA_the_departure_is_cancelled_first_and_its_instant_is_kept(t *testing.T) {
	r := newRig()
	s, created, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || !created {
		t.Fatalf("created %v, err %v", created, err)
	}
	if len(*r.log) == 0 || !strings.HasPrefix((*r.log)[0], "cancel:"+tenant+"/"+code+"/"+s.ID+":CANCEL_DEPARTURE") {
		t.Fatalf("calls %v: the cancellation must be the first call, with the derived key", *r.log)
	}
	if s.DepartureID != departure || !s.CancelledAt.Equal(cancelledAt) {
		t.Fatalf("departure id %q, instant %v: want the answer of operations-cost kept", s.DepartureID, s.CancelledAt)
	}
}

// AC-WF4-01.2
func TestA_each_reservation_gets_its_episode_with_the_instant_of_the_cancellation(t *testing.T) {
	r := newRig()
	s, _, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	if len(r.res.opens) != 2 {
		t.Fatalf("%d episodes opened, want 2", len(r.res.opens))
	}
	for i, c := range r.res.opens {
		want := openCall{tenant, rid(i + 1), departure, cancelledAt, s.ID + ":OPEN_EPISODE:" + rid(i+1)}
		if c.Tenant != want.Tenant || c.Reservation != want.Reservation || c.Departure != want.Departure || !c.CancelledAt.Equal(want.CancelledAt) || c.Key != want.Key {
			t.Fatalf("call %d = %+v, want %+v (the instant is the cancellation's, not the clock's)", i, c, want)
		}
	}
}

// AC-WF4-01.3
func TestA_the_saga_completes_when_every_reservation_has_its_episode(t *testing.T) {
	r := newRig()
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || s.Status != saga.Completed || steps(s) != "CANCEL_DEPARTURE,VOID_PROPOSALS,OPEN_EPISODES" {
		t.Fatalf("status %s, steps %s, err %v", s.Status, steps(s), err)
	}
	if len(s.Reservations) != 2 || s.Reservations[0].State != saga.Opened || s.Reservations[1].State != saga.Opened {
		t.Fatalf("reservations %+v: want both opened", s.Reservations)
	}
}

func TestA_a_departure_without_reservations_completes_with_no_episode(t *testing.T) {
	r := newRig()
	r.res.ids = nil
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || s.Status != saga.Completed || len(r.res.opens) != 0 {
		t.Fatalf("status %s, episodes %d, err %v", s.Status, len(r.res.opens), err)
	}
}

// AC-WF4-01.4
func TestA_the_proposals_towards_the_departure_are_voided(t *testing.T) {
	r := newRig()
	s, _, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	if len(r.res.voids) != 1 {
		t.Fatalf("%d calls to void the proposals, want 1", len(r.res.voids))
	}
	v := r.res.voids[0]
	if v.Tenant != tenant || v.Departure != departure || !v.CancelledAt.Equal(cancelledAt) || v.Key != s.ID+":VOID_PROPOSALS" {
		t.Fatalf("call %+v", v)
	}
	if want := []string{"cancel", "void", "list", "open", "open"}; !inOrder(*r.log, want) {
		t.Fatalf("calls %v: want the order %v", *r.log, want)
	}
}

// inOrder says whether the calls start with these kinds, in this order.
func inOrder(log []string, kinds []string) bool {
	if len(log) != len(kinds) {
		return false
	}
	for i, k := range kinds {
		if !strings.HasPrefix(log[i], k) {
			return false
		}
	}
	return true
}

// AC-WF4-01.5: the cancellation is irreversible, so what fails is carried forward and ends for a person.
func TestA_a_failing_reservation_does_not_stop_the_others_and_the_saga_ends_failed(t *testing.T) {
	r := newRig()
	r.res.ids = []string{rid(1), rid(2), rid(3)}
	r.res.open = func(_ int, c openCall) error {
		if c.Reservation == rid(2) {
			return fmt.Errorf("422 from the participant: %w", out.ErrRefused)
		}
		return nil
	}
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Status != saga.Failed || s.FailedStep != saga.OpenEpisodes || s.Reason != saga.ReservationsFailed {
		t.Fatalf("status %s, failed step %s, reason %s", s.Status, s.FailedStep, s.Reason)
	}
	if len(r.res.opens) != 3 {
		t.Fatalf("%d episodes asked: the third reservation must be handled after the second failed", len(r.res.opens))
	}
	if left := s.Left(); len(left) != 1 || left[0] != rid(2) {
		t.Fatalf("left %v, want only the reservation that failed", left)
	}
}

func TestA_a_cancellation_is_never_undone(t *testing.T) {
	r := newRig()
	r.res.open = func(int, openCall) error { return out.ErrRefused }
	s, _, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	if s.Status != saga.Failed || r.ops.cancels != 1 || r.calls("cancel") != 1 {
		t.Fatalf("status %s, cancellations %d: the departure is cancelled once and nothing is asked of operations-cost afterwards", s.Status, r.ops.cancels)
	}
	if !s.Has(saga.CancelDeparture) {
		t.Fatal("step 1 is not recorded as ended")
	}
}

func TestA_an_unclear_answer_is_repeated_with_the_same_key_and_the_saga_then_completes(t *testing.T) {
	r := newRig()
	r.res.open = func(n int, _ openCall) error {
		if n == 1 {
			return errors.New("connection reset") // the second reservation may have been opened and the answer lost
		}
		return nil
	}
	first, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || first.Status != saga.Running || first.Reservations[0].State != saga.Opened || first.Reservations[1].State != saga.Pending {
		t.Fatalf("status %s, reservations %+v, err %v: want RUNNING with the second still pending", first.Status, first.Reservations, err)
	}
	r.stale(first.ID, time.Hour)
	if resumed, err := r.resume(); err != nil || resumed != 1 {
		t.Fatalf("resumed %d, err %v", resumed, err)
	}
	got := r.store.byID[first.ID]
	if got.Status != saga.Completed {
		t.Fatalf("status %s, want COMPLETED", got.Status)
	}
	var keys []string
	for _, c := range r.res.opens {
		if c.Reservation == rid(2) {
			keys = append(keys, c.Key)
		}
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("keys of the second reservation %v: want the same key twice", keys)
	}
}

func TestA_a_saga_that_keeps_a_reservation_pending_for_fifteen_minutes_ends_failed_for_a_person(t *testing.T) {
	r := newRig()
	r.res.open = func(n int, _ openCall) error {
		if n == 1 {
			return errors.New("down")
		}
		return nil
	}
	first, _, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	r.stale(first.ID, 20*time.Minute)
	r.res.open = func(int, openCall) error { return errors.New("down") }
	if _, err := r.resume(); err != nil {
		t.Fatal(err)
	}
	got := r.store.byID[first.ID]
	if got.Status != saga.Failed || got.FailedStep != saga.OpenEpisodes || got.Reason != saga.ParticipantUnavailable {
		t.Fatalf("status %s, failed step %s, reason %s", got.Status, got.FailedStep, got.Reason)
	}
	if left := got.Left(); len(left) != 1 || left[0] != rid(2) {
		t.Fatalf("left %v, want the pending reservation", left)
	}
}

// AC-WF4-01.6
func TestA_a_refused_cancellation_touches_no_reservation_and_ends_compensated(t *testing.T) {
	r := newRig()
	r.ops.answer = func(int) (out.Cancellation, error) { return out.Cancellation{}, fmt.Errorf("422: %w", out.ErrRefused) }
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || s.Status != saga.Compensated || s.FailedStep != saga.CancelDeparture || s.Reason != saga.DepartureRefused {
		t.Fatalf("status %s, failed step %s, reason %s, err %v", s.Status, s.FailedStep, s.Reason, err)
	}
	if len(*r.log) != 1 {
		t.Fatalf("calls %v: nothing may be asked of commercial-reservation", *r.log)
	}
}

func TestA_an_unknown_departure_is_not_found_and_changes_nothing(t *testing.T) {
	r := newRig()
	r.ops.answer = func(int) (out.Cancellation, error) { return out.Cancellation{}, out.ErrNotFound }
	s, created, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if !errors.Is(err, out.ErrNotFound) || !created || s.Status != saga.Compensated || len(*r.log) != 1 {
		t.Fatalf("created %v, status %s, err %v, calls %v", created, s.Status, err, *r.log)
	}
}

func TestA_an_unclear_cancellation_leaves_the_saga_running_and_ends_failed_after_fifteen_minutes(t *testing.T) {
	r := newRig()
	r.ops.answer = func(int) (out.Cancellation, error) { return out.Cancellation{}, errors.New("connection reset") }
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || s.Status != saga.Running || len(s.Steps) != 0 || len(*r.log) != 1 {
		t.Fatalf("status %s, steps %s, calls %v, err %v", s.Status, steps(s), *r.log, err)
	}
	r.stale(s.ID, 20*time.Minute)
	r.resume()
	got := r.store.byID[s.ID]
	if got.Status != saga.Failed || got.FailedStep != saga.CancelDeparture || got.Reason != saga.ParticipantUnavailable {
		t.Fatalf("status %s, failed step %s, reason %s", got.Status, got.FailedStep, got.Reason)
	}
}

func TestA_a_refusal_to_void_the_proposals_ends_failed_naming_the_step(t *testing.T) {
	r := newRig()
	r.res.voidErr = out.ErrRefused
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if r.res.lists != 0 || len(r.res.opens) != 0 {
		t.Fatalf("lists %d, episodes %d: after a refused void nothing is listed or opened", r.res.lists, len(r.res.opens))
	}
	if err != nil || s.Status != saga.Failed || s.FailedStep != saga.VoidProposals || s.Reason != saga.ReservationsFailed {
		t.Fatalf("status %s, failed step %s, reason %s, err %v", s.Status, s.FailedStep, s.Reason, err)
	}
}

func TestA_a_list_that_cannot_be_read_does_not_open_any_episode(t *testing.T) {
	for name, listErr := range map[string]error{"unclear": errors.New("timeout"), "refused": out.ErrRefused} {
		r := newRig()
		r.res.listErr = listErr
		s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
		if err != nil || len(r.res.opens) != 0 {
			t.Fatalf("%s: err %v, episodes %d", name, err, len(r.res.opens))
		}
		wantStatus := saga.Running
		if name == "refused" {
			wantStatus = saga.Failed
		}
		if s.Status != wantStatus {
			t.Fatalf("%s: status %s, want %s", name, s.Status, wantStatus)
		}
	}
}

// AC-WF4-01.7
func TestA_a_stopped_saga_handles_only_the_reservations_left(t *testing.T) {
	r := newRig()
	stopped := saga.New(tenant, "key-0000001", code, now.Add(-time.Hour))
	stopped.DepartureID, stopped.CancelledAt = departure, cancelledAt
	stopped.Steps = []saga.Step{saga.CancelDeparture, saga.VoidProposals}
	stopped.Reservations = []saga.Reservation{{ID: rid(1), State: saga.Opened}, {ID: rid(2), State: saga.Pending}}
	stored, _, _ := r.store.Create(context.Background(), stopped)
	stored.UpdatedAt = now.Add(-time.Hour)
	r.store.byID[stored.ID] = stored

	resumed, err := r.resume()
	if err != nil || resumed != 1 {
		t.Fatalf("resumed %d, err %v", resumed, err)
	}
	got := r.store.byID[stored.ID]
	if got.Status != saga.Completed || len(r.res.opens) != 1 || r.res.opens[0].Reservation != rid(2) {
		t.Fatalf("status %s, episodes %+v: only the pending reservation is asked", got.Status, r.res.opens)
	}
	if r.ops.cancels != 0 || len(r.res.voids) != 0 || r.res.lists != 0 {
		t.Fatalf("cancellations %d, voids %d, lists %d: no step that ended is repeated", r.ops.cancels, len(r.res.voids), r.res.lists)
	}
	if !got.CancelledAt.Equal(cancelledAt) || r.res.opens[0].CancelledAt != cancelledAt {
		t.Fatalf("instant %v then %v: a resumed saga sends the same instant", got.CancelledAt, r.res.opens[0].CancelledAt)
	}
}

func TestA_a_saga_claimed_by_another_instance_is_left_alone(t *testing.T) {
	r := newRig()
	stopped := saga.New(tenant, "key-0000001", code, now.Add(-time.Hour))
	stored, _, _ := r.store.Create(context.Background(), stopped)
	stored.UpdatedAt = now.Add(-time.Hour)
	r.store.byID[stored.ID] = stored
	// another instance saved it between the listing and the claim: the listing has the old copy, the store a newer one
	r.store.listing = []saga.Saga{stored}
	other := stored
	other.Version = 7
	r.store.byID[stored.ID] = other

	if resumed, _ := r.resume(); resumed != 0 || r.ops.cancels != 0 {
		t.Fatalf("resumed %d, cancellations %d: a saga that changed under the claim must be skipped", resumed, r.ops.cancels)
	}
}

// RN-WFL4: the state is saved after every step and after every reservation.
func TestA_state_is_saved_after_each_step_and_each_reservation(t *testing.T) {
	r := newRig()
	r.uc.Start(context.Background(), cmd("key-0000001"))
	// the cancellation, the proposals, the list, two reservations and the end
	if r.store.saves != 6 {
		t.Fatalf("%d saves: want 6", r.store.saves)
	}
}

func TestA_a_stale_save_after_the_cancellation_asks_nothing_more(t *testing.T) {
	r := newRig()
	r.store.failSave = 1
	_, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if !errors.Is(err, out.ErrStale) || len(*r.log) != 1 {
		t.Fatalf("err %v, calls %v: want ErrStale and only the cancellation", err, *r.log)
	}
}

// AC-WF4-01.8
func TestA_the_same_key_returns_the_same_saga_and_runs_nothing_again(t *testing.T) {
	r := newRig()
	first, created, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	second, createdAgain, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || !created || createdAgain || second.ID != first.ID {
		t.Fatalf("created %v then %v, ids %s and %s, err %v", created, createdAgain, first.ID, second.ID, err)
	}
	if r.ops.cancels != 1 || len(r.res.opens) != 2 || len(r.res.voids) != 1 {
		t.Fatalf("cancellations %d, voids %d, episodes %d: the repeat must run no step", r.ops.cancels, len(r.res.voids), len(r.res.opens))
	}
}

func TestA_two_tenants_may_use_the_same_key(t *testing.T) {
	r := newRig()
	a, _, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	other := cmd("key-0000001")
	other.TenantID = otherTenant
	b, createdB, _ := r.uc.Start(context.Background(), other)
	if !createdB || a.ID == b.ID {
		t.Fatalf("created %v, ids %s and %s: another tenant has its own saga", createdB, a.ID, b.ID)
	}
}

func TestA_get_reads_only_the_saga_of_the_tenant(t *testing.T) {
	r := newRig()
	s, _, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	if got, err := r.uc.Get(context.Background(), tenant, s.ID); err != nil || got.ID != s.ID {
		t.Fatalf("got %v, err %v", got.ID, err)
	}
	if _, err := r.uc.Get(context.Background(), otherTenant, s.ID); !errors.Is(err, out.ErrNotFound) {
		t.Fatalf("err %v, want ErrNotFound for another tenant", err)
	}
}

// Another instance moved the saga while this one was in the middle of it: no call follows a save that was refused, and the saga that comes
// back from a failed save is not read.
func TestA_a_stale_save_after_the_proposals_or_inside_the_loop_asks_nothing_more(t *testing.T) {
	cases := map[int][]string{
		2: {"cancel", "void"},                           // the save after the proposals
		3: {"cancel", "void", "list"},                   // the save of the list
		4: {"cancel", "void", "list", "open:" + rid(1)}, // the save after the first reservation
	}
	for failSave, want := range cases {
		r := newRig()
		r.store.failSave = failSave
		_, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
		if !errors.Is(err, out.ErrStale) || !inOrder(*r.log, want) {
			t.Fatalf("save %d refused: err %v, calls %v, want ErrStale and %v", failSave, err, *r.log, want)
		}
	}
}

// An answer that is not clear in step 1, 2 or 3 is asked again with the same key, and the instant of the cancellation does not change.
func TestA_an_unclear_cancellation_is_asked_again_with_the_same_key_and_keeps_its_instant(t *testing.T) {
	r := newRig()
	r.ops.answer = func(n int) (out.Cancellation, error) {
		if n == 0 {
			return out.Cancellation{}, errors.New("connection reset")
		}
		return out.Cancellation{DepartureID: departure, CancelledAt: cancelledAt}, nil
	}
	first, _, _ := r.uc.Start(context.Background(), cmd("key-0000001"))
	r.stale(first.ID, time.Hour)
	if resumed, err := r.resume(); err != nil || resumed != 1 {
		t.Fatalf("resumed %d, err %v", resumed, err)
	}
	got := r.store.byID[first.ID]
	if got.Status != saga.Completed || !got.CancelledAt.Equal(cancelledAt) {
		t.Fatalf("status %s, instant %v", got.Status, got.CancelledAt)
	}
	if !strings.HasPrefix((*r.log)[0], "cancel:") || (*r.log)[0] != (*r.log)[1] {
		t.Fatalf("calls %v: the cancellation must be asked twice with the same key", *r.log)
	}
}

func TestA_an_unclear_void_or_list_is_asked_again_and_gives_up_after_fifteen_minutes(t *testing.T) {
	for name, tc := range map[string]struct {
		set  func(r *rig)
		step saga.Step
	}{
		"void": {func(r *rig) { r.res.voidErr = errors.New("timeout") }, saga.VoidProposals},
		"list": {func(r *rig) { r.res.listErr = errors.New("timeout") }, saga.OpenEpisodes},
	} {
		r := newRig()
		tc.set(r)
		s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
		if err != nil || s.Status != saga.Running || len(r.res.opens) != 0 {
			t.Fatalf("%s: status %s, episodes %d, err %v", name, s.Status, len(r.res.opens), err)
		}
		r.stale(s.ID, 20*time.Minute)
		r.resume()
		got := r.store.byID[s.ID]
		if got.Status != saga.Failed || got.FailedStep != tc.step || got.Reason != saga.ParticipantUnavailable {
			t.Fatalf("%s: status %s, failed step %s, reason %s", name, got.Status, got.FailedStep, got.Reason)
		}
		if len(r.res.opens) != 0 {
			t.Fatalf("%s: an episode was opened without a list or after the proposals failed", name)
		}
	}
}

// A reservation refused for good is not asked again; one that was unclear is, and the saga stays running until it ends.
func TestA_failed_and_pending_reservations_keep_the_saga_running_and_only_the_pending_are_asked_again(t *testing.T) {
	r := newRig()
	r.res.ids = []string{rid(1), rid(2), rid(3)}
	r.res.open = func(_ int, c openCall) error {
		switch c.Reservation {
		case rid(1):
			return out.ErrRefused
		case rid(2):
			return errors.New("timeout")
		}
		return nil
	}
	first, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || first.Status != saga.Running {
		t.Fatalf("status %s, err %v: a pending reservation keeps the saga running", first.Status, err)
	}
	r.stale(first.ID, time.Hour)
	r.res.open = func(int, openCall) error { return nil }
	before := len(r.res.opens)
	r.resume()
	asked := r.res.opens[before:]
	if len(asked) != 1 || asked[0].Reservation != rid(2) {
		t.Fatalf("asked again %+v: only the pending reservation", asked)
	}
	got := r.store.byID[first.ID]
	if got.Status != saga.Failed || got.Reason != saga.ReservationsFailed || len(got.Left()) != 1 || got.Left()[0] != rid(1) {
		t.Fatalf("status %s, reason %s, left %v: it ends FAILED naming the one refused", got.Status, got.Reason, got.Left())
	}
}

func TestA_a_list_that_is_not_found_ends_failed_and_opens_nothing(t *testing.T) {
	r := newRig()
	r.res.listErr = out.ErrNotFound
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || s.Status != saga.Failed || s.FailedStep != saga.OpenEpisodes || len(r.res.opens) != 0 {
		t.Fatalf("status %s, failed step %s, episodes %d, err %v", s.Status, s.FailedStep, len(r.res.opens), err)
	}
}

// What a participant answers goes into the state and into paths: an id that is not an id, or an instant that is not one, is not a yes.
func TestA_a_cancellation_without_a_usable_departure_id_or_instant_is_unclear(t *testing.T) {
	for name, answer := range map[string]out.Cancellation{
		"no id":      {CancelledAt: cancelledAt},
		"not an id":  {DepartureID: "SAL-013", CancelledAt: cancelledAt},
		"no instant": {DepartureID: departure},
	} {
		r := newRig()
		r.ops.answer = func(int) (out.Cancellation, error) { return answer, nil }
		s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
		if err != nil || s.Status != saga.Running || len(s.Steps) != 0 || len(*r.log) != 1 || r.store.saves != 0 {
			t.Fatalf("%s: status %s, steps %s, calls %v, saves %d, err %v", name, s.Status, steps(s), *r.log, r.store.saves, err)
		}
	}
}

func TestA_a_list_with_an_invalid_id_is_unclear_and_a_repeated_id_is_listed_once(t *testing.T) {
	r := newRig()
	r.res.ids = []string{rid(1), "not-an-id"}
	s, _, err := r.uc.Start(context.Background(), cmd("key-0000001"))
	if err != nil || s.Status != saga.Running || len(r.res.opens) != 0 || len(s.Reservations) != 0 {
		t.Fatalf("status %s, episodes %d, reservations %v, err %v: an invalid id is not saved", s.Status, len(r.res.opens), s.Reservations, err)
	}
	r = newRig()
	r.res.ids = []string{rid(1), rid(2), rid(1)}
	s, _, _ = r.uc.Start(context.Background(), cmd("key-0000001"))
	if len(s.Reservations) != 2 || len(r.res.opens) != 2 {
		t.Fatalf("reservations %v, episodes %d: a repeated id is one reservation", s.Reservations, len(r.res.opens))
	}
}
