package api

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/tarikdeveci/insider-notification-system/internal/domain"
	"github.com/tarikdeveci/insider-notification-system/internal/observability"
)

type notificationDTO struct {
	ID                string     `json:"id"`
	BatchID           *string    `json:"batch_id"`
	Channel           string     `json:"channel"`
	Recipient         string     `json:"recipient"`
	Content           string     `json:"content"`
	Priority          string     `json:"priority"`
	Status            string     `json:"status"`
	Attempts          int        `json:"attempts"`
	MaxAttempts       int        `json:"max_attempts"`
	NextAttemptAt     time.Time  `json:"next_attempt_at"`
	ProviderMessageID *string    `json:"provider_message_id"`
	LastError         *string    `json:"last_error"`
	CorrelationID     string     `json:"correlation_id"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	SentAt            *time.Time `json:"sent_at"`
}

type attemptDTO struct {
	AttemptNo  int        `json:"attempt_no"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	HTTPStatus *int       `json:"http_status"`
	Error      *string    `json:"error"`
	DurationMS *int       `json:"duration_ms"`
}

// notificationDetailDTO is the single-get shape: the notification plus its attempts.
type notificationDetailDTO struct {
	notificationDTO
	DeliveryAttempts []attemptDTO `json:"delivery_attempts"`
}

func toDTO(n domain.Notification) notificationDTO {
	return notificationDTO{
		ID:                n.ID,
		BatchID:           n.BatchID,
		Channel:           string(n.Channel),
		Recipient:         n.Recipient,
		Content:           n.Content,
		Priority:          n.Priority.String(),
		Status:            string(n.Status),
		Attempts:          n.Attempts,
		MaxAttempts:       n.MaxAttempts,
		NextAttemptAt:     n.NextAttemptAt,
		ProviderMessageID: n.ProviderMessageID,
		LastError:         n.LastError,
		CorrelationID:     n.CorrelationID,
		CreatedAt:         n.CreatedAt,
		UpdatedAt:         n.UpdatedAt,
		SentAt:            n.SentAt,
	}
}

func toAttemptDTOs(as []domain.Attempt) []attemptDTO {
	out := make([]attemptDTO, 0, len(as))
	for _, a := range as {
		out = append(out, attemptDTO{
			AttemptNo:  a.AttemptNo,
			StartedAt:  a.StartedAt,
			FinishedAt: a.FinishedAt,
			HTTPStatus: a.HTTPStatus,
			Error:      a.Error,
			DurationMS: a.DurationMS,
		})
	}
	return out
}

// internalError logs an unexpected failure (type and SQLSTATE only, never the
// error text, which can carry row data) and answers with a bare 500 problem.
func internalError(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	args := append([]any{"correlation_id", observability.CorrelationID(r.Context())}, observability.ErrAttrs(err)...)
	log.ErrorContext(r.Context(), "internal error", args...)
	writeProblem(w, r, http.StatusInternalServerError, "", nil)
}
