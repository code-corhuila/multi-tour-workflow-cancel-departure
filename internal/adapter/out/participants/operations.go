package participants

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const operationsPrefix = "/api/v1/operations-cost"

// Operations implements out.Operations over HTTP.
type Operations struct{ c *client }

var _ out.Operations = (*Operations)(nil)

func NewOperations(baseURL string, cfg Config) *Operations {
	return &Operations{newClient(baseURL, cfg)}
}

// CancelDeparture cancels the departure for a cause that is not attributable to the customer (plan D-02). It answers the id of the departure and
// the instant of the cancellation, which the saga saves and sends on; an answer without a valid id or instant is not a yes. The key makes a repeat
// safe: the owner answers the original instant.
func (o *Operations) CancelDeparture(ctx context.Context, tenantID, departureCode, key string) (out.Cancellation, error) {
	path, err := departurePath(operationsPrefix, tenantID, departureCode, "/cancellation")
	if err != nil {
		return out.Cancellation{}, err
	}
	status, reply, err := o.c.do(ctx, http.MethodPost, path, key, map[string]string{"cause": "NOT_ATTRIBUTABLE_TO_CUSTOMER"})
	if err != nil {
		return out.Cancellation{}, err
	}
	if status >= 400 {
		return out.Cancellation{}, refusal(status)
	}
	var answer struct {
		DepartureID string
		CancelledAt time.Time
	}
	if json.Unmarshal(reply, &answer) != nil || !saga.ValidID(answer.DepartureID) || answer.CancelledAt.IsZero() {
		return out.Cancellation{}, errUnclear("the answer has not its fields")
	}
	return out.Cancellation{DepartureID: answer.DepartureID, CancelledAt: answer.CancelledAt.UTC().Truncate(time.Second)}, nil
}
