package domain

import "time"

// Notification is the read model of a stored notification.
type Notification struct {
	ID                string
	BatchID           *string
	Channel           Channel
	Recipient         string
	Content           string
	Priority          Priority
	Status            Status
	Attempts          int
	MaxAttempts       int
	NextAttemptAt     time.Time
	ScheduledAt       *time.Time
	TemplateID        *string
	TemplateVars      map[string]string
	ProviderMessageID *string
	LastError         *string
	CorrelationID     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	SentAt            *time.Time
}

// Attempt is one delivery attempt of a notification.
type Attempt struct {
	AttemptNo  int
	StartedAt  time.Time
	FinishedAt *time.Time
	HTTPStatus *int
	Error      *string
	DurationMS *int
}

// Template is a reusable message body with {{var}} placeholders.
type Template struct {
	ID        string
	Name      string
	Channel   Channel
	Body      string
	CreatedAt time.Time
}
