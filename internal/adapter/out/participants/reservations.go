package participants

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	reservationsPrefix = "/api/v1/commercial-reservation"
	pageSize           = 100
	// maxPages bounds a list that never ends: 200 pages of 100 are 20000 reservations, more than any departure has seats for in practice. A
	// list that goes past it is not a yes, and the saga asks again later.
	maxPages = 200
)

// Reservations implements out.Reservations over HTTP.
type Reservations struct{ c *client }

var _ out.Reservations = (*Reservations)(nil)

func NewReservations(baseURL string, cfg Config) *Reservations {
	return &Reservations{newClient(baseURL, cfg)}
}

// ListAffected reads every page of the reservations that hold seats on the departure and returns their ids. The saga decides on this list, so
// one that may be incomplete is not a yes: its reservations would get no episode and no refund and the saga would end COMPLETED. The page, the
// number of pages and the total must all be said and must add up; a page that fails gives no partial list.
func (r *Reservations) ListAffected(ctx context.Context, tenantID, departureID string) ([]string, error) {
	base, err := idPath(reservationsPrefix, tenantID, departureID, "/reservations")
	if err != nil {
		return nil, err
	}
	var ids []string
	for page := 1; page <= maxPages; page++ {
		query := url.Values{"page": {strconv.Itoa(page)}, "limit": {strconv.Itoa(pageSize)}}
		status, reply, err := r.c.do(ctx, http.MethodGet, base+"?"+query.Encode(), "", nil)
		if err != nil {
			return nil, err
		}
		if status >= 400 {
			return nil, refusal(status)
		}
		var answer struct {
			Data []struct{ ReservationID string }
			Meta *struct{ Page, Total, TotalPages *int }
		}
		if json.Unmarshal(reply, &answer) != nil || answer.Data == nil || answer.Meta == nil || answer.Meta.Page == nil || answer.Meta.Total == nil ||
			answer.Meta.TotalPages == nil || *answer.Meta.Page != page {
			return nil, errUnclear("the page has not its fields")
		}
		for _, row := range answer.Data {
			if !saga.ValidID(row.ReservationID) {
				return nil, errUnclear("a reservation has no valid id")
			}
			ids = append(ids, row.ReservationID)
		}
		if page < *answer.Meta.TotalPages {
			continue
		}
		// the last page: the rows must be the pages that were said, no fewer, whatever moved while they were read
		if (*answer.Meta.TotalPages == 0) != (len(answer.Data) == 0 && page == 1) || len(ids) < *answer.Meta.Total {
			return nil, errUnclear("the pages do not add up to the total")
		}
		return ids, nil
	}
	return nil, errUnclear("the list has too many pages")
}

// VoidProposals leaves without effect the proposals of a rescheduling that point to the departure. The answer must be the 200 of the contract
// with its count and its list: a success that is not that (a proxy, a route that is not the one) is not a yes.
func (r *Reservations) VoidProposals(ctx context.Context, tenantID, departureID string, cancelledAt time.Time, key string) error {
	if !saga.ValidID(departureID) || !saga.ValidID(tenantID) {
		return notID()
	}
	// the route does not carry the departure in its path: it is a route of the tenant
	path := reservationsPrefix + "/internal/tenants/" + url.PathEscape(tenantID) + "/reschedulings/proposed-departure-cancellations"
	status, reply, err := r.post(ctx, path, key, map[string]string{"departureId": departureID, "cancelledAt": cancelledAt.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	var answer struct {
		VoidedProposals *int
		ReschedulingIDs []string
	}
	if status != http.StatusOK || json.Unmarshal(reply, &answer) != nil || answer.VoidedProposals == nil || answer.ReschedulingIDs == nil {
		return errUnclear("the answer has not its fields")
	}
	return nil
}

// OpenEpisode marks the reservation, releases its seat and freezes its deadlines; an episode that already exists answers 200 and is not changed.
// Both successes must name the episode and the reservation that was asked for.
func (r *Reservations) OpenEpisode(ctx context.Context, tenantID, reservationID, departureID string, cancelledAt time.Time, key string) error {
	if !saga.ValidID(reservationID) || !saga.ValidID(departureID) || !saga.ValidID(tenantID) {
		return notID()
	}
	path := reservationsPrefix + "/internal/tenants/" + url.PathEscape(tenantID) + "/reschedulings"
	status, reply, err := r.post(ctx, path, key, map[string]string{"reservationId": reservationID, "departureId": departureID,
		"cancelledAt": cancelledAt.UTC().Format(time.RFC3339)})
	if err != nil {
		return err
	}
	var answer struct{ ReschedulingID, ReservationID string }
	if (status != http.StatusOK && status != http.StatusCreated) || json.Unmarshal(reply, &answer) != nil || !saga.ValidID(answer.ReschedulingID) ||
		!equalIDs(answer.ReservationID, reservationID) {
		return errUnclear("the answer has not its fields")
	}
	return nil
}

// post sends a write and returns the status and the body of a success; a clear 4xx is already a sentinel of the port.
func (r *Reservations) post(ctx context.Context, path, key string, body map[string]string) (int, []byte, error) {
	status, reply, err := r.c.do(ctx, http.MethodPost, path, key, body)
	if err != nil {
		return 0, nil, err
	}
	if status >= 400 {
		return 0, nil, refusal(status)
	}
	return status, reply, nil
}
