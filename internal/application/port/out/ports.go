// Package out holds what the saga needs from the outside, defined by the saga: the two participants and the store.
package out

import (
	"context"
	"errors"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

// The answers the saga tells apart. Any other error is a participant that did not answer well (network, 429, 5xx) after its attempts.
var (
	ErrNotFound = errors.New("not found")
	ErrRefused  = errors.New("refused by a rule of the participant")
	ErrStale    = errors.New("the saga changed since it was read")
)

// Cancellation is what operations-cost answers when it cancels a departure: the id of the departure, which commercial-reservation takes on
// its routes, and the instant of the cancellation, from which the customer's 24 hours count.
type Cancellation struct {
	DepartureID string
	CancelledAt time.Time
}

// Operations is operations-cost, which owns the departure.
type Operations interface {
	// CancelDeparture cancels the departure for a cause not attributable to the customer; it is repeatable with the same key, which answers
	// the original instant. It refuses with ErrRefused when the departure cannot be cancelled and with ErrNotFound when it does not exist.
	CancelDeparture(ctx context.Context, tenantID, departureCode, idempotencyKey string) (Cancellation, error)
}

// Reservations is commercial-reservation, which owns the reservations and their rescheduling episodes. Every call is repeatable with the same
// key; the tenant may be inactive.
type Reservations interface {
	// ListAffected returns the ids of all the reservations that hold seats on the departure, reading every page of the list.
	ListAffected(ctx context.Context, tenantID, departureID string) ([]string, error)
	// VoidProposals leaves without effect the proposals of a rescheduling that point to the departure.
	VoidProposals(ctx context.Context, tenantID, departureID string, cancelledAt time.Time, idempotencyKey string) error
	// OpenEpisode marks the reservation, releases its seat and freezes its deadlines; an episode that already exists is not changed.
	OpenEpisode(ctx context.Context, tenantID, reservationID, departureID string, cancelledAt time.Time, idempotencyKey string) error
}

// Store keeps the sagas. Create returns the saga that already has that tenant and key, with created false.
type Store interface {
	Create(ctx context.Context, s saga.Saga) (stored saga.Saga, created bool, err error)
	// Save writes the saga only if its version is the stored one and raises it; otherwise ErrStale.
	Save(ctx context.Context, s saga.Saga) (saved saga.Saga, err error)
	Get(ctx context.Context, tenantID, id string) (saga.Saga, error)
	// ListStale returns up to limit RUNNING sagas not updated since before.
	ListStale(ctx context.Context, before time.Time, limit int) ([]saga.Saga, error)
}
