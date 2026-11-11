// Package in holds what the entry of the saga asks of it.
package in

import (
	"context"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

// StartCommand is the request to cancel a departure. The tenant is the one of the token of a person, or the one of the path of the internal
// route of job 3, never of the body.
type StartCommand struct {
	TenantID, IdempotencyKey, DepartureCode string
}

// CancelDeparture is the use case behind the routes of the saga.
type CancelDeparture interface {
	// Start runs the saga to its end; created is false when the key was already used and nothing ran.
	Start(ctx context.Context, cmd StartCommand) (s saga.Saga, created bool, err error)
	Get(ctx context.Context, tenantID, id string) (saga.Saga, error)
}
