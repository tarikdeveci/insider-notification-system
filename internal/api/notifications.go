package api

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
	"github.com/tarikdeveci/insider-notification-system/internal/store"
)

const (
	maxCreateBody  = 64 << 10
	maxKeyLen      = 128
	maxListLimit   = 100
	defaultListLen = 20
)

type listResponse struct {
	Items      []notificationDTO `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// idempotencyKey reads the optional Idempotency-Key header. A missing header is
// ("", true); a repeated, empty, over-long or non 0x21..0x7E value is not ok.
func idempotencyKey(r *http.Request) (string, bool) {
	vals := r.Header.Values("Idempotency-Key")
	switch len(vals) {
	case 0:
		return "", true
	case 1:
	default:
		return "", false
	}
	k := vals[0]
	if k == "" || len(k) > maxKeyLen {
		return "", false
	}
	for i := 0; i < len(k); i++ {
		if k[i] < 0x21 || k[i] > 0x7E {
			return "", false
		}
	}
	return k, true
}

func (s *server) createNotification(w http.ResponseWriter, r *http.Request) {
	key, ok := idempotencyKey(r)
	if !ok {
		writeProblem(w, r, http.StatusBadRequest, "Idempotency-Key must be 1 to 128 printable ASCII characters without spaces", nil)
		return
	}
	var req domain.CreateRequest
	if err := decodeJSON(w, r, &req, maxCreateBody); err != nil {
		writeRequestError(w, r, err)
		return
	}
	nn, errs := req.Validate()
	if len(errs) > 0 {
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", errs)
		return
	}
	if serrs := domain.CheckSchedule(nn.ScheduledAt, s.now()); len(serrs) > 0 {
		s.scheduleRejected(w, r, key, nn.Hash(), serrs)
		return
	}
	res, err := s.Store.CreateNotification(r.Context(), store.CreateParams{
		New:            nn,
		IdempotencyKey: key,
		RequestHash:    nn.Hash(),
		CorrelationID:  observability.CorrelationID(r.Context()),
	})
	switch {
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "Idempotency-Key was already used with a different request", nil)
	case err != nil:
		internalError(w, r, s.Logger, err)
	case res.Replayed:
		writeJSON(w, http.StatusOK, toDTO(res.Notification))
	default:
		s.Metrics.Created(nn.Channel, nn.Priority)
		writeJSON(w, http.StatusCreated, toDTO(res.Notification))
	}
}

// scheduleRejected answers a create whose schedule is out of range. The time
// check applies to new inserts only (spec section 9), so with an
// Idempotency-Key a stored request is replayed (200) or conflicts (409) first.
func (s *server) scheduleRejected(w http.ResponseWriter, r *http.Request, key, hash string, errs []domain.FieldError) {
	if key == "" {
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", errs)
		return
	}
	res, err := s.Store.ResolveReplay(r.Context(), key, hash)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, toDTO(res.Notification))
	case errors.Is(err, store.ErrIdempotencyConflict):
		writeProblem(w, r, http.StatusConflict, "Idempotency-Key was already used with a different request", nil)
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, r, http.StatusUnprocessableEntity, "validation failed", errs)
	default:
		internalError(w, r, s.Logger, err)
	}
}

func (s *server) getNotification(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeProblem(w, r, http.StatusNotFound, "notification not found", nil)
		return
	}
	n, err := s.Store.GetNotification(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "notification not found", nil)
		return
	}
	if err != nil {
		internalError(w, r, s.Logger, err)
		return
	}
	attempts, err := s.Store.ListAttempts(r.Context(), id)
	if err != nil {
		internalError(w, r, s.Logger, err)
		return
	}
	writeJSON(w, http.StatusOK, notificationDetailDTO{notificationDTO: toDTO(n), DeliveryAttempts: toAttemptDTOs(attempts)})
}

func (s *server) cancelNotification(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeProblem(w, r, http.StatusNotFound, "notification not found", nil)
		return
	}
	n, err := s.Store.CancelNotification(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "notification not found", nil)
	case errors.Is(err, store.ErrNotPending):
		writeProblem(w, r, http.StatusConflict, "only pending notifications can be cancelled", nil)
	case err != nil:
		internalError(w, r, s.Logger, err)
	default:
		writeJSON(w, http.StatusOK, toDTO(n))
	}
}

func (s *server) retryNotification(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !domain.IsUUID(id) {
		writeProblem(w, r, http.StatusNotFound, "notification not found", nil)
		return
	}
	n, err := s.Store.RetryNotification(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "notification not found", nil)
	case errors.Is(err, store.ErrNotFailed):
		writeProblem(w, r, http.StatusConflict, "only failed notifications can be retried", nil)
	case err != nil:
		internalError(w, r, s.Logger, err)
	default:
		writeJSON(w, http.StatusOK, toDTO(n))
	}
}

func (s *server) listNotifications(w http.ResponseWriter, r *http.Request) {
	f, errs := parseListQuery(r.URL.Query())
	if len(errs) > 0 {
		writeProblem(w, r, http.StatusBadRequest, "invalid query parameters", errs)
		return
	}
	items, next, err := s.Store.ListNotifications(r.Context(), f)
	if err != nil {
		internalError(w, r, s.Logger, err)
		return
	}
	resp := listResponse{Items: make([]notificationDTO, 0, len(items))}
	for _, n := range items {
		resp.Items = append(resp.Items, toDTO(n))
	}
	if next != nil {
		c := store.EncodeCursor(*next)
		resp.NextCursor = &c
	}
	writeJSON(w, http.StatusOK, resp)
}

// parseListQuery validates the list query parameters, reporting every bad one.
func parseListQuery(q url.Values) (store.ListFilter, []domain.FieldError) {
	var (
		f    store.ListFilter
		errs []domain.FieldError
	)
	bad := func(field, msg string) { errs = append(errs, domain.FieldError{Field: field, Message: msg}) }

	if v := q.Get("status"); v != "" {
		switch st := domain.Status(v); st {
		case domain.StatusPending, domain.StatusProcessing, domain.StatusSent, domain.StatusFailed, domain.StatusCancelled:
			f.Status = &st
		default:
			bad("status", "must be one of pending, processing, sent, failed, cancelled")
		}
	}
	if v := q.Get("channel"); v != "" {
		if ch := domain.Channel(v); ch.Valid() {
			f.Channel = &ch
		} else {
			bad("channel", "must be one of sms, email, push")
		}
	}
	if v := q.Get("batch_id"); v != "" {
		if domain.IsUUID(v) {
			f.BatchID = &v
		} else {
			bad("batch_id", "must be a UUID")
		}
	}
	f.Limit = defaultListLen
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			bad("limit", "must be an integer from 1 to 100")
		} else {
			f.Limit = n
		}
	}
	if v := q.Get("cursor"); v != "" {
		c, err := store.DecodeCursor(v)
		if err != nil {
			bad("cursor", "is not a valid cursor")
		} else {
			f.Cursor = &c
		}
	}
	parseTime := func(field string) *time.Time {
		v := q.Get(field)
		if v == "" {
			return nil
		}
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			bad(field, "must be an RFC 3339 timestamp")
			return nil
		}
		return &t
	}
	f.CreatedFrom, f.CreatedTo = parseTime("created_from"), parseTime("created_to")
	if f.CreatedFrom != nil && f.CreatedTo != nil && f.CreatedFrom.After(*f.CreatedTo) {
		bad("created_from", "must not be after created_to")
	}
	return f, errs
}
