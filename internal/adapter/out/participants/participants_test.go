package participants_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/adapter/out/participants"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/correlation"
)

const (
	tenant      = "11111111-1111-4111-8111-111111111111"
	departure   = "55555555-5555-4555-8555-555555555555"
	reservation = "66666666-6666-4666-8666-666666666661"
	token       = "service-token-value"
)

var (
	cfg         = participants.Config{Token: token, Timeout: time.Second, Attempts: 3}
	cancelledAt = time.Date(2026, 11, 11, 8, 50, 0, 0, time.UTC)
)

// seen is what a participant received.
type seen struct {
	Method, URI, Key, Auth, Correlation, ContentType string
	Body                                             map[string]any
}

// server answers each request with the next of the given replies (the last repeats) and records what it received.
type server struct {
	*httptest.Server
	mu       sync.Mutex
	requests []seen
}

type reply struct {
	status int
	body   string
}

func newServer(t *testing.T, replies ...reply) *server {
	t.Helper()
	s := &server{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		n := len(s.requests)
		s.requests = append(s.requests, seen{r.Method, r.URL.RequestURI(), r.Header.Get("Idempotency-Key"), r.Header.Get("Authorization"),
			r.Header.Get("X-Correlation-Id"), r.Header.Get("Content-Type"), body})
		s.mu.Unlock()
		rep := replies[min(n, len(replies)-1)]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rep.status)
		_, _ = w.Write([]byte(rep.body))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *server) count() int { s.mu.Lock(); defer s.mu.Unlock(); return len(s.requests) }

const openedBody = `{"reschedulingId":"77777777-7777-4777-8777-777777777777","reservationId":"66666666-6666-4666-8666-666666666661"}`

const cancelled = `{"departureId":"55555555-5555-4555-8555-555555555555","cancelledAt":"2026-11-11T08:50:00Z"}`

// AC-WF4-01.11: every call carries the service token, the tenant in the path, the derived key and the correlation id.
func TestC_the_cancellation_carries_the_service_token_the_tenant_the_key_and_the_cause_and_gives_the_id_and_the_instant(t *testing.T) {
	srv := newServer(t, reply{200, cancelled})
	ctx := correlation.With(context.Background(), "c0ffee00-0000-4000-8000-000000000001")
	got, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(ctx, tenant, "SAL-013", "saga-1:CANCEL_DEPARTURE")
	if err != nil || got.DepartureID != departure || !got.CancelledAt.Equal(cancelledAt) {
		t.Fatalf("answer %+v, err %v", got, err)
	}
	r := srv.requests[0]
	if r.Method != "POST" || r.URI != "/api/v1/operations-cost/internal/tenants/"+tenant+"/departures/SAL-013/cancellation" || r.Auth != "Bearer "+token ||
		r.Key != "saga-1:CANCEL_DEPARTURE" || r.Correlation != "c0ffee00-0000-4000-8000-000000000001" || !strings.HasPrefix(r.ContentType, "application/json") ||
		r.Body["cause"] != "NOT_ATTRIBUTABLE_TO_CUSTOMER" {
		t.Fatalf("request %+v", r)
	}
}

// The code of a departure is text a person typed: it cannot climb out of its path, so a code that is not plain is refused before any call.
func TestC_the_departure_code_cannot_change_the_path(t *testing.T) {
	srv := newServer(t, reply{200, cancelled})
	for _, code := range []string{"../x", "a/b", "x?y=1", "x#z", "x y", "", strings.Repeat("A", 41), "%2e%2e"} {
		if _, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(context.Background(), tenant, code, "k-0000001"); !errors.Is(err, out.ErrRefused) {
			t.Fatalf("code %q: err %v, want ErrRefused", code, err)
		}
	}
	if srv.count() != 0 {
		t.Fatalf("%d calls were made with a code that is not plain", srv.count())
	}
}

// The saga saves what the participant answers and sends it on: an answer without a valid id or instant is not a yes.
func TestC_a_cancellation_without_a_usable_id_or_instant_is_unclear(t *testing.T) {
	for _, body := range []string{`{"hello":"world"}`, `{"cancelledAt":"2026-11-11T08:50:00Z"}`, `{"departureId":"SAL-013","cancelledAt":"2026-11-11T08:50:00Z"}`,
		`{"departureId":"55555555-5555-4555-8555-555555555555"}`, `{"departureId":"55555555-5555-4555-8555-555555555555","cancelledAt":"yesterday"}`} {
		srv := newServer(t, reply{200, body})
		_, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(context.Background(), tenant, "SAL-013", "k-0000001")
		if err == nil || errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound) {
			t.Fatalf("body %s: err %v, want an unclear answer", body, err)
		}
	}
}

func TestC_the_episode_is_opened_with_the_reservation_the_departure_and_the_instant_and_both_successes_are_a_yes(t *testing.T) {
	for _, status := range []int{201, 200} {
		srv := newServer(t, reply{status, openedBody})
		err := participants.NewReservations(srv.URL, cfg).OpenEpisode(context.Background(), tenant, reservation, departure, cancelledAt, "saga-1:OPEN_EPISODE:"+reservation)
		if err != nil {
			t.Fatalf("status %d: err %v", status, err)
		}
		r := srv.requests[0]
		if r.Method != "POST" || r.URI != "/api/v1/commercial-reservation/internal/tenants/"+tenant+"/reschedulings" || r.Auth != "Bearer "+token ||
			r.Key != "saga-1:OPEN_EPISODE:"+reservation || r.Body["reservationId"] != reservation || r.Body["departureId"] != departure ||
			r.Body["cancelledAt"] != "2026-11-11T08:50:00Z" {
			t.Fatalf("request %+v: the instant goes as RFC 3339 in UTC", r)
		}
	}
}

func TestC_the_proposals_are_voided_with_the_departure_and_the_instant(t *testing.T) {
	srv := newServer(t, reply{200, `{"voidedProposals":2,"reschedulingIds":[]}`})
	if err := participants.NewReservations(srv.URL, cfg).VoidProposals(context.Background(), tenant, departure, cancelledAt, "saga-1:VOID_PROPOSALS"); err != nil {
		t.Fatal(err)
	}
	r := srv.requests[0]
	if r.Method != "POST" || r.URI != "/api/v1/commercial-reservation/internal/tenants/"+tenant+"/reschedulings/proposed-departure-cancellations" ||
		r.Key != "saga-1:VOID_PROPOSALS" || r.Body["departureId"] != departure || r.Body["cancelledAt"] != "2026-11-11T08:50:00Z" {
		t.Fatalf("request %+v", r)
	}
}

// The list is read page by page until the last one, and every id comes back.
func TestC_the_list_of_reservations_is_read_page_by_page_until_the_last(t *testing.T) {
	a, b, c := reservation, "66666666-6666-4666-8666-666666666662", "66666666-6666-4666-8666-666666666663"
	srv := newServer(t,
		reply{200, `{"data":[{"reservationId":"` + a + `"},{"reservationId":"` + b + `"}],"meta":{"page":1,"limit":100,"total":3,"totalPages":2}}`},
		reply{200, `{"data":[{"reservationId":"` + c + `"}],"meta":{"page":2,"limit":100,"total":3,"totalPages":2}}`})
	ids, err := participants.NewReservations(srv.URL, cfg).ListAffected(context.Background(), tenant, departure)
	if err != nil || len(ids) != 3 || ids[0] != a || ids[1] != b || ids[2] != c {
		t.Fatalf("ids %v, err %v", ids, err)
	}
	base := "/api/v1/commercial-reservation/internal/tenants/" + tenant + "/departures/" + departure + "/reservations"
	if srv.count() != 2 || srv.requests[0].URI != base+"?limit=100&page=1" || srv.requests[1].URI != base+"?limit=100&page=2" || srv.requests[0].Method != "GET" ||
		srv.requests[0].Auth != "Bearer "+token {
		t.Fatalf("requests %+v", srv.requests)
	}
}

func TestC_a_departure_with_no_reservation_gives_an_empty_list(t *testing.T) {
	srv := newServer(t, reply{200, `{"data":[],"meta":{"page":1,"limit":100,"total":0,"totalPages":0}}`})
	ids, err := participants.NewReservations(srv.URL, cfg).ListAffected(context.Background(), tenant, departure)
	if err != nil || len(ids) != 0 || srv.count() != 1 {
		t.Fatalf("ids %v, requests %d, err %v", ids, srv.count(), err)
	}
}

// A list that never ends must not make the saga loop for ever: it is not a yes, and the saga asks again later.
func TestC_a_list_that_never_ends_or_has_a_bad_id_or_no_fields_is_unclear(t *testing.T) {
	for name, rep := range map[string]reply{
		"endless":   {200, `{"data":[{"reservationId":"` + reservation + `"}],"meta":{"page":1,"limit":100,"total":9999999,"totalPages":99999}}`},
		"bad id":    {200, `{"data":[{"reservationId":"not-an-id"}],"meta":{"page":1,"limit":100,"total":1,"totalPages":1}}`},
		"no fields": {200, `{"hello":"world"}`},
	} {
		srv := newServer(t, rep)
		_, err := participants.NewReservations(srv.URL, cfg).ListAffected(context.Background(), tenant, departure)
		if err == nil || errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound) {
			t.Fatalf("%s: err %v, want an unclear answer", name, err)
		}
		if srv.count() > 200 {
			t.Fatalf("%s: %d requests: the pages are bounded", name, srv.count())
		}
	}
}

// The saga tells these answers apart: the owners' own words are reduced to a definite sentinel.
func TestC_definite_answers_become_the_sentinels_of_the_port(t *testing.T) {
	cases := []struct {
		name string
		rep  reply
		want error
	}{
		{"not found", reply{404, `{"error":"NOT_FOUND","message":"x","traceId":"t"}`}, out.ErrNotFound},
		{"business rule", reply{422, `{"error":"BUSINESS_RULE_VIOLATION","message":"x","traceId":"t"}`}, out.ErrRefused},
		{"another 422", reply{422, `{"error":"INVALID_STATUS_TRANSITION","message":"x","traceId":"t"}`}, out.ErrRefused},
		{"bad request", reply{400, `{"error":"VALIDATION_ERROR","message":"x","traceId":"t"}`}, out.ErrRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newServer(t, tc.rep)
			calls := map[string]error{
				"cancel": func() error {
					_, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(context.Background(), tenant, "SAL-013", "k-0000001")
					return err
				}(),
				"open": participants.NewReservations(srv.URL, cfg).OpenEpisode(context.Background(), tenant, reservation, departure, cancelledAt, "k-0000002"),
				"void": participants.NewReservations(srv.URL, cfg).VoidProposals(context.Background(), tenant, departure, cancelledAt, "k-0000003"),
				"list": func() error {
					_, err := participants.NewReservations(srv.URL, cfg).ListAffected(context.Background(), tenant, departure)
					return err
				}(),
			}
			for call, err := range calls {
				if !errors.Is(err, tc.want) {
					t.Fatalf("%s: err %v, want %v", call, err, tc.want)
				}
			}
			if srv.count() != 4 {
				t.Fatalf("%d requests: a clear answer is not repeated", srv.count())
			}
		})
	}
}

// A 401 or 403 is the token of the saga, a fault of configuration and not a decision about the departure: it says nothing about the work, so
// it is unclear and the saga is asked again after the token is fixed, instead of being reported as refused.
func TestC_a_refused_token_is_an_unclear_answer_not_a_refusal(t *testing.T) {
	for _, status := range []int{401, 403} {
		srv := newServer(t, reply{status, `{"error":"FORBIDDEN","message":"x","traceId":"t"}`})
		err := participants.NewReservations(srv.URL, cfg).OpenEpisode(context.Background(), tenant, reservation, departure, cancelledAt, "k-0000001")
		if err == nil || errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound) {
			t.Fatalf("status %d: err %v, want an unclear answer", status, err)
		}
	}
}

// The tenant comes from the token, but it goes into a path; the departure of the reservation routes is an id, not a code.
func TestC_a_tenant_or_a_departure_that_is_not_an_id_is_refused_before_any_call(t *testing.T) {
	srv := newServer(t, reply{200, `{}`})
	res := participants.NewReservations(srv.URL, cfg)
	for _, bad := range []string{"", "..", "a/b", "not-a-uuid", "SAL-013"} {
		if _, err := res.ListAffected(context.Background(), bad, departure); !errors.Is(err, out.ErrRefused) {
			t.Fatalf("tenant %q: err %v", bad, err)
		}
		if _, err := res.ListAffected(context.Background(), tenant, bad); !errors.Is(err, out.ErrRefused) {
			t.Fatalf("departure %q: err %v", bad, err)
		}
		if err := res.OpenEpisode(context.Background(), tenant, bad, departure, cancelledAt, "k-0000001"); !errors.Is(err, out.ErrRefused) {
			t.Fatalf("reservation %q: err %v", bad, err)
		}
		if err := res.VoidProposals(context.Background(), tenant, bad, cancelledAt, "k-0000001"); !errors.Is(err, out.ErrRefused) {
			t.Fatalf("void departure %q: err %v", bad, err)
		}
	}
	if srv.count() != 0 {
		t.Fatalf("%d calls", srv.count())
	}
}

// ADR-004 D10: network errors, 429 and 5xx are repeated with the same key; nothing else is.
func TestC_transient_answers_are_repeated_with_the_same_key_and_then_succeed(t *testing.T) {
	for _, status := range []int{500, 502, 503, 429} {
		srv := newServer(t, reply{status, `{}`}, reply{201, openedBody})
		err := participants.NewReservations(srv.URL, cfg).OpenEpisode(context.Background(), tenant, reservation, departure, cancelledAt, "same-key-1")
		if err != nil || srv.count() != 2 || srv.requests[0].Key != srv.requests[1].Key {
			t.Fatalf("status %d: err %v, requests %d", status, err, srv.count())
		}
	}
}

// An answer that stays unclear after the attempts is an error that is none of the sentinels, and it leaks nothing.
func TestC_an_answer_that_stays_unclear_is_not_a_refusal_and_leaks_nothing(t *testing.T) {
	srv := newServer(t, reply{503, `{"error":"X","message":"jdbc:postgresql://db:5432 password=hunter2"}`})
	err := participants.NewReservations(srv.URL, cfg).VoidProposals(context.Background(), tenant, departure, cancelledAt, "k-0000001")
	if err == nil || errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound) {
		t.Fatalf("err %v: want an unclear error", err)
	}
	if srv.count() != 3 || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "jdbc") || strings.Contains(err.Error(), token) {
		t.Fatalf("%d requests, err %q", srv.count(), err.Error())
	}
}

func TestC_a_server_that_is_down_is_an_unclear_answer(t *testing.T) {
	srv := newServer(t, reply{200, cancelled})
	url := srv.URL
	srv.Close()
	_, err := participants.NewOperations(url, cfg).CancelDeparture(context.Background(), tenant, "SAL-013", "k-0000001")
	if err == nil || errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound) {
		t.Fatalf("err %v", err)
	}
}

// A redirect would send the credential elsewhere: it is not followed, and it is an unclear answer.
func TestC_a_redirect_is_not_followed(t *testing.T) {
	elsewhere := newServer(t, reply{200, cancelled})
	srv := newServer(t, reply{307, ``})
	srv.Server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, elsewhere.URL+"/x", http.StatusTemporaryRedirect)
	})
	_, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(context.Background(), tenant, "SAL-013", "k-0000001")
	if err == nil || elsewhere.count() != 0 {
		t.Fatalf("err %v, requests that reached the other host: %d", err, elsewhere.count())
	}
}

func TestC_a_cancelled_context_stops_the_attempts(t *testing.T) {
	srv := newServer(t, reply{503, `{}`})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(ctx, tenant, "SAL-013", "k-0000001")
	if err == nil || errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound) || srv.count() > 1 {
		t.Fatalf("err %v, %d requests", err, srv.count())
	}
}

// The saga keeps what the list says and decides on it: a list that may be incomplete is not a yes, because the reservations it lacks would get no
// episode and no refund and the saga would end COMPLETED.
func TestC_a_list_that_may_be_incomplete_is_unclear(t *testing.T) {
	row := `{"reservationId":"` + reservation + `"}`
	for name, body := range map[string]string{
		"no total pages":       `{"data":[` + row + `],"meta":{"page":1,"limit":100,"total":1}}`,
		"zero pages with data": `{"data":[` + row + `],"meta":{"page":1,"limit":100,"total":1,"totalPages":0}}`,
		"another page":         `{"data":[` + row + `],"meta":{"page":2,"limit":100,"total":1,"totalPages":1}}`,
		"no data":              `{"data":null,"meta":{"page":1,"limit":100,"total":0,"totalPages":0}}`,
		"fewer ids than total": `{"data":[` + row + `],"meta":{"page":1,"limit":100,"total":5,"totalPages":1}}`,
		"no total":             `{"data":[` + row + `],"meta":{"page":1,"limit":100,"totalPages":1}}`,
	} {
		srv := newServer(t, reply{200, body})
		_, err := participants.NewReservations(srv.URL, cfg).ListAffected(context.Background(), tenant, departure)
		if err == nil || errors.Is(err, out.ErrRefused) || errors.Is(err, out.ErrNotFound) {
			t.Fatalf("%s: err %v, want an unclear answer", name, err)
		}
	}
}

func TestC_a_page_that_fails_gives_no_partial_list(t *testing.T) {
	srv := newServer(t, reply{200, `{"data":[{"reservationId":"` + reservation + `"}],"meta":{"page":1,"limit":100,"total":101,"totalPages":2}}`}, reply{503, `{}`})
	ids, err := participants.NewReservations(srv.URL, cfg).ListAffected(context.Background(), tenant, departure)
	if err == nil || ids != nil {
		t.Fatalf("ids %v, err %v: the second page failed, so there is no list", ids, err)
	}
}

// The instant goes to the participants in seconds, in UTC; the saga saves the same one it sends, so a resumption sends it again unchanged.
func TestC_the_instant_of_the_cancellation_is_cut_to_the_second_in_utc_when_it_is_received(t *testing.T) {
	srv := newServer(t, reply{200, `{"departureId":"` + departure + `","cancelledAt":"2026-11-11T03:50:00.987654-05:00"}`})
	got, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(context.Background(), tenant, "SAL-013", "k-0000001")
	if err != nil || !got.CancelledAt.Equal(cancelledAt) || got.CancelledAt.Location() != time.UTC || got.CancelledAt.Nanosecond() != 0 {
		t.Fatalf("instant %v, err %v: want 2026-11-11T08:50:00Z", got.CancelledAt, err)
	}
}

// Opening an episode and voiding the proposals are decisions of the saga that cannot be undone: a success that does not say what the contract promises
// (a proxy, a route that is not the one) is not a yes.
func TestC_a_success_without_the_answer_of_the_contract_is_unclear(t *testing.T) {
	res := func(srv *server) *participants.Reservations { return participants.NewReservations(srv.URL, cfg) }
	for name, rep := range map[string]reply{
		"empty object": {201, `{}`}, "garbage": {200, `<html>ok</html>`}, "no content": {204, ``}, "accepted": {202, openedBody},
		"another reservation": {201, `{"reschedulingId":"77777777-7777-4777-8777-777777777777","reservationId":"66666666-6666-4666-8666-666666666662"}`},
	} {
		srv := newServer(t, rep)
		if err := res(srv).OpenEpisode(context.Background(), tenant, reservation, departure, cancelledAt, "k-0000001"); err == nil || errors.Is(err, out.ErrRefused) {
			t.Fatalf("open, %s: err %v, want an unclear answer", name, err)
		}
	}
	for name, rep := range map[string]reply{
		"empty object": {200, `{}`}, "garbage": {200, `ok`}, "created": {201, `{"voidedProposals":0,"reschedulingIds":[]}`}, "no content": {204, ``},
	} {
		srv := newServer(t, rep)
		if err := res(srv).VoidProposals(context.Background(), tenant, departure, cancelledAt, "k-0000001"); err == nil || errors.Is(err, out.ErrRefused) {
			t.Fatalf("void, %s: err %v, want an unclear answer", name, err)
		}
	}
}

// A refusal says which status it was and never what the participant wrote: the orchestrator logs it, and a 400 of ours must not look like a rule.
func TestC_a_refusal_carries_its_status_and_none_of_the_text_of_the_participant(t *testing.T) {
	srv := newServer(t, reply{400, `{"error":"VALIDATION_ERROR","message":"secret detail of the participant","traceId":"t"}`})
	_, err := participants.NewOperations(srv.URL, cfg).CancelDeparture(context.Background(), tenant, "SAL-013", "k-0000001")
	if !errors.Is(err, out.ErrRefused) || !strings.Contains(err.Error(), "400") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err %v", err)
	}
	srv = newServer(t, reply{404, `{"message":"secret detail"}`})
	err = participants.NewReservations(srv.URL, cfg).VoidProposals(context.Background(), tenant, departure, cancelledAt, "k-0000001")
	if !errors.Is(err, out.ErrNotFound) || !strings.Contains(err.Error(), "404") || strings.Contains(err.Error(), "secret") {
		t.Fatalf("err %v", err)
	}
}

// ADR-004 D10: three attempts with a base wait of 200 ms unless the composition root says otherwise; a configuration left at zero must not turn
// the retries off without a word.
func TestC_the_defaults_are_the_attempts_and_the_wait_of_the_plan(t *testing.T) {
	srv := newServer(t, reply{503, `{}`}, reply{503, `{}`}, reply{200, cancelled})
	started := time.Now()
	_, err := participants.NewOperations(srv.URL, participants.Config{Token: token}).CancelDeparture(context.Background(), tenant, "SAL-013", "k-0000001")
	if err != nil || srv.count() != 3 {
		t.Fatalf("err %v, requests %d: want three attempts by default", err, srv.count())
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("took %v: the waits are bounded by the base and its doubling", time.Since(started))
	}
}
