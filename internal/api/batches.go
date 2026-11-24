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
	hash := domain.BatchHash(items)
	var serrs []domain.FieldError
	now := s.now()
	for i, it := range items {
		for _, fe := range domain.CheckSchedule(it.ScheduledAt, now) {
			idx := i
			fe.Index = &idx
			serrs = append(serrs, fe)
		}
	}
	if len(serrs) > 0 {
		s.batchScheduleRejected(w, r, key, hash, serrs)
		return
	}
	if rerrs, err := s.renderBatch(r.Context(), items); err != nil {
		internalError(w, r, s.Logger, err)
		return
	} else if len(rerrs) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", rerrs)
		return
	}
	res, err := s.Store.CreateBatch(r.Context(), store.CreateBatchParams{
		Items:          items,
		IdempotencyKey: key,
		RequestHash:    hash,
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
		} else {
			for _, it := range items {
				s.Metrics.Created(it.Channel, it.Priority)
			}
		}
		writeJSON(w, code, batchCreatedResponse{BatchID: res.BatchID, NotificationIDs: res.IDs})
	}
}

// batchScheduleRejected is scheduleRejected for batches: a stored batch under
// the same key is replayed or conflicts before the schedule errors are shown.
func (s *server) batchScheduleRejected(w http.ResponseWriter, r *http.Request, key, hash string, errs []domain.FieldError) {
	if key == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", errs)
		return
	}
	res, err := s.Store.ResolveBatchReplay(r.Context(), key, hash)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, batchCreatedResponse{BatchID: res.BatchID, NotificationIDs: res.IDs})
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "Idempotency-Key was already used with a different request", nil)
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", errs)
	default:
		internalError(w, r, s.Logger, err)
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
