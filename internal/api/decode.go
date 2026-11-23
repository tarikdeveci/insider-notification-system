package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// requestError is a client-facing failure with an HTTP status.
type requestError struct {
	Status int
	Detail string
}

func (e *requestError) Error() string { return e.Detail }

// decodeJSON strictly decodes a single JSON object from the body into dst.
// Failures are *requestError with generic details (the body is never echoed).
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any, max int64) error {
	r.Body = http.MaxBytesReader(w, r.Body, max)
	b, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return &requestError{http.StatusRequestEntityTooLarge, "request body too large"}
		}
		return &requestError{http.StatusBadRequest, "could not read request body"}
	}
	b = bytes.TrimSpace(b)
	if len(b) == 0 || b[0] != '{' {
		return &requestError{http.StatusBadRequest, "body must be a JSON object"}
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &requestError{http.StatusBadRequest, "invalid JSON body"}
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return &requestError{http.StatusBadRequest, "unexpected data after JSON object"}
	}
	return nil
}
