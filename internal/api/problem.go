package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

type problem struct {
	Type      string              `json:"type"`
	Title     string              `json:"title"`
	Status    int                 `json:"status"`
	Detail    string              `json:"detail,omitempty"`
	RequestID string              `json:"request_id,omitempty"`
	Errors    []domain.FieldError `json:"errors,omitempty"`
}

// writeProblem writes a problem+json document carrying the correlation id.
func writeProblem(w http.ResponseWriter, r *http.Request, status int, detail string, errs []domain.FieldError) {
	p := problem{
		Type:      "about:blank",
		Title:     http.StatusText(status),
		Status:    status,
		Detail:    detail,
		RequestID: observability.CorrelationID(r.Context()),
		Errors:    errs,
	}
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(p)
}

// writeJSON writes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeRequestError renders a *requestError as a problem; any other error is a
// generic 500 whose text is never sent to the client.
func writeRequestError(w http.ResponseWriter, r *http.Request, err error) {
	var re *requestError
	if errors.As(err, &re) {
		writeProblem(w, r, re.Status, re.Detail, nil)
		return
	}
	writeProblem(w, r, http.StatusInternalServerError, "internal error", nil)
}
