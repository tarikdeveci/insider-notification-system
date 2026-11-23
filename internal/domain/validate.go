package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/mail"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxBatchItems is the largest accepted batch.
const MaxBatchItems = 1000

// MaxScheduleAhead is how far in the future a notification may be scheduled.
const MaxScheduleAhead = 30 * 24 * time.Hour

var (
	smsRecipient = regexp.MustCompile(`^\+[0-9]{8,15}$`)
	uuidRe       = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
)

// FieldError describes one invalid input field.
type FieldError struct {
	Index   *int   `json:"index,omitempty"`
	Field   string `json:"field"`
	Message string `json:"message"`
}

// CreateRequest is the API payload for one notification.
type CreateRequest struct {
	Recipient string `json:"recipient"`
	Channel   string `json:"channel"`
	Content   string `json:"content"`
	Priority  string `json:"priority"`
	// ScheduledAt is an optional RFC 3339 timestamp; parsed in Validate.
	ScheduledAt *string `json:"scheduled_at"`
}

// BatchRequest is the API payload for a batch.
type BatchRequest struct {
	Notifications []CreateRequest `json:"notifications"`
}

// NewNotification is a validated, normalized create request.
type NewNotification struct {
	Channel   Channel
	Recipient string
	Content   string
	Priority  Priority
	// ScheduledAt is the parsed schedule in UTC, nil when not scheduled.
	ScheduledAt *time.Time
}

// Validate checks r and returns every problem found, not just the first.
func (r CreateRequest) Validate() (NewNotification, []FieldError) {
	var errs []FieldError
	add := func(field, msg string) { errs = append(errs, FieldError{Field: field, Message: msg}) }

	ch := Channel(r.Channel)
	chOK := ch.Valid()
	if !chOK {
		add("channel", "must be one of sms, email, push")
	}
	prio, ok := ParsePriority(r.Priority)
	if !ok {
		add("priority", "must be one of high, normal, low")
	}

	runes := utf8.RuneCountInString(r.Content)
	if r.Recipient == "" {
		add("recipient", "is required")
	}
	if strings.TrimSpace(r.Content) == "" {
		add("content", "is required")
	}

	if chOK {
		maxContent := 0
		switch ch {
		case ChannelSMS:
			maxContent = 160
			if r.Recipient != "" && !smsRecipient.MatchString(r.Recipient) {
				add("recipient", "must be an E.164 number like +905551234567")
			}
		case ChannelEmail:
			maxContent = 10000
			if r.Recipient != "" {
				addr, err := mail.ParseAddress(r.Recipient)
				if err != nil || addr.Address != r.Recipient || len(r.Recipient) > 254 {
					add("recipient", "must be a plain email address of at most 254 characters")
				}
			}
		case ChannelPush:
			maxContent = 256
			if r.Recipient != "" {
				if utf8.RuneCountInString(r.Recipient) > 255 || strings.IndexFunc(r.Recipient, unicode.IsSpace) >= 0 {
					add("recipient", "must be a token of 1 to 255 characters without whitespace")
				}
			}
		}
		if strings.TrimSpace(r.Content) != "" && runes > maxContent {
			add("content", "exceeds the limit for this channel")
		}
	}

	var scheduled *time.Time
	if r.ScheduledAt != nil {
		t, err := time.Parse(time.RFC3339, *r.ScheduledAt)
		if err != nil {
			add("scheduled_at", "must be an RFC 3339 timestamp like 2026-12-01T09:00:00Z")
		} else {
			t = t.UTC()
			scheduled = &t
		}
	}

	if len(errs) > 0 {
		return NewNotification{}, errs
	}
	return NewNotification{Channel: ch, Recipient: r.Recipient, Content: r.Content, Priority: prio, ScheduledAt: scheduled}, nil
}

// CheckSchedule is the time sensitive part of schedule validation: at must be
// strictly after now and at most MaxScheduleAhead ahead. A nil at is valid.
// It is applied only to new inserts, never to idempotent replays.
func CheckSchedule(at *time.Time, now time.Time) []FieldError {
	if at == nil {
		return nil
	}
	if !at.After(now) {
		return []FieldError{{Field: "scheduled_at", Message: "must be in the future"}}
	}
	if at.After(now.Add(MaxScheduleAhead)) {
		return []FieldError{{Field: "scheduled_at", Message: "must be at most 30 days ahead"}}
	}
	return nil
}

// Hash is a stable SHA-256 hex digest of the normalized notification.
func (n NewNotification) Hash() string {
	b, _ := json.Marshal(struct {
		Recipient string `json:"recipient"`
		Channel   string `json:"channel"`
		Content   string `json:"content"`
		Priority  string `json:"priority"`
		// omitempty keeps the hash of an unscheduled request unchanged (PF17).
		ScheduledAt string `json:"scheduled_at,omitempty"`
	}{n.Recipient, string(n.Channel), n.Content, n.Priority.String(), scheduledHash(n.ScheduledAt)})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func scheduledHash(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// ValidateBatch validates every item. Items are returned only when all are valid.
func ValidateBatch(r BatchRequest) ([]NewNotification, []FieldError) {
	if n := len(r.Notifications); n < 1 || n > MaxBatchItems {
		return nil, []FieldError{{Field: "notifications", Message: "must contain 1 to 1000 items"}}
	}
	var errs []FieldError
	items := make([]NewNotification, 0, len(r.Notifications))
	for i, req := range r.Notifications {
		item, ferrs := req.Validate()
		for _, fe := range ferrs {
			idx := i
			fe.Index = &idx
			errs = append(errs, fe)
		}
		items = append(items, item)
	}
	if len(errs) > 0 {
		return nil, errs
	}
	return items, nil
}

// BatchHash is an order-sensitive digest of a batch.
func BatchHash(items []NewNotification) string {
	hs := make([]string, len(items))
	for i, it := range items {
		hs[i] = it.Hash()
	}
	sum := sha256.Sum256([]byte(strings.Join(hs, "\n")))
	return hex.EncodeToString(sum[:])
}

// IsUUID reports whether s is a canonical textual UUID.
func IsUUID(s string) bool { return uuidRe.MatchString(s) }
