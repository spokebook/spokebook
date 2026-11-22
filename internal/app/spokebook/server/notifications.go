package server

import (
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/i18n"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

// channelErrors are the codes of notify.ErrorCode, the only ones the
// notifications page shows from its query.
var channelErrors = []string{
	notify.ErrorUnauthorized, notify.ErrorRejected, notify.ErrorUnreachable, notify.ErrorBlocked, notify.ErrorSecret,
	notify.ErrorFailed,
}

// channelRow is a channel in the list of the notifications page.
type channelRow struct {
	store.NotificationChannel
	// Broken means the stored configuration can't be opened.
	Broken bool
	// LastSent is the localized time of the last sending; empty if never.
	LastSent string
}

// fieldError is a field with an invalid value and its translated
// message, for the templates.
type fieldError struct {
	Field string
	Msg   string
}

// asFieldError returns the field error of err translated.
func asFieldError(l *i18n.Localizer, err error) (fieldError, bool) {
	var fe *notify.FieldError
	if !errors.As(err, &fe) {
		return fieldError{}, false
	}
	return fieldError{Field: fe.Field, Msg: l.T(fe.Msg)}, true
}

// localTime formats t as localized date and time of day.
func localTime(l *i18n.Localizer, t time.Time) string {
	at := t.In(time.Local)
	return l.Date(at) + " " + at.Format("15:04")
}

func (s *Server) notificationsPage(w http.ResponseWriter, r *http.Request) {
	s.renderNotifications(w, r, http.StatusOK, nil, fieldError{})
}

// renderNotifications shows the channels and the settings; form is the
// submitted settings form after an error, nil shows the saved settings.
func (s *Server) renderNotifications(w http.ResponseWriter, r *http.Request, status int, form url.Values, ferr fieldError) {
	ctx := r.Context()
	l := i18n.FromContext(ctx)
	channels, err := s.notify.Channels(ctx, ownerID(ctx))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows := make([]channelRow, 0, len(channels))
	for _, c := range channels {
		row := channelRow{NotificationChannel: c}
		if s.notify.Enabled() {
			info, err := s.notify.Channel(ctx, ownerID(ctx), c.ID)
			if err != nil {
				s.fail(w, r, err)
				return
			}
			row.Broken = info.Broken
		}
		if c.LastSentAt != nil {
			row.LastSent = localTime(l, *c.LastSentAt)
		}
		rows = append(rows, row)
	}
	settings, err := s.notify.Settings(ctx, ownerID(ctx), l.Lang())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if form == nil {
		form = url.Values{
			"event":     settings.Events,
			"threshold": {settings.Threshold},
			"open_days": {strconv.Itoa(settings.OpenDays)},
			"language":  {settings.Language},
		}
	}
	q := r.URL.Query()
	data := map[string]any{
		"Enabled": s.notify.Enabled(), "Kinds": s.notify.Kinds(), "Channels": rows,
		"Events": notify.Events, "Thresholds": notify.Thresholds, "Languages": s.notify.Languages(),
		"MaxOpenDays": notify.MaxOpenDays, "Form": form, "FieldError": ferr,
		"Saved": q.Has("saved"), "Full": len(rows) >= notify.MaxChannels, "MaxChannels": notify.MaxChannels,
	}
	if settings.LastRunAt != nil {
		data["LastRun"] = localTime(l, *settings.LastRunAt)
	}
	if id := q.Get("tested"); id != "" {
		if i := slices.IndexFunc(rows, func(c channelRow) bool { return c.ID == id }); i >= 0 {
			data["Tested"] = rows[i].Name
			if code := q.Get("error"); slices.Contains(channelErrors, code) {
				data["TestError"] = code
			}
		}
	}
	s.render(w, r, status, "notifications.html", data)
}

func (s *Server) saveNotificationSettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l := i18n.FromContext(ctx)
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err)
		return
	}
	// An invalid number is 0, which notify refuses.
	days, _ := strconv.Atoi(r.PostFormValue("open_days"))
	err := s.notify.SaveSettings(ctx, store.NotificationSettings{
		OwnerID: ownerID(ctx), Events: r.PostForm["event"], Threshold: r.PostFormValue("threshold"),
		OpenDays: days, Language: r.PostFormValue("language"),
	})
	if fe, ok := asFieldError(l, err); ok {
		s.renderNotifications(w, r, http.StatusUnprocessableEntity, r.PostForm, fe)
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/notifications?saved=1", http.StatusSeeOther)
}

// channelField is a configuration field of the channel form.
type channelField struct {
	notify.Field
	// Value is the value shown; secret fields never have one.
	Value string
	// SecretSet means a secret field has a stored value.
	SecretSet bool
	// Clear means the "remove the stored value" box is checked.
	Clear bool
	Error string
}

// channelForm is what the channel form shows.
type channelForm struct {
	Kind notify.Kind
	// Channel is the channel edited; nil for a new one.
	Channel *notify.ChannelInfo
	Name    string
	Enabled bool
	Fields  []channelField
	// Error is the message of an invalid name.
	NameError string
	// Error is the message of an error of no field.
	Error string
}

// configName is the form name of a configuration field, kept apart from
// the channel's own fields.
func configName(field string) string { return "config." + field }

// newChannelForm returns the form of the kind filled from the values
// (configuration fields under configName); the field error marks the
// invalid one.
func newChannelForm(kind notify.Kind, info *notify.ChannelInfo, form url.Values, ferr fieldError) channelForm {
	f := channelForm{Kind: kind, Channel: info, Name: form.Get("name"), Enabled: form.Get("enabled") != ""}
	if ferr.Field == "name" {
		f.NameError = ferr.Msg
	}
	for _, field := range kind.Fields {
		cf := channelField{Field: field, Clear: slices.Contains(form["clear"], field.Name)}
		if field.Secret {
			cf.SecretSet = info != nil && info.SecretSet[field.Name]
		} else {
			cf.Value = form.Get(configName(field.Name))
		}
		if ferr.Field == field.Name {
			cf.Error = ferr.Msg
		}
		f.Fields = append(f.Fields, cf)
	}
	return f
}

// channelInput reads the submitted channel form of the kind.
func channelInput(r *http.Request, kind notify.Kind) notify.ChannelInput {
	in := notify.ChannelInput{
		Name: r.PostFormValue("name"), Config: notify.Config{}, Enabled: r.PostFormValue("enabled") != "",
		Language: i18n.FromContext(r.Context()).Lang(),
	}
	for _, f := range kind.Fields {
		in.Config[f.Name] = r.PostFormValue(configName(f.Name))
		if f.Secret && slices.Contains(r.PostForm["clear"], f.Name) {
			in.Clear = append(in.Clear, f.Name)
		}
	}
	return in
}

func (s *Server) renderChannelForm(w http.ResponseWriter, r *http.Request, status int, f channelForm) {
	// The form shows the configuration, e.g. the ntfy topic, which works
	// like a password.
	w.Header().Set("Cache-Control", "no-store")
	s.render(w, r, status, "notification_channel.html", map[string]any{"Channel": f, "Kinds": s.notify.Kinds()})
}

// channelKind returns the kind of a new channel from the "kind" parameter;
// with a single kind, that one. ok is false if none is chosen.
func (s *Server) channelKind(r *http.Request) (kind notify.Kind, ok bool) {
	if id := r.FormValue("kind"); id != "" {
		return s.notify.Kind(id)
	}
	if kinds := s.notify.Kinds(); len(kinds) == 1 {
		return kinds[0], true
	}
	return notify.Kind{}, false
}

func (s *Server) newChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.notify.Enabled() {
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
		return
	}
	channels, err := s.notify.Channels(ctx, ownerID(ctx))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if len(channels) >= notify.MaxChannels {
		// The notifications page tells why there is no new one.
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
		return
	}
	kind, ok := s.channelKind(r)
	if !ok {
		if r.URL.Query().Has("kind") || len(s.notify.Kinds()) == 0 {
			s.fail(w, r, store.ErrNotFound)
			return
		}
		// Several kinds: the page offers them to choose from.
		s.renderChannelForm(w, r, http.StatusOK, channelForm{})
		return
	}
	form := url.Values{"enabled": {"1"}}
	for _, f := range kind.Fields {
		if !f.Secret && f.Default != "" {
			form.Set(configName(f.Name), f.Default)
		}
	}
	s.renderChannelForm(w, r, http.StatusOK, newChannelForm(kind, nil, form, fieldError{}))
}

func (s *Server) createChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if !s.notify.Enabled() {
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err)
		return
	}
	kind, ok := s.notify.Kind(r.PostFormValue("kind"))
	if !ok {
		s.fail(w, r, store.ErrNotFound)
		return
	}
	_, err := s.notify.CreateChannel(ctx, ownerID(ctx), kind.ID, channelInput(r, kind))
	s.channelSaved(w, r, kind, nil, err)
}

// channelSaved redirects to the notifications page after a channel was
// saved, or shows the form again with the error.
func (s *Server) channelSaved(w http.ResponseWriter, r *http.Request, kind notify.Kind, info *notify.ChannelInfo, err error) {
	l := i18n.FromContext(r.Context())
	if fe, ok := asFieldError(l, err); ok {
		s.renderChannelForm(w, r, http.StatusUnprocessableEntity, newChannelForm(kind, info, r.PostForm, fe))
		return
	}
	switch {
	case errors.Is(err, notify.ErrTooManyChannels):
		f := newChannelForm(kind, info, r.PostForm, fieldError{})
		f.Error = l.T("notify.error.max_channels", notify.MaxChannels)
		s.renderChannelForm(w, r, http.StatusUnprocessableEntity, f)
	case errors.Is(err, notify.ErrDisabled):
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
	case errors.Is(err, notify.ErrUnknownKind):
		s.fail(w, r, store.ErrNotFound)
	case err != nil:
		s.fail(w, r, err)
	default:
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
	}
}

// channelInfo loads the channel in the path to edit it; ok is false if a
// response went out instead.
func (s *Server) channelInfo(w http.ResponseWriter, r *http.Request) (info notify.ChannelInfo, kind notify.Kind, ok bool) {
	ctx := r.Context()
	info, err := s.notify.Channel(ctx, ownerID(ctx), r.PathValue("id"))
	if errors.Is(err, notify.ErrDisabled) {
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
		return info, kind, false
	}
	if err != nil {
		s.fail(w, r, err)
		return info, kind, false
	}
	if kind, ok = s.notify.Kind(info.Kind); !ok {
		s.fail(w, r, store.ErrNotFound)
		return info, kind, false
	}
	return info, kind, true
}

func (s *Server) editChannel(w http.ResponseWriter, r *http.Request) {
	info, kind, ok := s.channelInfo(w, r)
	if !ok {
		return
	}
	form := url.Values{"name": {info.Name}}
	if info.Enabled {
		form.Set("enabled", "1")
	}
	for _, f := range kind.Fields {
		if f.Secret {
			continue
		}
		v, saved := info.Settings[f.Name]
		if !saved {
			v = f.Default
		}
		form.Set(configName(f.Name), v)
	}
	s.renderChannelForm(w, r, http.StatusOK, newChannelForm(kind, &info, form, fieldError{}))
}

func (s *Server) updateChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	info, kind, ok := s.channelInfo(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		s.fail(w, r, err)
		return
	}
	err := s.notify.UpdateChannel(ctx, ownerID(ctx), info.ID, channelInput(r, kind))
	s.channelSaved(w, r, kind, &info, err)
}

func (s *Server) confirmDeleteChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	c, err := s.store.Notifications().GetNotificationChannel(ctx, ownerID(ctx), r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	cancel := "/notifications"
	if s.notify.Enabled() {
		cancel = "/notifications/channels/" + c.ID + "/edit"
	}
	s.confirm(w, r, i18n.FromContext(ctx).T("notify.channel.delete.confirm", c.Name),
		"/notifications/channels/"+c.ID+"/delete", cancel)
}

func (s *Server) deleteChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.notify.DeleteChannel(ctx, ownerID(ctx), r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/notifications", http.StatusSeeOther)
}

func (s *Server) testChannel(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	err := s.notify.Test(ctx, ownerID(ctx), id)
	switch {
	case errors.Is(err, notify.ErrDisabled):
		http.Redirect(w, r, "/notifications", http.StatusSeeOther)
		return
	case errors.Is(err, store.ErrNotFound):
		s.fail(w, r, err)
		return
	}
	q := url.Values{"tested": {id}}
	if code := notify.ErrorCode(err); code != "" {
		q.Set("error", code)
	}
	http.Redirect(w, r, "/notifications?"+q.Encode(), http.StatusSeeOther)
}
