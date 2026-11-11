// Package usecase is the orchestrator of the saga: it calls the participants in order, saves the state after every step and every
// reservation, and never undoes the cancellation.
package usecase

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/in"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	resumeBatch = 50
	// defaultResumeBudget is what a resumed saga may take before it is left for the next pass: one slow saga does not eat the whole pass. It is
	// shorter than the time a saga may stand still before it is taken, so a saga that is being run is never taken.
	defaultResumeBudget = 60 * time.Second
	// giveUpAfter is how long a saga may stay RUNNING because a participant gave no clear answer; then it ends FAILED for a person.
	giveUpAfter = 15 * time.Minute
)

// CancelDeparture is the orchestrator of the saga.
type CancelDeparture struct {
	operations   out.Operations
	reservations out.Reservations
	store        out.Store
	clock        func() time.Time
	log          *slog.Logger
	resumeBudget time.Duration
}

var _ in.CancelDeparture = (*CancelDeparture)(nil)

// NewCancelDeparture returns the orchestrator over its participants and its store.
func NewCancelDeparture(o out.Operations, r out.Reservations, st out.Store, clock func() time.Time, log *slog.Logger) *CancelDeparture {
	return &CancelDeparture{operations: o, reservations: r, store: st, clock: clock, log: log, resumeBudget: defaultResumeBudget}
}

// WithResumeBudget sets what each resumed saga may take; the default is a minute.
func (c *CancelDeparture) WithResumeBudget(d time.Duration) *CancelDeparture {
	c.resumeBudget = d
	return c
}

// Start creates the saga, or returns the one that has the same tenant and key without running anything, and runs it to its end.
// A departure that does not exist ends the saga refused and is also returned as out.ErrNotFound.
func (c *CancelDeparture) Start(ctx context.Context, cmd in.StartCommand) (saga.Saga, bool, error) {
	s, created, err := c.store.Create(ctx, saga.New(cmd.TenantID, cmd.IdempotencyKey, cmd.DepartureCode, c.clock()))
	if err != nil || !created {
		return s, false, err
	}
	id := s.ID
	s, err = c.run(ctx, s)
	if err != nil && ctx.Err() != nil {
		// the budget of the request ended before an answer could be saved: the saga exists and goes on in the resumption, so it is answered as it
		// is stored and not with an error that would hide it
		fresh, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if stored, getErr := c.store.Get(fresh, cmd.TenantID, id); getErr == nil {
			return stored, true, nil
		}
	}
	return s, true, err
}

// Get reads a saga of the tenant; another tenant's is out.ErrNotFound.
func (c *CancelDeparture) Get(ctx context.Context, tenantID, id string) (saga.Saga, error) {
	return c.store.Get(ctx, tenantID, id)
}

// Resume continues the RUNNING sagas that have not moved since before, each from where it stopped; a saga another instance saved first is
// left to it. It returns how many it took.
func (c *CancelDeparture) Resume(ctx context.Context, before time.Time) (int, error) {
	stale, err := c.store.ListStale(ctx, before, resumeBatch)
	if err != nil {
		return 0, err
	}
	resumed := 0
	for _, s := range stale {
		s.UpdatedAt = c.clock()
		claimed, err := c.store.Save(ctx, s) // raises the version: a second instance with the old one is refused
		if errors.Is(err, out.ErrStale) {
			continue
		}
		if err != nil {
			c.log.ErrorContext(ctx, "a saga could not be claimed", "sagaId", s.ID, "error", err.Error())
			continue
		}
		resumed++
		runCtx, stop := context.WithTimeout(ctx, c.resumeBudget)
		_, err = c.run(runCtx, claimed)
		stop()
		if err != nil {
			c.log.ErrorContext(ctx, "resumed saga did not end well", "sagaId", claimed.ID, "error", err.Error())
		}
	}
	return resumed, nil
}

// run goes on from the first step that has not ended. A step that ends the saga returns it as it is.
func (c *CancelDeparture) run(ctx context.Context, s saga.Saga) (saga.Saga, error) {
	if !s.Has(saga.CancelDeparture) {
		var ended bool
		var err error
		if s, ended, err = c.cancelDeparture(ctx, s); ended || err != nil {
			return s, err
		}
	}
	if !s.Has(saga.VoidProposals) {
		var ended bool
		var err error
		if s, ended, err = c.voidProposals(ctx, s); ended || err != nil {
			return s, err
		}
	}
	return c.openEpisodes(ctx, s)
}

// cancelDeparture is step 1: operations-cost cancels the departure and answers its id and the instant. Both are saved at once, because a
// saga resumed later must send the same instant. A refusal changes nothing anywhere, so it is the only way to COMPENSATED.
func (c *CancelDeparture) cancelDeparture(ctx context.Context, s saga.Saga) (saga.Saga, bool, error) {
	done, err := c.operations.CancelDeparture(ctx, s.TenantID, s.DepartureCode, s.ID+":CANCEL_DEPARTURE")
	if err != nil {
		if !definite(err) {
			ended, endErr := c.unclear(ctx, s, saga.CancelDeparture, err)
			return ended, true, endErr
		}
		ended, endErr := c.end(ctx, s, saga.Compensated, saga.CancelDeparture, saga.DepartureRefused)
		if errors.Is(err, out.ErrNotFound) {
			return ended, true, errors.Join(out.ErrNotFound, endErr)
		}
		return ended, true, endErr
	}
	if !saga.ValidID(done.DepartureID) || done.CancelledAt.IsZero() {
		// what is not an id or an instant is not saved or sent: the departure may be cancelled, so the saga asks again with the same key
		ended, endErr := c.unclear(ctx, s, saga.CancelDeparture, errors.New("the cancellation has no usable departure id or instant"))
		return ended, true, endErr
	}
	s.DepartureID, s.CancelledAt = done.DepartureID, done.CancelledAt
	s.Done(saga.CancelDeparture, c.clock())
	saved, err := c.store.Save(ctx, s)
	return saved, false, err
}

// voidProposals is step 2: the proposals of a rescheduling that point to the departure are left without effect. A refusal for good is a
// request that commercial-reservation will not accept, so the saga ends FAILED for a person.
func (c *CancelDeparture) voidProposals(ctx context.Context, s saga.Saga) (saga.Saga, bool, error) {
	if err := c.reservations.VoidProposals(ctx, s.TenantID, s.DepartureID, s.CancelledAt, s.ID+":VOID_PROPOSALS"); err != nil {
		if !definite(err) {
			ended, endErr := c.unclear(ctx, s, saga.VoidProposals, err)
			return ended, true, endErr
		}
		ended, endErr := c.end(ctx, s, saga.Failed, saga.VoidProposals, saga.ReservationsFailed)
		return ended, true, endErr
	}
	s.Done(saga.VoidProposals, c.clock())
	saved, err := c.store.Save(ctx, s)
	return saved, false, err
}

// openEpisodes is step 3. The whole list of reservations is read and saved before the first episode is opened: opening one changes the
// reservation, so the pages of a list read while opening would shift. Then each pending reservation is asked for, and saved as it is
// handled. A reservation refused for good is marked and does not stop the others; one that gave no clear answer stays pending and is asked
// again, with the same key, by the next pass.
func (c *CancelDeparture) openEpisodes(ctx context.Context, s saga.Saga) (saga.Saga, error) {
	if len(s.Reservations) == 0 {
		ids, err := c.reservations.ListAffected(ctx, s.TenantID, s.DepartureID)
		if err != nil {
			if !definite(err) {
				return c.unclear(ctx, s, saga.OpenEpisodes, err)
			}
			return c.end(ctx, s, saga.Failed, saga.OpenEpisodes, saga.ReservationsFailed)
		}
		ids, usable := distinctIDs(ids)
		if !usable {
			return c.unclear(ctx, s, saga.OpenEpisodes, errors.New("the list has an id that is not an id"))
		}
		for _, id := range ids {
			s.Reservations = append(s.Reservations, saga.Reservation{ID: id, State: saga.Pending})
		}
		s.UpdatedAt = c.clock()
		saved, err := c.store.Save(ctx, s)
		if err != nil {
			return saved, err
		}
		s = saved
	}
	for i := range s.Reservations {
		if ctx.Err() != nil {
			break // the budget is over: the reservations that are left stay pending and are asked in the next pass
		}
		if s.Reservations[i].State != saga.Pending {
			continue
		}
		id := s.Reservations[i].ID
		err := c.reservations.OpenEpisode(ctx, s.TenantID, id, s.DepartureID, s.CancelledAt, s.ID+":OPEN_EPISODE:"+id)
		switch {
		case err == nil:
			s.Reservations[i].State = saga.Opened
		case definite(err):
			c.log.WarnContext(ctx, "an episode was refused", "sagaId", s.ID, "reservationId", id, "error", err.Error())
			s.Reservations[i].State = saga.EpisodeFailed
		default:
			c.log.WarnContext(ctx, "a reservation gave no clear answer", "sagaId", s.ID, "reservationId", id, "error", err.Error())
			continue // it stays pending: nothing is saved for it
		}
		s.UpdatedAt = c.clock()
		saved, err := c.store.Save(ctx, s)
		if err != nil {
			return saved, err
		}
		s = saved
	}
	return c.finish(ctx, s)
}

// finish ends the saga when no reservation is pending, and gives up on the pending ones after giveUpAfter.
func (c *CancelDeparture) finish(ctx context.Context, s saga.Saga) (saga.Saga, error) {
	pending, failed := 0, 0
	for _, r := range s.Reservations {
		switch r.State {
		case saga.Pending:
			pending++
		case saga.EpisodeFailed:
			failed++
		}
	}
	switch {
	case pending > 0:
		return c.unclear(ctx, s, saga.OpenEpisodes, errors.New("a reservation gave no clear answer"))
	case failed > 0:
		return c.end(ctx, s, saga.Failed, saga.OpenEpisodes, saga.ReservationsFailed)
	}
	s.Done(saga.OpenEpisodes, c.clock())
	s.End(saga.Completed, "", "", c.clock())
	return c.store.Save(ctx, s)
}

// unclear handles an answer that is neither a yes nor a clear refusal (network, 429, 5xx after the attempts of the client): the participant
// may have done the work and lost the reply, so the saga stays RUNNING and the resumption asks again with the same key, which the
// participant repeats safely; after giveUpAfter it ends FAILED for a person. On an error the saga returned is the one the caller had.
func (c *CancelDeparture) unclear(ctx context.Context, s saga.Saga, step saga.Step, cause error) (saga.Saga, error) {
	c.log.WarnContext(ctx, "a participant gave no clear answer", "sagaId", s.ID, "step", string(step), "error", cause.Error())
	if ctx.Err() == nil && c.clock().Sub(s.CreatedAt) >= giveUpAfter {
		return c.end(ctx, s, saga.Failed, step, saga.ParticipantUnavailable)
	}
	return s, nil
}

func (c *CancelDeparture) end(ctx context.Context, s saga.Saga, status saga.Status, failedStep saga.Step, reason saga.Reason) (saga.Saga, error) {
	s.End(status, failedStep, reason, c.clock())
	return c.store.Save(ctx, s)
}

// definite says whether the answer of a participant is a definite yes or no. Any other error leaves open whether the work was done.
func definite(err error) bool {
	return errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound)
}

// distinctIDs drops the repeated ids of a list and says whether every one is an id: they go into the state, into paths and into SQL.
func distinctIDs(ids []string) ([]string, bool) {
	seen := make(map[string]bool, len(ids))
	var distinct []string
	for _, id := range ids {
		if !saga.ValidID(id) {
			return nil, false
		}
		if !seen[strings.ToLower(id)] {
			seen[strings.ToLower(id)] = true
			distinct = append(distinct, id)
		}
	}
	return distinct, true
}
