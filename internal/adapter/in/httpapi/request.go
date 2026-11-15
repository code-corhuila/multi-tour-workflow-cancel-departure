package httpapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/code-corhuila/multi-tour-workflow-cancel-departure/internal/domain/saga"
)

const maxBodyBytes = 16 << 10

type startRequest struct {
	DepartureCode string `json:"departureCode"`
}

type fieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// decodeStart reads the body strictly: one JSON object, no field the contract does not declare (the tenant is never read from the body). The
// messages name the field and the rule, never the value.
func decodeStart(r *http.Request, w http.ResponseWriter) (string, []fieldError) {
	var req startRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil || decoder.Decode(&struct{}{}) != io.EOF {
		return "", []fieldError{{"body", "must be one JSON object with only the declared fields"}}
	}
	if !saga.ValidDepartureCode(req.DepartureCode) {
		return "", []fieldError{{"departureCode", "must be 1 to 40 letters, digits, dots, hyphens or underscores"}}
	}
	return req.DepartureCode, nil
}
