package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"golang.org/x/text/language"

	"github.com/spokebook/spokebook/internal/app/spokebook/i18n"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
	"github.com/spokebook/spokebook/web"
)

// errSecret wraps errors opening a configuration.
var errSecret = errors.New("notify: cannot open configuration")

// Options configure a Service.
type Options struct {
	// BaseURL is the address users open, for the links in messages.
	BaseURL string
	// Bundle translates the messages; nil loads the embedded catalogs.
	Bundle *i18n.Bundle
	// Kinds are the kinds of channel, e.g. ntfy.Kind(...).
	Kinds []Kind
	Log   *slog.Logger
	// Now returns the current time; nil is time.Now.
	Now func() time.Time
}

// Service manages the channels and settings of owners and notifies them.
type Service struct {
	st      store.Store
	box     *secret.Box
	baseURL string
	bundle  *i18n.Bundle
	kinds   []Kind
	log     *slog.Logger
	now     func() time.Time
}

var (
	// locksMu guards locks.
	locksMu sync.Mutex
	// locks serialize the checks of each owner, so no message goes out
	// twice, and keep a backup import from running during a check.
	locks = map[string]*sync.Mutex{}
)

// LockOwner locks the notification checks of the owner and returns the
// unlock function. A backup import holds it, so a running check doesn't
// store states of the replaced data.
func LockOwner(ownerID string) func() {
	locksMu.Lock()
	m, ok := locks[ownerID]
	if !ok {
		m = &sync.Mutex{}
		locks[ownerID] = m
	}
	locksMu.Unlock()
	m.Lock()
	return m.Unlock
}

// New returns a service; without box (no APP_SECRET_KEY) it is disabled.
func New(st store.Store, box *secret.Box, opts Options) *Service {
	s := &Service{
		st: st, box: box, baseURL: strings.TrimSuffix(opts.BaseURL, "/"), bundle: opts.Bundle,
		kinds: opts.Kinds, log: opts.Log, now: opts.Now,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.now == nil {
		s.now = time.Now
	}
	if s.bundle == nil {
		s.bundle = embedded()
	}
	return s
}

// embedded returns the embedded catalogs.
var embedded = sync.OnceValue(func() *i18n.Bundle {
	b, err := i18n.Load(web.Locales, "locales")
	if err != nil {
		panic(err) // the catalogs are embedded
	}
	return b
})

// Languages returns the languages of the embedded catalogs, e.g. "en".
func Languages() []string { return languages(embedded()) }

func languages(b *i18n.Bundle) []string {
	var out []string
	for _, t := range b.Languages() {
		out = append(out, t.String())
	}
	return out
}

// Enabled reports whether channels can be stored and used.
func (s *Service) Enabled() bool { return s != nil && s.box != nil }

// Kinds returns the kinds of channel.
func (s *Service) Kinds() []Kind { return s.kinds }

// Kind returns the kind with the ID.
func (s *Service) Kind(id string) (Kind, bool) {
	i := slices.IndexFunc(s.kinds, func(k Kind) bool { return k.ID == id })
	if i < 0 {
		return Kind{}, false
	}
	return s.kinds[i], true
}

// Languages returns the languages messages can be sent in, e.g. "en".
func (s *Service) Languages() []string { return languages(s.bundle) }

func purpose(ownerID, channelID string) string { return ownerID + "/notify/" + channelID }

func (s *Service) seal(ownerID, channelID string, cfg Config) ([]byte, error) {
	plain, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	return s.box.Seal(plain, purpose(ownerID, channelID)), nil
}

func (s *Service) open(c store.NotificationChannel) (Config, error) {
	plain, err := s.box.Open(c.Config, purpose(c.OwnerID, c.ID))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSecret, err)
	}
	var cfg Config
	if err := json.Unmarshal(plain, &cfg); err != nil {
		return nil, fmt.Errorf("%w: %w", errSecret, err)
	}
	return cfg, nil
}

// Channels returns the owner's channels, oldest first, without their
// configuration. It works without APP_SECRET_KEY.
func (s *Service) Channels(ctx context.Context, ownerID string) ([]store.NotificationChannel, error) {
	list, err := s.st.Notifications().ListNotificationChannels(ctx, ownerID)
	for i := range list {
		list[i].Config = nil
	}
	return list, err
}

// ChannelInfo is a channel with its configuration to edit it.
type ChannelInfo struct {
	// NotificationChannel has no Config.
	store.NotificationChannel
	// Settings is the configuration without secret fields.
	Settings Config
	// SecretSet tells which secret fields have a stored value.
	SecretSet map[string]bool
	// Broken means the stored configuration can't be opened (e.g.
	// APP_SECRET_KEY changed); it must be entered again.
	Broken bool
}

// Channel returns a channel of the owner to edit.
func (s *Service) Channel(ctx context.Context, ownerID, id string) (ChannelInfo, error) {
	if !s.Enabled() {
		return ChannelInfo{}, ErrDisabled
	}
	c, err := s.st.Notifications().GetNotificationChannel(ctx, ownerID, id)
	if err != nil {
		return ChannelInfo{}, err
	}
	info := ChannelInfo{Settings: Config{}, SecretSet: map[string]bool{}}
	cfg, err := s.open(c)
	c.Config = nil
	info.NotificationChannel = c
	if err != nil {
		info.Broken = true
		return info, nil
	}
	kind, _ := s.Kind(c.Kind)
	for k, v := range cfg {
		if f, ok := kind.Field(k); ok && f.Secret {
			info.SecretSet[k] = v != ""
			continue
		}
		info.Settings[k] = v
	}
	return info, nil
}

// ChannelInput is what the owner enters for a channel.
type ChannelInput struct {
	Name    string
	Config  Config
	Enabled bool
	// Clear names secret fields whose stored value is removed; otherwise
	// an empty secret field keeps it.
	Clear []string
	// Language is the language of the messages if the owner has no
	// settings yet, e.g. the browser's; CreateChannel saves the default
	// settings with it. Empty or unknown is DefaultLanguage.
	Language string
}

// checkName returns the trimmed name.
func checkName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > maxNameLen {
		return "", &FieldError{Field: "name", Msg: "notify.error.name"}
	}
	return name, nil
}

// CreateChannel checks and stores a new channel of the kind and returns
// its ID. Invalid input returns a *FieldError, an owner with MaxChannels
// channels ErrTooManyChannels.
func (s *Service) CreateChannel(ctx context.Context, ownerID, kindID string, in ChannelInput) (string, error) {
	if !s.Enabled() {
		return "", ErrDisabled
	}
	kind, ok := s.Kind(kindID)
	if !ok {
		return "", ErrUnknownKind
	}
	name, err := checkName(in.Name)
	if err != nil {
		return "", err
	}
	cfg, err := kind.Validate(in.Config)
	if err != nil {
		return "", err
	}
	id := uuid.Must(uuid.NewV7()).String()
	sealed, err := s.seal(ownerID, id, cfg)
	if err != nil {
		return "", err
	}
	return id, s.st.WithTx(ctx, func(tx store.Stores) error {
		ns := tx.Notifications()
		if _, err := ns.GetNotificationSettings(ctx, ownerID); errors.Is(err, store.ErrNotFound) {
			if err := ns.SaveNotificationSettings(ctx, s.defaults(ownerID, in.Language)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		list, err := ns.ListNotificationChannels(ctx, ownerID)
		if err != nil {
			return err
		}
		if len(list) >= MaxChannels {
			return ErrTooManyChannels
		}
		return ns.CreateNotificationChannel(ctx, store.NotificationChannel{
			ID: id, OwnerID: ownerID, Kind: kind.ID, Name: name, Config: sealed, Enabled: in.Enabled,
			CreatedAt: s.now().UTC(),
		})
	})
}

// UpdateChannel checks and stores the changed channel. Empty secret
// fields keep their stored value unless named in Clear. Invalid input
// returns a *FieldError.
func (s *Service) UpdateChannel(ctx context.Context, ownerID, id string, in ChannelInput) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	return s.st.WithTx(ctx, func(tx store.Stores) error {
		c, err := tx.Notifications().GetNotificationChannel(ctx, ownerID, id)
		if err != nil {
			return err
		}
		kind, ok := s.Kind(c.Kind)
		if !ok {
			return ErrUnknownKind
		}
		if c.Name, err = checkName(in.Name); err != nil {
			return err
		}
		// A broken configuration has no values to keep.
		old, _ := s.open(c)
		cfg := Config{}
		for k, v := range in.Config {
			cfg[k] = v
		}
		for _, f := range kind.Fields {
			if f.Secret && cfg[f.Name] == "" && !slices.Contains(in.Clear, f.Name) {
				cfg[f.Name] = old[f.Name]
			}
		}
		if cfg, err = kind.Validate(cfg); err != nil {
			return err
		}
		if c.Config, err = s.seal(ownerID, id, cfg); err != nil {
			return err
		}
		c.Enabled = in.Enabled
		if err := tx.Notifications().UpdateNotificationChannel(ctx, c); err != nil {
			return err
		}
		return forgetIfSilent(ctx, tx.Notifications(), ownerID)
	})
}

// DeleteChannel deletes a channel; it works without APP_SECRET_KEY.
func (s *Service) DeleteChannel(ctx context.Context, ownerID, id string) error {
	return s.st.WithTx(ctx, func(tx store.Stores) error {
		if err := tx.Notifications().DeleteNotificationChannel(ctx, ownerID, id); err != nil {
			return err
		}
		return forgetIfSilent(ctx, tx.Notifications(), ownerID)
	})
}

// forgetIfSilent deletes the owner's notification states if no channel is
// enabled: nothing is checked while so, and once a channel is enabled
// again, the conditions of then are reported fresh.
func forgetIfSilent(ctx context.Context, ns store.NotificationStore, ownerID string) error {
	list, err := ns.ListNotificationChannels(ctx, ownerID)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(list, func(c store.NotificationChannel) bool { return c.Enabled }) {
		return nil
	}
	return ns.DeleteNotificationStates(ctx, ownerID)
}

// Test sends a test message on the channel (also a disabled one) in the
// language of the settings and records the result on the channel. The
// error tells why sending failed, see ErrorCode.
func (s *Service) Test(ctx context.Context, ownerID, id string) error {
	if !s.Enabled() {
		return ErrDisabled
	}
	c, err := s.st.Notifications().GetNotificationChannel(ctx, ownerID, id)
	if err != nil {
		return err
	}
	settings, err := s.Settings(ctx, ownerID, "")
	if err != nil {
		return err
	}
	l := s.localizer(settings.Language)
	m := Message{
		Title: l.T("notify.test.title"), Body: l.T("notify.test.body"), Priority: PriorityDefault,
		Tags: []string{"bike"}, Link: s.baseURL + "/",
	}
	sendErr := s.send(ctx, c, []Message{m})
	if err := s.record(ctx, c, sendErr, sendErr == nil); err != nil {
		return err
	}
	return sendErr
}

// send opens the channel and sends the messages; it stops at the first
// error.
func (s *Service) send(ctx context.Context, c store.NotificationChannel, msgs []Message) error {
	kind, ok := s.Kind(c.Kind)
	if !ok {
		return ErrUnknownKind
	}
	cfg, err := s.open(c)
	if err != nil {
		return err
	}
	ch, err := kind.Open(cfg)
	if err != nil {
		return err
	}
	for _, m := range msgs {
		if err := ch.Send(ctx, m); err != nil {
			return err
		}
	}
	return nil
}

// record stores how sending on the channel went.
func (s *Service) record(ctx context.Context, c store.NotificationChannel, err error, sent bool) error {
	var at *time.Time
	if sent {
		now := s.now().UTC()
		at = &now
	}
	if err != nil {
		// The channel's errors don't contain secrets.
		s.log.Warn("notify: sending failed", "user", c.OwnerID, "channel", c.ID, "kind", c.Kind, "err", err)
	}
	return s.st.Notifications().SetNotificationChannelResult(ctx, c.OwnerID, c.ID, ErrorCode(err), at)
}

// Defaults returns the settings of an owner who saved none.
func Defaults(ownerID string) store.NotificationSettings {
	return store.NotificationSettings{
		OwnerID: ownerID, Events: []string{EventMaintenance, EventWear}, Threshold: ThresholdSoon,
		OpenDays: DefaultOpenDays, Language: DefaultLanguage,
	}
}

// defaults returns the default settings with the language lang if
// messages can be sent in it.
func (s *Service) defaults(ownerID, lang string) store.NotificationSettings {
	ns := Defaults(ownerID)
	if slices.Contains(s.Languages(), lang) {
		ns.Language = lang
	}
	return ns
}

// Settings returns the owner's settings, or the defaults with language
// lang (e.g. the browser's; empty or unknown is DefaultLanguage) if none
// are saved. It works without APP_SECRET_KEY.
func (s *Service) Settings(ctx context.Context, ownerID, lang string) (store.NotificationSettings, error) {
	ns, err := s.st.Notifications().GetNotificationSettings(ctx, ownerID)
	if errors.Is(err, store.ErrNotFound) {
		return s.defaults(ownerID, lang), nil
	}
	return ns, err
}

// SaveSettings checks and stores the owner's settings (see
// CheckSettings) and returns a *FieldError ("language") for a language
// without catalog. It works without APP_SECRET_KEY.
func (s *Service) SaveSettings(ctx context.Context, ns store.NotificationSettings) error {
	ns, err := CheckSettings(ns)
	if err != nil {
		return err
	}
	if !slices.Contains(s.Languages(), ns.Language) {
		return &FieldError{Field: "language", Msg: "notify.error.language"}
	}
	return s.st.Notifications().SaveNotificationSettings(ctx, ns)
}

// CheckSettings drops unknown events and returns a *FieldError
// ("threshold", "open_days", "language") for an invalid threshold, number
// of days or language tag. LastRunAt is ignored when saving.
func CheckSettings(ns store.NotificationSettings) (store.NotificationSettings, error) {
	var events []string
	for _, e := range Events {
		if slices.Contains(ns.Events, e) {
			events = append(events, e)
		}
	}
	ns.Events = events
	switch {
	case !slices.Contains(Thresholds, ns.Threshold):
		return ns, &FieldError{Field: "threshold", Msg: "notify.error.threshold"}
	case ns.OpenDays < 1 || ns.OpenDays > MaxOpenDays:
		return ns, &FieldError{Field: "open_days", Msg: "notify.error.open_days"}
	}
	if t, err := language.Parse(ns.Language); err != nil || len(ns.Language) > 35 || t.String() != ns.Language {
		return ns, &FieldError{Field: "language", Msg: "notify.error.language"}
	}
	return ns, nil
}

// localizer returns the localizer of the language.
func (s *Service) localizer(lang string) *i18n.Localizer { return s.bundle.Localizer(lang) }
