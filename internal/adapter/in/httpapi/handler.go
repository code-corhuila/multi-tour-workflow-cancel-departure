// Package httpapi is the entry of the saga that cancels a departure: the Tenant Administrator starts it and reads its state, and job 3 starts
// it for a tenant that may be inactive through an internal route (openapi/openapi.yaml). Every error uses the single envelope and never carries
// the text of a participant or of the database; the cause stays in the log under the trace id.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/in"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/application/port/out"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/correlation"
	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const (
	route         = "/api/v1/sagas/cancel-departure"
	internalRoute = "/api/v1/sagas/internal/tenants/{tenantId}/cancel-departure"
	minKeyLength  = 8
	maxKeyLength  = 128
	correlationHd = "X-Correlation-Id"
)

type handler struct {
	uc       in.CancelDeparture
	verifier *Verifier
	log      *slog.Logger
	// starters are the subjects of the service tokens that may start the saga through the internal route (plan D-11): cancelling a
	// departure cannot be undone, so being the role SERVICE is not enough.
	starters []string
}

// New returns the whole inbound side: the routes of the saga and /health. Only the services named in starters may use the internal route.
func New(uc in.CancelDeparture, verifier *Verifier, log *slog.Logger, starters []string) http.Handler {
	h := &handler{uc, verifier, log, starters}
	mux := http.NewServeMux()
	mux.HandleFunc(route, h.collection)
	mux.HandleFunc(route+"/{id}", h.item)
	mux.HandleFunc(internalRoute, h.internal)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "route not found")
	})
	return h.withCorrelation(h.recovering(mux))
}

// recovering turns a panic below it into the generic 500 envelope, logged under the trace id.
func (h *handler) recovering(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recovered := recover(); recovered != nil {
				h.log.Error("entry route panicked", "traceId", correlation.From(r.Context()), "panic", fmt.Sprint(recovered))
				writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// withCorrelation reuses a well-formed X-Correlation-Id or generates one, returns it and puts it in the context.
func (h *handler) withCorrelation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(correlationHd)
		if !correlation.Valid(id) {
			id = correlation.NewID()
		}
		w.Header().Set(correlationHd, id)
		next.ServeHTTP(w, r.WithContext(correlation.With(r.Context(), id)))
	})
}

// authorize answers 401 or 403 itself and reports whether the caller has the role the route asks for, with the claims.
func (h *handler) authorize(w http.ResponseWriter, r *http.Request, allowed func(Claims) bool, who string) (Claims, bool) {
	scheme, raw, found := strings.Cut(r.Header.Get("Authorization"), " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(raw) == "" {
		return Claims{}, deny(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	}
	claims, err := h.verifier.Verify(strings.TrimSpace(raw))
	if err != nil {
		h.log.Debug("token refused", "traceId", correlation.From(r.Context()), "reason", err.Error())
		return Claims{}, deny(w, r, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
	}
	if !allowed(claims) {
		return Claims{}, deny(w, r, http.StatusForbidden, "FORBIDDEN", "only "+who+" may use this route")
	}
	return claims, true
}

func deny(w http.ResponseWriter, r *http.Request, status int, code, message string) bool {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", "Bearer")
	}
	writeError(w, r, status, code, message)
	return false
}

// collection is the start of the Tenant Administrator: the tenant is the one of the token.
func (h *handler) collection(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	claims, ok := h.authorize(w, r, Claims.isTenantAdministrator, "the Tenant Administrator")
	if !ok {
		return
	}
	h.start(w, r, claims.TenantID)
}

// internal is the start of job 3: the role SERVICE with a subject listed as a starter, and the tenant of the path, which may be inactive.
func (h *handler) internal(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	if _, ok := h.authorize(w, r, h.isStarter, "a service allowed to start it"); !ok {
		return
	}
	tenantID := r.PathValue("tenantId")
	if !saga.ValidID(tenantID) {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "the tenant is not valid", fieldError{"tenantId", "must be an id"})
		return
	}
	h.start(w, r, tenantID)
}

func (h *handler) start(w http.ResponseWriter, r *http.Request, tenantID string) {
	key := r.Header.Get("Idempotency-Key")
	if n := utf8.RuneCountInString(key); !utf8.ValidString(key) || n < minKeyLength || n > maxKeyLength {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key is required, 8 to 128 characters",
			fieldError{"Idempotency-Key", "must be 8 to 128 characters"})
		return
	}
	if mediaType, _, _ := strings.Cut(r.Header.Get("Content-Type"), ";"); strings.TrimSpace(mediaType) != "application/json" {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "Content-Type must be application/json")
		return
	}
	code, problems := decodeStart(r, w)
	if problems != nil {
		writeError(w, r, http.StatusBadRequest, "VALIDATION_ERROR", "the request is not valid", problems...)
		return
	}
	started, created, err := h.uc.Start(r.Context(), in.StartCommand{TenantID: tenantID, IdempotencyKey: key, DepartureCode: code})
	switch {
	case errors.Is(err, out.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "departure not found")
		return
	case err != nil:
		h.fail(w, r, "start the saga", err)
		return
	case !created:
		writeJSON(w, http.StatusOK, toResponse(started))
		return
	}
	w.Header().Set("Location", route+"/"+started.ID)
	writeJSON(w, http.StatusCreated, toResponse(started))
}

func (h *handler) item(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeError(w, r, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
		return
	}
	claims, ok := h.authorize(w, r, Claims.isTenantAdministrator, "the Tenant Administrator")
	if !ok {
		return
	}
	id := r.PathValue("id")
	if !saga.ValidID(id) {
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "saga not found")
		return
	}
	found, err := h.uc.Get(r.Context(), claims.TenantID, id)
	switch {
	case errors.Is(err, out.ErrNotFound):
		writeError(w, r, http.StatusNotFound, "NOT_FOUND", "saga not found")
	case err != nil:
		h.fail(w, r, "read the saga", err)
	default:
		writeJSON(w, http.StatusOK, toResponse(found))
	}
}

// fail logs the cause under the trace id and answers a generic 500: no driver text, host or trace leaves.
func (h *handler) fail(w http.ResponseWriter, r *http.Request, what string, err error) {
	h.log.Error("entry route failed", "traceId", correlation.From(r.Context()), "action", what, "error", err.Error())
	writeError(w, r, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
}

type errorResponse struct {
	Error   string       `json:"error"`
	Message string       `json:"message"`
	Details []fieldError `json:"details,omitempty"`
	TraceID string       `json:"traceId"`
}

func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string, details ...fieldError) {
	writeJSON(w, status, errorResponse{code, message, details, correlation.From(r.Context())})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// sagaResponse is the Saga of the contract. It has no field for the text of a participant on purpose: only a short reason code, a count and the
// ids of the reservations left, for a person to decide.
type sagaResponse struct {
	ID                  string    `json:"id"`
	Status              string    `json:"status"`
	DepartureCode       string    `json:"departureCode"`
	CompletedSteps      []string  `json:"completedSteps"`
	FailedStep          string    `json:"failedStep,omitempty"`
	Reason              string    `json:"reason,omitempty"`
	HandledReservations int       `json:"handledReservations"`
	ReservationsLeft    []string  `json:"reservationsLeft,omitempty"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

func toResponse(s saga.Saga) sagaResponse {
	steps := make([]string, 0, len(s.Steps))
	for _, st := range s.Steps {
		steps = append(steps, string(st))
	}
	return sagaResponse{ID: s.ID, Status: string(s.Status), DepartureCode: s.DepartureCode, CompletedSteps: steps, FailedStep: string(s.FailedStep),
		Reason: string(s.Reason), HandledReservations: s.Handled(), ReservationsLeft: s.Left(), CreatedAt: s.CreatedAt.UTC(), UpdatedAt: s.UpdatedAt.UTC()}
}

// isStarter is true for a service token whose subject is one of the starters.
func (h *handler) isStarter(c Claims) bool {
	return c.isService() && slices.Contains(h.starters, c.Subject)
}
