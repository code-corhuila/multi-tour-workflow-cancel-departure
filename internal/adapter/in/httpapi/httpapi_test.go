package httpapi_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/golang-jwt/jwt/v5"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/adapter/in/httpapi"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/in"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/correlation"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	route       = "/api/v1/sagas/cancel-departure"
	sagaID      = "55555555-5555-4555-8555-555555555555"
	tenantID    = "11111111-1111-4111-8111-111111111111"
	otherTenant = "22222222-2222-4222-8222-222222222222"
	reservation = "66666666-6666-4666-8666-666666666661"
	goodKey     = "key-0001-abcdef"
	goodBody    = `{"departureCode":"SAL-013"}`
	issuer      = "multi-tour-security"
	audience    = "multi-tour"
)

var internalRoute = "/api/v1/sagas/internal/tenants/" + tenantID + "/cancel-departure"

var signer = mustKey()

func mustKey() *rsa.PrivateKey {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return key
}

func token(t *testing.T, key *rsa.PrivateKey, claims jwt.MapClaims) string {
	t.Helper()
	signed, err := jwt.NewWithClaims(jwt.SigningMethodRS256, claims).SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return signed
}

func admin(mutate func(jwt.MapClaims)) jwt.MapClaims {
	c := jwt.MapClaims{"sub": "ana", "roles": []string{"TENANT_ADMINISTRATOR"}, "tenantId": tenantID, "iss": issuer, "aud": []string{audience},
		"exp": time.Now().Add(time.Hour).Unix()}
	if mutate != nil {
		mutate(c)
	}
	return c
}

// service is the token of job 3: the role SERVICE and no tenant.
func service(mutate func(jwt.MapClaims)) jwt.MapClaims {
	c := jwt.MapClaims{"sub": "multi-tour-worker-departure-deadlines", "roles": []string{"SERVICE"}, "iss": issuer, "aud": []string{audience},
		"exp": time.Now().Add(time.Hour).Unix()}
	if mutate != nil {
		mutate(c)
	}
	return c
}

type fakeUseCase struct {
	started  saga.Saga
	created  bool
	startErr error
	cmd      in.StartCommand
	startCtx context.Context

	found     saga.Saga
	getErr    error
	getTenant string
}

func (f *fakeUseCase) Start(ctx context.Context, cmd in.StartCommand) (saga.Saga, bool, error) {
	f.cmd, f.startCtx = cmd, ctx
	return f.started, f.created, f.startErr
}

func (f *fakeUseCase) Get(_ context.Context, tenant, _ string) (saga.Saga, error) {
	f.getTenant = tenant
	return f.found, f.getErr
}

func completed() saga.Saga {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	return saga.Saga{ID: sagaID, TenantID: tenantID, DepartureCode: "SAL-013", Status: saga.Completed, CreatedAt: now, UpdatedAt: now,
		Steps:        []saga.Step{saga.CancelDeparture, saga.VoidProposals, saga.OpenEpisodes},
		Reservations: []saga.Reservation{{ID: reservation, State: saga.Opened}}}
}

func newHandler(uc in.CancelDeparture, logs *bytes.Buffer) http.Handler {
	return httpapi.New(uc, httpapi.NewVerifier(&signer.PublicKey, issuer, audience), slog.New(slog.NewJSONHandler(logs, nil)))
}

type call struct {
	method, path, contentType, body, bearer, key, corr string
}

func (c call) send(h http.Handler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
	if c.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.bearer)
	}
	if c.contentType != "" {
		req.Header.Set("Content-Type", c.contentType)
	}
	if c.key != "" {
		req.Header.Set("Idempotency-Key", c.key)
	}
	if c.corr != "" {
		req.Header.Set("X-Correlation-Id", c.corr)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func post(t *testing.T, body string) call {
	return call{http.MethodPost, route, "application/json", body, token(t, signer, admin(nil)), goodKey, ""}
}

// postInternal is job 3 asking for a departure of the tenant of the path.
func postInternal(t *testing.T, body string) call {
	return call{http.MethodPost, internalRoute, "application/json", body, token(t, signer, service(nil)), goodKey, ""}
}

func get(t *testing.T, id string) call {
	return call{method: http.MethodGet, path: route + "/" + id, bearer: token(t, signer, admin(nil))}
}

// Every answer is validated against the published contract, and the last check fails when a response the contract declares was never
// produced: the gate toward qa (ADR-004 D7).
func TestC_every_contract_operation_is_exercised(t *testing.T) {
	ctx := context.Background()
	doc, err := openapi3.NewLoader().LoadFromFile("../../../../openapi/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(ctx); err != nil {
		t.Fatalf("the contract is not valid: %v", err)
	}
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	exercised := map[string]bool{}
	platform := token(t, signer, admin(func(c jwt.MapClaims) { c["roles"] = []string{"PLATFORM_ADMINISTRATOR"} }))

	type scenario struct {
		name string
		c    call
		uc   *fakeUseCase
		want int
	}
	okUC := func(created bool) *fakeUseCase { return &fakeUseCase{started: completed(), created: created, found: completed()} }
	failedSaga := completed()
	failedSaga.Reservations = []saga.Reservation{{ID: reservation, State: saga.EpisodeFailed}}
	failedSaga.End(saga.Failed, saga.OpenEpisodes, saga.ReservationsFailed, failedSaga.UpdatedAt)

	var scenarios []scenario
	for _, p := range []struct {
		name  string
		build func() call
		other func(c *call) // a token the route refuses for its role
	}{
		{"public", func() call { return post(t, goodBody) }, func(c *call) { c.bearer = platform }},
		{"internal", func() call { return postInternal(t, goodBody) }, func(c *call) { c.bearer = token(t, signer, admin(nil)) }},
	} {
		noAuth, forbidden, noKey, shortKey, notJSON, unknown, badType := p.build(), p.build(), p.build(), p.build(), p.build(), p.build(), p.build()
		noAuth.bearer = ""
		p.other(&forbidden)
		noKey.key, shortKey.key = "", "short"
		notJSON.body, unknown.body = `{"departureCode":`, `{"departureCode":"SAL-013","extra":1}`
		badType.contentType = "text/plain"
		scenarios = append(scenarios,
			scenario{p.name + " created", p.build(), &fakeUseCase{started: failedSaga, created: true}, http.StatusCreated},
			scenario{p.name + " repeated key", p.build(), okUC(false), http.StatusOK},
			scenario{p.name + " no token", noAuth, okUC(true), http.StatusUnauthorized},
			scenario{p.name + " wrong role", forbidden, okUC(true), http.StatusForbidden},
			scenario{p.name + " no key", noKey, okUC(true), http.StatusBadRequest},
			scenario{p.name + " short key", shortKey, okUC(true), http.StatusBadRequest},
			scenario{p.name + " malformed json", notJSON, okUC(true), http.StatusBadRequest},
			scenario{p.name + " unknown field", unknown, okUC(true), http.StatusBadRequest},
			scenario{p.name + " wrong content type", badType, okUC(true), http.StatusBadRequest},
			scenario{p.name + " unknown departure", p.build(), &fakeUseCase{startErr: out.ErrNotFound}, http.StatusNotFound},
			scenario{p.name + " use case fails", p.build(), &fakeUseCase{startErr: errors.New("pq: password authentication failed for host=db-internal")}, http.StatusInternalServerError},
		)
	}
	getNoAuth, getForbidden := get(t, sagaID), get(t, sagaID)
	getNoAuth.bearer, getForbidden.bearer = "", platform
	scenarios = append(scenarios,
		scenario{"read", get(t, sagaID), okUC(true), http.StatusOK},
		scenario{"read without token", getNoAuth, okUC(true), http.StatusUnauthorized},
		scenario{"read forbidden", getForbidden, okUC(true), http.StatusForbidden},
		scenario{"read unknown", get(t, sagaID), &fakeUseCase{getErr: out.ErrNotFound}, http.StatusNotFound},
		scenario{"read store failure", get(t, sagaID), &fakeUseCase{getErr: errors.New("connection refused host=db-internal")}, http.StatusInternalServerError},
	)
	for _, s := range scenarios {
		var logs bytes.Buffer
		s.c.corr = "gate-1"
		rec := s.c.send(newHandler(s.uc, &logs))
		if rec.Code != s.want {
			t.Errorf("%s: status %d, want %d (%s)", s.name, rec.Code, s.want, rec.Body)
			continue
		}
		if strings.Contains(rec.Body.String(), "db-internal") {
			t.Errorf("%s: the body leaks internal detail: %s", s.name, rec.Body)
		}
		req := httptest.NewRequest(s.c.method, s.c.path, nil)
		found, params, err := router.FindRoute(req)
		if err != nil {
			t.Fatalf("%s %s is not in the contract: %v", s.c.method, s.c.path, err)
		}
		in := &openapi3filter.ResponseValidationInput{
			RequestValidationInput: &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: found},
			Status:                 rec.Code, Header: rec.Header(), Options: &openapi3filter.Options{IncludeResponseStatus: true},
		}
		in.SetBodyBytes(rec.Body.Bytes())
		if err := openapi3filter.ValidateResponse(ctx, in); err != nil {
			t.Errorf("%s: answer against the contract: %v", s.name, err)
		}
		exercised[s.c.method+" "+found.Path+" "+strconv.Itoa(rec.Code)] = true
	}
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			for status := range operation.Responses.Map() {
				if !exercised[method+" "+path+" "+status] {
					t.Errorf("the response %s of %s %s was never produced", status, method, path)
				}
			}
		}
	}
}

// AC-WF4-01.10: one detail per invalid field, and the saga does not start.
func TestC_a_request_is_refused_for_every_invalid_field(t *testing.T) {
	for name, body := range map[string]string{
		"departureCode empty":     `{"departureCode":""}`,
		"departureCode not plain": `{"departureCode":"../x"}`,
		"departureCode too long":  `{"departureCode":"` + strings.Repeat("A", 41) + `"}`,
		"departureCode missing":   `{}`,
	} {
		uc := &fakeUseCase{started: completed(), created: true}
		rec := post(t, body).send(newHandler(uc, &bytes.Buffer{}))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"field":"departureCode"`) {
			t.Errorf("%s: status %d, body %s", name, rec.Code, rec.Body)
		}
		if uc.startCtx != nil {
			t.Errorf("%s: the saga must not start on an invalid request", name)
		}
	}
}

// AC-WF4-01.9, AC-WF4-01.10: for a person the tenant is the one of the token, never of the body.
func TestC_the_saga_starts_for_the_tenant_of_the_token_with_its_key_and_correlation_id(t *testing.T) {
	uc := &fakeUseCase{started: completed(), created: true}
	body := `{"departureCode":"SAL-013","tenantId":"` + otherTenant + `"}`
	rec := call{http.MethodPost, route, "application/json", body, token(t, signer, admin(nil)), goodKey, "corr-77"}.send(newHandler(uc, &bytes.Buffer{}))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("a body with a tenantId must be refused (unknown field), got %d", rec.Code)
	}
	rec = call{http.MethodPost, route, "application/json", goodBody, token(t, signer, admin(nil)), goodKey, "corr-77"}.send(newHandler(uc, &bytes.Buffer{}))
	if uc.cmd != (in.StartCommand{TenantID: tenantID, IdempotencyKey: goodKey, DepartureCode: "SAL-013"}) {
		t.Errorf("command %+v", uc.cmd)
	}
	if got := correlation.From(uc.startCtx); got != "corr-77" || rec.Header().Get("X-Correlation-Id") != "corr-77" {
		t.Errorf("context id %q, header %q", got, rec.Header().Get("X-Correlation-Id"))
	}
	if loc := rec.Header().Get("Location"); loc != route+"/"+sagaID {
		t.Errorf("Location %q", loc)
	}
}

// AC-WF4-01.9: job 3 has no tenant in its token, so the tenant of its route is the one of the path, even if that tenant is inactive.
func TestC_job_3_starts_the_saga_for_the_tenant_of_the_path(t *testing.T) {
	uc := &fakeUseCase{started: completed(), created: true}
	rec := postInternal(t, goodBody).send(newHandler(uc, &bytes.Buffer{}))
	if rec.Code != http.StatusCreated || uc.cmd != (in.StartCommand{TenantID: tenantID, IdempotencyKey: goodKey, DepartureCode: "SAL-013"}) {
		t.Errorf("status %d, command %+v", rec.Code, uc.cmd)
	}
	if loc := rec.Header().Get("Location"); loc != route+"/"+sagaID {
		t.Errorf("Location %q", loc)
	}
	for _, bad := range []string{"not-an-id", "..", "a%2Fb"} {
		c := postInternal(t, goodBody)
		c.path = "/api/v1/sagas/internal/tenants/" + bad + "/cancel-departure"
		uc := &fakeUseCase{}
		if rec := c.send(newHandler(uc, &bytes.Buffer{})); rec.Code != http.StatusBadRequest || uc.startCtx != nil {
			t.Errorf("tenant %q: status %d, started %v", bad, rec.Code, uc.startCtx != nil)
		}
	}
}

// AC-WF4-01.6 of the story, as the entry shows it.
func TestC_repeated_start_returns_same_saga_200(t *testing.T) {
	rec := post(t, goodBody).send(newHandler(&fakeUseCase{started: completed(), created: false}, &bytes.Buffer{}))
	if rec.Code != http.StatusOK || rec.Header().Get("Location") != "" {
		t.Errorf("status %d, Location %q", rec.Code, rec.Header().Get("Location"))
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["id"] != sagaID {
		t.Errorf("body %s", rec.Body)
	}
}

// Only a Tenant Administrator may use the public routes, with an ordinary token that names its tenant; only the role SERVICE may use the
// internal one; and a token of the other kind is a 403 on each.
func TestC_each_route_lets_in_only_its_own_role(t *testing.T) {
	publicDenied := map[string]jwt.MapClaims{
		"a customer":         admin(func(c jwt.MapClaims) { c["roles"] = []string{"CUSTOMER"} }),
		"a platform admin":   admin(func(c jwt.MapClaims) { c["roles"] = []string{"PLATFORM_ADMINISTRATOR"} }),
		"no roles":           admin(func(c jwt.MapClaims) { delete(c, "roles") }),
		"no tenant":          admin(func(c jwt.MapClaims) { delete(c, "tenantId") }),
		"a tenant not an id": admin(func(c jwt.MapClaims) { c["tenantId"] = "../x" }),
		"a restricted token": admin(func(c jwt.MapClaims) { c["access"] = "restricted" }),
		"a service":          service(nil),
	}
	for name, cl := range publicDenied {
		c := post(t, goodBody)
		c.bearer = token(t, signer, cl)
		uc := &fakeUseCase{started: completed(), created: true}
		if rec := c.send(newHandler(uc, &bytes.Buffer{})); rec.Code != http.StatusForbidden || uc.startCtx != nil {
			t.Errorf("public, %s: status %d, started %v", name, rec.Code, uc.startCtx != nil)
		}
	}
	internalDenied := map[string]jwt.MapClaims{
		"a tenant administrator": admin(nil),
		"a customer":             service(func(c jwt.MapClaims) { c["roles"] = []string{"CUSTOMER"} }),
		"no roles":               service(func(c jwt.MapClaims) { delete(c, "roles") }),
		"a restricted service":   service(func(c jwt.MapClaims) { c["access"] = "restricted" }),
	}
	for name, cl := range internalDenied {
		c := postInternal(t, goodBody)
		c.bearer = token(t, signer, cl)
		uc := &fakeUseCase{started: completed(), created: true}
		if rec := c.send(newHandler(uc, &bytes.Buffer{})); rec.Code != http.StatusForbidden || uc.startCtx != nil {
			t.Errorf("internal, %s: status %d, started %v", name, rec.Code, uc.startCtx != nil)
		}
	}
	bad := map[string]string{
		"expired":        token(t, signer, admin(func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() })),
		"another issuer": token(t, signer, admin(func(c jwt.MapClaims) { c["iss"] = "someone" })),
		"another key":    token(t, mustKey(), admin(nil)),
		"garbage":        "not.a.token",
	}
	for name, tok := range bad {
		for _, c := range []call{post(t, goodBody), postInternal(t, goodBody)} {
			c.bearer = tok
			if rec := c.send(newHandler(&fakeUseCase{}, &bytes.Buffer{})); rec.Code != http.StatusUnauthorized {
				t.Errorf("%s on %s: status %d", name, c.path, rec.Code)
			}
		}
	}
}

// A person reads only the sagas of their tenant.
func TestC_a_saga_is_read_for_the_tenant_of_the_token(t *testing.T) {
	uc := &fakeUseCase{found: completed()}
	if rec := get(t, sagaID).send(newHandler(uc, &bytes.Buffer{})); rec.Code != http.StatusOK || uc.getTenant != tenantID {
		t.Errorf("status %d, tenant %q", rec.Code, uc.getTenant)
	}
	if rec := get(t, "not-an-id").send(newHandler(&fakeUseCase{}, &bytes.Buffer{})); rec.Code != http.StatusNotFound {
		t.Errorf("a bad id: status %d", rec.Code)
	}
}

func TestC_a_request_without_a_usable_correlation_id_gets_a_new_one_and_an_error_carries_it_as_trace_id(t *testing.T) {
	for _, corr := range []string{"", "bad id with spaces"} {
		c := get(t, sagaID)
		c.corr = corr
		rec := c.send(newHandler(&fakeUseCase{found: completed()}, &bytes.Buffer{}))
		if got := rec.Header().Get("X-Correlation-Id"); got == "" || got == corr {
			t.Errorf("sent %q, answered %q", corr, got)
		}
	}
	c := get(t, sagaID)
	c.bearer, c.corr = "", "trace-9"
	rec := c.send(newHandler(&fakeUseCase{}, &bytes.Buffer{}))
	var body struct{ Error, Message, TraceID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusUnauthorized || body.Error != "UNAUTHORIZED" || body.TraceID != "trace-9" {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
}

// A failed saga answers the step, a reason code, how many reservations are handled and the ids of the ones left, for a person to decide; nothing
// a participant said.
func TestC_a_failed_saga_answers_the_step_a_reason_code_the_count_and_the_ids_left(t *testing.T) {
	s := completed()
	s.Steps = []saga.Step{saga.CancelDeparture, saga.VoidProposals}
	s.Reservations = []saga.Reservation{{ID: reservation, State: saga.EpisodeFailed}, {ID: "66666666-6666-4666-8666-666666666662", State: saga.Opened},
		{ID: "66666666-6666-4666-8666-666666666663", State: saga.Pending}}
	s.End(saga.Failed, saga.OpenEpisodes, saga.ReservationsFailed, s.UpdatedAt)
	rec := post(t, goodBody).send(newHandler(&fakeUseCase{started: s, created: true}, &bytes.Buffer{}))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusCreated || body["status"] != "FAILED" || body["failedStep"] != "OPEN_EPISODES" || body["reason"] != "RESERVATIONS_FAILED" ||
		body["departureCode"] != "SAL-013" || body["handledReservations"] != float64(1) {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
	if left, _ := body["reservationsLeft"].([]any); len(left) != 2 || left[0] != reservation {
		t.Errorf("reservationsLeft %v", body["reservationsLeft"])
	}
	if steps, _ := body["completedSteps"].([]any); len(steps) != 2 || steps[1] != "VOID_PROPOSALS" {
		t.Errorf("completedSteps %v", body["completedSteps"])
	}
}

// A refused cancellation: nothing changed, no step ended, and the answer says which step and why.
func TestC_a_refused_cancellation_answers_compensated_with_its_step_and_reason(t *testing.T) {
	s := completed()
	s.Steps, s.Reservations = nil, nil
	s.End(saga.Compensated, saga.CancelDeparture, saga.DepartureRefused, s.UpdatedAt)
	rec := post(t, goodBody).send(newHandler(&fakeUseCase{started: s, created: true}, &bytes.Buffer{}))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusCreated || body["status"] != "COMPENSATED" || body["failedStep"] != "CANCEL_DEPARTURE" || body["reason"] != "DEPARTURE_REFUSED" ||
		body["handledReservations"] != float64(0) || body["reservationsLeft"] != nil {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
}

// A saga left RUNNING by an unclear answer is answered as such, 201: it goes on by itself.
func TestC_a_saga_that_is_still_running_is_answered_as_running(t *testing.T) {
	running := completed()
	running.Status, running.Steps = saga.Running, []saga.Step{saga.CancelDeparture}
	rec := post(t, goodBody).send(newHandler(&fakeUseCase{started: running, created: true}, &bytes.Buffer{}))
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if rec.Code != http.StatusCreated || body["status"] != "RUNNING" || body["failedStep"] != nil || body["reason"] != nil {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
}

func TestC_the_cause_of_a_failure_goes_to_the_log_and_never_to_the_answer(t *testing.T) {
	var logs bytes.Buffer
	rec := post(t, goodBody).send(newHandler(&fakeUseCase{startErr: errors.New("dial tcp 10.0.0.9:5432: password=hunter2")}, &logs))
	if rec.Code != http.StatusInternalServerError || strings.Contains(rec.Body.String(), "hunter2") || strings.Contains(rec.Body.String(), "10.0.0.9") {
		t.Errorf("status %d, body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(logs.String(), "10.0.0.9") {
		t.Error("the cause did not reach the log")
	}
}

func TestC_health_answers_without_a_token(t *testing.T) {
	rec := call{method: http.MethodGet, path: "/health"}.send(newHandler(&fakeUseCase{}, &bytes.Buffer{}))
	if rec.Code != http.StatusOK {
		t.Errorf("status %d", rec.Code)
	}
}
