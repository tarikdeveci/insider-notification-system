package domain

// Channel is a delivery channel.
type Channel string

const (
	ChannelSMS   Channel = "sms"
	ChannelEmail Channel = "email"
	ChannelPush  Channel = "push"
)

// Valid reports whether c is a known channel.
func (c Channel) Valid() bool {
	switch c {
	case ChannelSMS, ChannelEmail, ChannelPush:
		return true
	}
	return false
}

// Priority orders delivery; lower numbers are delivered first.
type Priority int16

const (
	PriorityHigh   Priority = 0
	PriorityNormal Priority = 1
	PriorityLow    Priority = 2
)

// ParsePriority maps an API string to a Priority. The empty string means normal.
func ParsePriority(s string) (Priority, bool) {
	switch s {
	case "", "normal":
		return PriorityNormal, true
	case "high":
		return PriorityHigh, true
	case "low":
		return PriorityLow, true
	}
	return 0, false
}

// String returns the API name of p.
func (p Priority) String() string {
	switch p {
	case PriorityHigh:
		return "high"
	case PriorityLow:
		return "low"
	}
	return "normal"
}

// Status is the lifecycle state of a notification.
type Status string

const (
	StatusPending    Status = "pending"
	StatusProcessing Status = "processing"
	StatusSent       Status = "sent"
	StatusFailed     Status = "failed"
	StatusCancelled  Status = "cancelled"
)

var transitions = map[Status][]Status{
	StatusPending:    {StatusProcessing, StatusCancelled},
	StatusProcessing: {StatusSent, StatusPending, StatusFailed},
	StatusFailed:     {StatusPending},
}

// CanTransition reports whether a status change from to to is allowed.
func CanTransition(from, to Status) bool {
	for _, s := range transitions[from] {
		if s == to {
			return true
		}
	}
	return false
}
