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

	"github.com/tarikdeveci/insider-notification-system/internal/template"
)

// MaxBatchItems is the largest accepted batch.
const MaxBatchItems = 1000

// Template variable limits (spec section 9).
const (
	MaxTemplateVars     = 50
	MaxTemplateVarRunes = 1000
)

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
	// TemplateID and TemplateVars replace Content: the API renders the stored
	// template body with the vars at create time.
	TemplateID   *string           `json:"template_id"`
	TemplateVars map[string]string `json:"template_vars"`
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
	// TemplateID (lowercase UUID) and TemplateVars are set for template
	// requests, whose Content stays empty until the API renders it.
	TemplateID   *string
	TemplateVars map[string]string
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

	var tplID *string
	var tplVars map[string]string
	hasTemplate := r.TemplateID != nil
	switch {
	case hasTemplate && r.Content != "":
		add("template_id", "content and template_id are mutually exclusive")
	case hasTemplate:
	case r.Content == "":
		add("content", "content or template_id is required")
	}
	if hasTemplate {
		if id := strings.ToLower(*r.TemplateID); IsUUID(id) {
			tplID = &id
		} else {
			add("template_id", "must be a UUID")
		}
		tplVars = r.validateVars(add)
	} else if r.TemplateVars != nil {
		add("template_vars", "requires template_id")
	}

	if r.Recipient == "" {
		add("recipient", "is required")
	}
	if !hasTemplate && r.Content != "" {
		if msg := contentProblem(ch, chOK, r.Content); msg != "" {
			add("content", msg)
		}
	}

	if chOK {
		switch ch {
		case ChannelSMS:
			if r.Recipient != "" && !smsRecipient.MatchString(r.Recipient) {
				add("recipient", "must be an E.164 number like +905551234567")
			}
		case ChannelEmail:
			if r.Recipient != "" {
				addr, err := mail.ParseAddress(r.Recipient)
				if err != nil || addr.Address != r.Recipient || len(r.Recipient) > 254 {
					add("recipient", "must be a plain email address of at most 254 characters")
				}
			}
		case ChannelPush:
			if r.Recipient != "" {
				if utf8.RuneCountInString(r.Recipient) > 255 || strings.IndexFunc(r.Recipient, unicode.IsSpace) >= 0 {
					add("recipient", "must be a token of 1 to 255 characters without whitespace")
				}
			}
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
	return NewNotification{Channel: ch, Recipient: r.Recipient, Content: r.Content, Priority: prio, ScheduledAt: scheduled, TemplateID: tplID, TemplateVars: tplVars}, nil
}

// validateVars checks the template_vars bounds and returns the vars to keep
// (nil when empty). Messages never echo a key or a value.
func (r CreateRequest) validateVars(add func(field, msg string)) map[string]string {
	if len(r.TemplateVars) > MaxTemplateVars {
		add("template_vars", "must have at most 50 entries")
	}
	badKey, longValue := false, false
	for k, v := range r.TemplateVars {
		if !template.ValidName(k) {
			badKey = true
		}
		if utf8.RuneCountInString(v) > MaxTemplateVarRunes {
			longValue = true
		}
	}
	if badKey {
		add("template_vars", "names must match [A-Za-z_][A-Za-z0-9_]*")
	}
	if longValue {
		add("template_vars", "values must be at most 1000 characters")
	}
	if len(r.TemplateVars) == 0 {
		return nil
	}
	return r.TemplateVars
}

// contentLimit is the content rune limit of a valid channel.
func contentLimit(ch Channel) int {
	switch ch {
	case ChannelSMS:
		return 160
	case ChannelEmail:
		return 10000
	}
	return 256
}

// contentProblem returns the message for unusable content, or "". The channel
// limit is applied only when the channel is valid.
func contentProblem(ch Channel, chOK bool, content string) string {
	if strings.TrimSpace(content) == "" {
		return "is required"
	}
	if chOK && utf8.RuneCountInString(content) > contentLimit(ch) {
		return "exceeds the limit for this channel"
	}
	return ""
}

// CheckRenderedContent applies the channel content rules to text rendered from
// a template. Errors are on field content with a "rendered content: " prefix.
func CheckRenderedContent(ch Channel, content string) []FieldError {
	if msg := contentProblem(ch, ch.Valid(), content); msg != "" {
		return []FieldError{{Field: "content", Message: "rendered content: " + msg}}
	}
	return nil
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
		// Template fields are omitempty for the same reason. The vars marshal as
		// a JSON object whose keys encoding/json sorts, so map order is stable.
		// The rendered content is never hashed: the API hashes before rendering.
		TemplateID   string            `json:"template_id,omitempty"`
		TemplateVars map[string]string `json:"template_vars,omitempty"`
	}{n.Recipient, string(n.Channel), n.Content, n.Priority.String(), scheduledHash(n.ScheduledAt), derefStr(n.TemplateID), n.TemplateVars})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
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
