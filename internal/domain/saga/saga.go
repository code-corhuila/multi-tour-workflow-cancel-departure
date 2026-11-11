// Package saga is the state of one cancellation of a departure: what was asked, which steps ended, which reservations are handled, and how the
// saga ended. It knows nothing of HTTP or PostgreSQL. A cancellation cannot be undone, so nothing here is compensated.
package saga

import "time"

// Step is one step of the saga, named as the contract names it.
type Step string

const (
	CancelDeparture Step = "CANCEL_DEPARTURE"
	VoidProposals   Step = "VOID_PROPOSALS"
	OpenEpisodes    Step = "OPEN_EPISODES"
)

// Status is where the saga is. A saga ends COMPLETED, COMPENSATED (the cancellation was refused, so nothing changed anywhere) or FAILED
// (the departure is cancelled and a person decides what is left).
type Status string

const (
	Running     Status = "RUNNING"
	Completed   Status = "COMPLETED"
	Compensated Status = "COMPENSATED"
	Failed      Status = "FAILED"
)

// Reason says in a short code why a saga did not complete; it is never the text of a participant.
type Reason string

const (
	DepartureRefused       Reason = "DEPARTURE_REFUSED"
	ReservationsFailed     Reason = "RESERVATIONS_FAILED"
	ParticipantUnavailable Reason = "PARTICIPANT_UNAVAILABLE"
)

// ReservationState is where the episode of one reservation is: asked for again while PENDING, OPENED when it exists, EpisodeFailed when its
// owner refused it for good.
type ReservationState string

const (
	Pending       ReservationState = "PENDING"
	Opened        ReservationState = "OPENED"
	EpisodeFailed ReservationState = "FAILED"
)

// Reservation is one reservation of the cancelled departure and the state of its episode.
type Reservation struct {
	ID    string
	State ReservationState
}

// Saga is the persisted state. DepartureID and CancelledAt are the answer of step 1, kept together: the customer's 24 hours count from the
// instant of the cancellation, so a saga resumed later sends the same one. Reservations is empty until step 3 reads the list.
type Saga struct {
	ID, TenantID, IdempotencyKey, DepartureCode, DepartureID string
	CancelledAt                                              time.Time
	Status                                                   Status
	Steps                                                    []Step
	FailedStep                                               Step
	Reason                                                   Reason
	Reservations                                             []Reservation
	Version                                                  int64
	CreatedAt, UpdatedAt                                     time.Time
}

// New returns a saga that has just been asked for; the store gives it its id.
func New(tenantID, idempotencyKey, departureCode string, now time.Time) Saga {
	return Saga{TenantID: tenantID, IdempotencyKey: idempotencyKey, DepartureCode: departureCode, Status: Running, CreatedAt: now, UpdatedAt: now}
}

// Has says whether the step ended.
func (s Saga) Has(step Step) bool {
	for _, st := range s.Steps {
		if st == step {
			return true
		}
	}
	return false
}

// Done records that the step ended.
func (s *Saga) Done(step Step, now time.Time) {
	s.Steps = append(s.Steps, step)
	s.UpdatedAt = now
}

// End closes the saga. failedStep and reason are empty for a COMPLETED one.
func (s *Saga) End(status Status, failedStep Step, reason Reason, now time.Time) {
	s.Status, s.FailedStep, s.Reason, s.UpdatedAt = status, failedStep, reason, now
}

// Handled is how many reservations have their episode open.
func (s Saga) Handled() int {
	n := 0
	for _, r := range s.Reservations {
		if r.State == Opened {
			n++
		}
	}
	return n
}

// Left are the ids of the reservations whose episode is not open (failed or still pending), in the order of the list the saga holds (the store gives them by id).
func (s Saga) Left() []string {
	var left []string
	for _, r := range s.Reservations {
		if r.State != Opened {
			left = append(left, r.ID)
		}
	}
	return left
}
