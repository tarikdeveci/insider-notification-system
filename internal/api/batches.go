package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

// maxBatchBody caps a batch request body (1000 items fit well under it).
const maxBatchBody = 8 << 20

type batchCreatedResponse struct {
	BatchID         string   `json:"batch_id"`
	NotificationIDs []string `json:"notification_ids"`
}

type batchSummaryResponse struct {
	ID        string         `json:"id"`
	Total     int            `json:"total"`
	CreatedAt time.Time      `json:"created_at"`
	Counts    map[string]int `json:"counts"`
}

func (s *server) createBatch(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "Idempotency-Key must be 1 to 128 printable ASCII characters without spaces", nil)
		return
	}
	var req domain.BatchRequest
	if err := decodeJSON(w, r, &req, maxBatchBody); err != nil {
		writeRequestError(w, r, err)
		return
	}
	items, errs := domain.ValidateBatch(req)
	if len(errs) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", errs)
		return
	}
	res, err := s.Store.CreateBatch(r.Context(), store.CreateBatchParams{
		Items:          items,
		IdempotencyKey: key,
		RequestHash:    domain.BatchHash(items),
		CorrelationID:  observability.CorrelationID(r.Context()),
	})
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "Idempotency-Key was already used with a different request", nil)
	case err != nil:
		internalError(w, r, s.Logger, err)
	default:
		code := http.StatusCreated
		if res.Replayed {
			code = http.StatusOK
		}
		writeJSON(w, code, batchCreatedResponse{BatchID: res.BatchID, NotificationIDs: res.IDs})
	}
}

func (s *server) getBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeProblem(w, r, http.StatusNotFound, "batch not found", nil)
		return
	}
	sum, err := s.Store.GetBatch(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "batch not found", nil)
		return
	}
	if err != nil {
		internalError(w, r, s.Logger, err)
		return
	}
	counts := make(map[string]int, len(sum.Counts))
	for st, c := range sum.Counts {
		counts[string(st)] = c
	}
	writeJSON(w, http.StatusOK, batchSummaryResponse{ID: sum.ID, Total: sum.Total, CreatedAt: sum.CreatedAt, Counts: counts})
}

func (s *server) cancelBatch(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeProblem(w, r, http.StatusNotFound, "batch not found", nil)
		return
	}
	n, err := s.Store.CancelBatch(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "batch not found", nil)
		return
	}
	if err != nil {
		internalError(w, r, s.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"cancelled": n})
}
