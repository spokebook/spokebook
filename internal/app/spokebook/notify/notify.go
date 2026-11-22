// Package notify tells owners about new due maintenance, worn components
// and other events once a day over channels such as ntfy. The kinds of
// channel are interchangeable: each Kind describes its configuration
// fields, checks them and opens the Channel that sends the messages.
// Channel configurations are sealed with APP_SECRET_KEY; without it,
// notifications are disabled.
package notify

import (
	"context"
	"errors"
	"slices"
)

// Message is a notification. Body may have several lines.
type Message struct {
	Title string
	Body  string
	// Priority is 1 (min) to 5 (max), 3 is the default.
	Priority int
	// Tags classify the message, e.g. "warning"; channels may show them as
	// icons or ignore them.
	Tags []string
	// Link is the absolute URL of the page the message is about; empty if
	// none.
	Link string
}

// Priorities of messages.
const (
	PriorityDefault = 3
	PriorityHigh    = 4
)

// Channel sends messages, e.g. to an ntfy topic.
type Channel interface {
	// Send returns an error wrapping ErrUnauthorized, ErrRejected,
	// ErrBlocked or ErrUnreachable if it can tell why sending failed. Errors never
	// contain secrets of the configuration.
	Send(ctx context.Context, m Message) error
}

// Config is the configuration of a channel: values by field name.
type Config map[string]string

// Field is a configuration field of a kind of channel. Label and Hint
// are message IDs.
type Field struct {
	// Name is the key in Config and the name of the form field.
	Name  string
	Label string
	// Hint is a message ID; empty if none.
	Hint string
	// Type is the type of the input, e.g. "url", "text" or "password".
	Type     string
	Required bool
	// Secret fields are never shown again: on edit, an empty value keeps
	// the stored one.
	Secret bool
	// Default is the value of a new channel.
	Default string
}

// FieldError means a field has an invalid value; Msg is the message ID of
// the error.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return "notify: invalid " + e.Field + ": " + e.Msg }

// Kind is a kind of channel, e.g. ntfy. Its name is the message ID
// "notify.kind.<ID>".
type Kind struct {
	ID     string
	Fields []Field
	// Validate checks a configuration and returns it normalized (e.g.
	// trimmed, defaults filled in, unknown fields dropped). Invalid values
	// return a *FieldError.
	Validate func(Config) (Config, error)
	// Open returns the channel of a valid configuration.
	Open func(Config) (Channel, error)
}

// Field returns the field with the name.
func (k Kind) Field(name string) (Field, bool) {
	i := slices.IndexFunc(k.Fields, func(f Field) bool { return f.Name == name })
	if i < 0 {
		return Field{}, false
	}
	return k.Fields[i], true
}

var (
	// ErrDisabled means APP_SECRET_KEY is not set, so no channel can be
	// stored or used.
	ErrDisabled = errors.New("notify: no secret key configured")
	// ErrTooManyChannels means the owner has MaxChannels channels.
	ErrTooManyChannels = errors.New("notify: too many channels")
	// ErrUnknownKind means there is no kind of channel with the ID.
	ErrUnknownKind = errors.New("notify: unknown kind of channel")
	// ErrUnauthorized means the receiver refused the credentials.
	ErrUnauthorized = errors.New("notify: unauthorized")
	// ErrRejected means the receiver refused the message.
	ErrRejected = errors.New("notify: rejected")
	// ErrUnreachable means the receiver could not be reached.
	ErrUnreachable = errors.New("notify: unreachable")
	// ErrBlocked means the receiver's address is not allowed, e.g. a
	// private one without APP_NOTIFY_ALLOW_PRIVATE.
	ErrBlocked = errors.New("notify: address not allowed")
)

// Why sending failed, as stored in the LastError of a channel. The UI
// shows them as message ID "notify.channel_error.<code>".
const (
	ErrorUnauthorized = "unauthorized"
	ErrorRejected     = "rejected"
	ErrorUnreachable  = "unreachable"
	ErrorBlocked      = "blocked"
	// ErrorSecret means the configuration can't be opened, e.g. because
	// APP_SECRET_KEY changed.
	ErrorSecret = "secret"
	ErrorFailed = "failed"
)

// ErrorCode returns the code of an error returned by sending; "" for nil.
func ErrorCode(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, ErrUnauthorized):
		return ErrorUnauthorized
	case errors.Is(err, ErrRejected):
		return ErrorRejected
	case errors.Is(err, ErrBlocked):
		return ErrorBlocked
	case errors.Is(err, ErrUnreachable):
		return ErrorUnreachable
	case errors.Is(err, errSecret):
		return ErrorSecret
	default:
		return ErrorFailed
	}
}

// Events an owner can be notified about.
const (
	// EventMaintenance: a level or a step with its own interval is due
	// soon or overdue.
	EventMaintenance = "maintenance"
	// EventWear: an installed component is close to or at its limit.
	EventWear = "wear"
	// EventGarmin: Garmin Connect needs a new API key.
	EventGarmin = "garmin"
	// EventUnassigned: there are rides without bike.
	EventUnassigned = "unassigned"
	// EventOpenMaintenance: a maintenance is open for longer than the
	// owner's OpenDays.
	EventOpenMaintenance = "open_maintenance"
)

// Events are all events in the order to show them; the name of each is
// the message ID "notify.event.<event>".
var Events = []string{EventMaintenance, EventWear, EventOpenMaintenance, EventUnassigned, EventGarmin}

// Thresholds: from which state on due items and worn components count.
// Their names are the message IDs "notify.threshold.<threshold>".
const (
	ThresholdSoon    = "soon"
	ThresholdOverdue = "overdue"
)

// Thresholds are all thresholds.
var Thresholds = []string{ThresholdSoon, ThresholdOverdue}

// Limits of the settings.
const (
	DefaultOpenDays = 7
	MaxOpenDays     = 365
	DefaultLanguage = "en"
	// MaxChannels is how many channels an owner can have.
	MaxChannels = 10
	maxNameLen  = 100
)
