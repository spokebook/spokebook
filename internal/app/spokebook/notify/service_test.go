package notify_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/i18n"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify/ntfy"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
	"github.com/spokebook/spokebook/internal/app/spokebook/store/sqlite"
	"github.com/spokebook/spokebook/web"
)

// ntfyServer records requests and answers with the status.
type ntfyServer struct {
	*httptest.Server
	mu     sync.Mutex
	status int
	reqs   []*http.Request
	bodies []string
	loc    string
}

func newNtfyServer(t *testing.T) *ntfyServer {
	t.Helper()
	s := &ntfyServer{status: http.StatusOK}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, r)
		s.bodies = append(s.bodies, string(b))
		if s.loc != "" {
			w.Header().Set("Location", s.loc)
		}
		w.WriteHeader(s.status)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *ntfyServer) setStatus(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.status = status
}

func (s *ntfyServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

// ntfyEnv is a service with the ntfy kind and a clock.
type ntfyEnv struct {
	*env
	srv *ntfyServer
}

const (
	secretTopic = "mytopic-7f3a9c1e"
	secretToken = "tk_s3cr3tT0ken"
)

func newNtfyEnv(t *testing.T) *ntfyEnv {
	t.Helper()
	e := newEnv(t)
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = notify.New(e.st, box, notify.Options{
		BaseURL: baseURL, Kinds: []notify.Kind{ntfy.Kind(ntfy.NewClient(true))},
		Log: slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: func() time.Time { return e.now },
	})
	return &ntfyEnv{env: e, srv: newNtfyServer(t)}
}

func (e *ntfyEnv) create(ownerID string, enabled bool) string {
	e.t.Helper()
	id, err := e.svc.CreateChannel(e.ctx, ownerID, ntfy.ID, notify.ChannelInput{
		Name: "Phone", Enabled: enabled, Language: "en",
		Config: notify.Config{"server": e.srv.URL, "topic": secretTopic, "token": secretToken},
	})
	e.must(err)
	return id
}

// Criterion 1: without APP_SECRET_KEY notifications are disabled.
func TestDisabledWithoutSecretKey(t *testing.T) {
	e := newEnv(t)
	// A channel and a due bike exist (e.g. the key was removed later).
	id := e.channel(owner, "a")
	e.bike(owner, "Gravel", date("2026-06-01"))
	svc := notify.New(e.st, nil, notify.Options{
		BaseURL: baseURL, Kinds: []notify.Kind{e.a.kind("a")}, Now: func() time.Time { return e.now },
	})
	if svc.Enabled() {
		t.Fatal("service without key is enabled")
	}
	if _, err := svc.CreateChannel(e.ctx, owner, "a", notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}}); !errors.Is(err, notify.ErrDisabled) {
		t.Errorf("CreateChannel: %v", err)
	}
	if err := svc.UpdateChannel(e.ctx, owner, id, notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}}); !errors.Is(err, notify.ErrDisabled) {
		t.Errorf("UpdateChannel: %v", err)
	}
	if err := svc.Test(e.ctx, owner, id); !errors.Is(err, notify.ErrDisabled) {
		t.Errorf("Test: %v", err)
	}
	if _, err := svc.Channel(e.ctx, owner, id); !errors.Is(err, notify.ErrDisabled) {
		t.Errorf("Channel: %v", err)
	}

	// Nothing is sent or stored, however it is started.
	if err := svc.Check(e.ctx, owner); err != nil {
		t.Errorf("Check: %v", err)
	}
	svc.RunDue(e.ctx)
	done := make(chan struct{})
	go func() { svc.Run(e.ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run without key does not return")
	}
	if got := e.a.take(); len(got) != 0 {
		t.Errorf("sent %+v", got)
	}
	if got := e.states(owner); len(got) != 0 {
		t.Errorf("states %v", got)
	}
	owners, err := e.st.Notifications().ListNotificationOwners(e.ctx)
	if err != nil || len(owners) != 1 || owners[0].LastRunAt != nil {
		t.Errorf("owners %+v, %v", owners, err)
	}
	if ch, _ := e.st.Notifications().GetNotificationChannel(e.ctx, owner, id); ch.LastError != "" || ch.LastSentAt != nil {
		t.Errorf("channel touched: %+v", ch)
	}

	// A nil service is disabled, too.
	var nilSvc *notify.Service
	if nilSvc.Enabled() {
		t.Error("nil service is enabled")
	}

	// Settings, listing and deleting still work.
	ns := notify.Defaults(owner)
	ns.Events = []string{notify.EventGarmin}
	if err := svc.SaveSettings(e.ctx, ns); err != nil {
		t.Errorf("SaveSettings: %v", err)
	}
	if got, err := svc.Settings(e.ctx, owner, ""); err != nil || !slices.Equal(got.Events, ns.Events) {
		t.Errorf("Settings: %+v, %v", got, err)
	}
	list, err := svc.Channels(e.ctx, owner)
	if err != nil || len(list) != 1 || list[0].Config != nil {
		t.Errorf("Channels: %+v, %v", list, err)
	}
	if err := svc.DeleteChannel(e.ctx, owner, id); err != nil {
		t.Errorf("DeleteChannel: %v", err)
	}
	// Disabled before anything was created: no settings are made up.
	empty := notify.New(e.st, nil, notify.Options{})
	if _, err := empty.CreateChannel(e.ctx, "o9", "a", notify.ChannelInput{Name: "x"}); !errors.Is(err, notify.ErrDisabled) {
		t.Error(err)
	}
	if _, err := e.st.Notifications().GetNotificationSettings(e.ctx, "o9"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("settings created while disabled: %v", err)
	}
}

// Criterion 2: the configuration is stored sealed, secrets stay out of
// errors and logs.
func TestConfigStoredSealed(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "n.sqlite")
	st, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	srv := newNtfyServer(t)
	svc := notify.New(st, box, notify.Options{BaseURL: baseURL, Kinds: []notify.Kind{ntfy.Kind(ntfy.NewClient(true))}})
	id, err := svc.CreateChannel(ctx, owner, ntfy.ID, notify.ChannelInput{
		Name: "Phone", Enabled: true,
		Config: notify.Config{"server": srv.URL, "topic": secretTopic, "token": secretToken},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := st.Notifications().GetNotificationChannel(ctx, owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Config) == 0 {
		t.Fatal("no config stored")
	}
	for _, s := range []string{secretTopic, secretToken, srv.URL, `"topic"`, `"token"`} {
		if bytes.Contains(raw.Config, []byte(s)) {
			t.Errorf("stored config contains %q", s)
		}
	}
	// Neither does the database file (including its write-ahead log).
	for _, f := range []string{path, path + "-wal"} {
		data, err := os.ReadFile(f)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range []string{secretTopic, secretToken} {
			if bytes.Contains(data, []byte(s)) {
				t.Errorf("%s contains %q", filepath.Base(f), s)
			}
		}
	}

	// The service reads it back, but never shows the secret field again.
	info, err := svc.Channel(ctx, owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.Broken || info.Settings["topic"] != secretTopic || info.Settings["server"] != srv.URL ||
		info.SecretSet["token"] != true || len(info.Settings["token"]) != 0 || info.Config != nil {
		t.Errorf("channel %+v", info)
	}
	if _, has := info.Settings["token"]; has {
		t.Error("token is in the settings")
	}

	// The sealed config is bound to owner and channel: copied to another
	// channel it can't be opened.
	copy := store.NotificationChannel{
		ID: "copy", OwnerID: owner, Kind: ntfy.ID, Name: "Copy", Config: raw.Config, Enabled: true, CreatedAt: time.Now(),
	}
	if err := st.Notifications().CreateNotificationChannel(ctx, copy); err != nil {
		t.Fatal(err)
	}
	if info, err := svc.Channel(ctx, owner, "copy"); err != nil || !info.Broken {
		t.Errorf("copied config opens: %+v, %v", info, err)
	}
	if err := svc.Test(ctx, owner, "copy"); !errors.Is(err, notify.ErrDisabled) && notify.ErrorCode(err) != notify.ErrorSecret {
		t.Errorf("Test copied: %v", err)
	}
	if srv.count() != 0 {
		t.Errorf("copied channel sent %d requests", srv.count())
	}
}

// A changed APP_SECRET_KEY leaves a channel broken, not crashing.
func TestChannelWithOtherKeyIsBroken(t *testing.T) {
	e := newNtfyEnv(t)
	id := e.create(owner, true)
	other, err := secret.New("another key, at least 32 characters long")
	if err != nil {
		t.Fatal(err)
	}
	svc := notify.New(e.st, other, notify.Options{
		BaseURL: baseURL, Kinds: []notify.Kind{ntfy.Kind(ntfy.NewClient(true))}, Log: slog.New(slog.NewTextHandler(e.log, nil)),
		Now: func() time.Time { return e.now },
	})
	info, err := svc.Channel(e.ctx, owner, id)
	if err != nil || !info.Broken {
		t.Fatalf("channel %+v, %v", info, err)
	}
	if err := svc.Test(e.ctx, owner, id); notify.ErrorCode(err) != notify.ErrorSecret {
		t.Errorf("Test: %v", err)
	}
	ch, _ := e.st.Notifications().GetNotificationChannel(e.ctx, owner, id)
	if ch.LastError != notify.ErrorSecret || ch.LastSentAt != nil {
		t.Errorf("recorded %+v", ch)
	}
	if e.srv.count() != 0 {
		t.Error("sent with a config that can't be opened")
	}
	// Entering it again repairs the channel.
	err = svc.UpdateChannel(e.ctx, owner, id, notify.ChannelInput{
		Name: "Phone", Enabled: true, Config: notify.Config{"server": e.srv.URL, "topic": secretTopic},
	})
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := svc.Channel(e.ctx, owner, id); info.Broken || info.SecretSet["token"] {
		t.Errorf("after repair %+v", info)
	}
	if err := svc.Test(e.ctx, owner, id); err != nil {
		t.Errorf("Test after repair: %v", err)
	}
}

// Criterion 3: the test message and how failures are recorded.
func TestTestMessage(t *testing.T) {
	e := newNtfyEnv(t)
	id := e.create(owner, false) // a disabled channel can be tested
	if err := e.svc.Test(e.ctx, owner, id); err != nil {
		t.Fatal(err)
	}
	if e.srv.count() != 1 {
		t.Fatalf("%d requests", e.srv.count())
	}
	r, body := e.srv.reqs[0], e.srv.bodies[0]
	if r.Method != http.MethodPost || r.URL.Path != "/"+secretTopic {
		t.Errorf("request %s %s", r.Method, r.URL.Path)
	}
	for k, want := range map[string]string{
		"Title": "Spokebook test", "Priority": "3", "Tags": "bike", "Click": baseURL + "/",
		"Authorization": "Bearer " + secretToken,
	} {
		if got := r.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	if body != "Notifications from Spokebook arrive here." {
		t.Errorf("body %q", body)
	}
	ch, _ := e.st.Notifications().GetNotificationChannel(e.ctx, owner, id)
	if ch.LastError != "" || ch.LastSentAt == nil || !ch.LastSentAt.Equal(e.now) {
		t.Errorf("channel %+v", ch)
	}

	// The message is in the language of the settings.
	e.setLanguage(owner, "de")
	if err := e.svc.Test(e.ctx, owner, id); err != nil {
		t.Fatal(err)
	}
	if got := e.srv.bodies[1]; !strings.Contains(got, "Benachrichtigungen von Spokebook") {
		t.Errorf("German body %q", got)
	}
}

func TestTestFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		setup  func(*ntfyEnv)
		want   string
		target error
	}{
		{"401", func(e *ntfyEnv) { e.srv.setStatus(401) }, notify.ErrorUnauthorized, notify.ErrUnauthorized},
		{"403", func(e *ntfyEnv) { e.srv.setStatus(403) }, notify.ErrorUnauthorized, notify.ErrUnauthorized},
		{"500", func(e *ntfyEnv) { e.srv.setStatus(500) }, notify.ErrorRejected, notify.ErrRejected},
		{"400", func(e *ntfyEnv) { e.srv.setStatus(400) }, notify.ErrorRejected, notify.ErrRejected},
		{"redirect", func(e *ntfyEnv) {
			e.srv.setStatus(http.StatusFound)
			e.srv.loc = "http://169.254.169.254/latest"
		}, notify.ErrorRejected, notify.ErrRejected},
		{"unreachable", func(e *ntfyEnv) { e.srv.Close() }, notify.ErrorUnreachable, notify.ErrUnreachable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newNtfyEnv(t)
			id := e.create(owner, true)
			// An earlier success must stay visible after the failure.
			if err := e.svc.Test(e.ctx, owner, id); err != nil {
				t.Fatal(err)
			}
			sentAt := e.now
			e.now = e.now.Add(time.Hour)
			tc.setup(e)
			err := e.svc.Test(e.ctx, owner, id)
			if !errors.Is(err, tc.target) || notify.ErrorCode(err) != tc.want {
				t.Fatalf("error %v, code %q", err, notify.ErrorCode(err))
			}
			for _, s := range []string{secretToken, secretTopic} {
				if strings.Contains(err.Error(), s) {
					t.Errorf("error contains %q: %v", s, err)
				}
				if strings.Contains(e.log.String(), s) {
					t.Errorf("log contains %q:\n%s", s, e.log.String())
				}
			}
			if !strings.Contains(e.log.String(), "sending failed") {
				t.Errorf("failure not logged: %q", e.log.String())
			}
			ch, _ := e.st.Notifications().GetNotificationChannel(e.ctx, owner, id)
			if ch.LastError != tc.want || ch.LastSentAt == nil || !ch.LastSentAt.Equal(sentAt) {
				t.Errorf("channel %+v", ch)
			}
		})
	}
}

func TestTestRecoversAndNotFound(t *testing.T) {
	e := newNtfyEnv(t)
	id := e.create(owner, true)
	e.srv.setStatus(401)
	if err := e.svc.Test(e.ctx, owner, id); err == nil {
		t.Fatal("401 accepted")
	}
	e.srv.setStatus(200)
	e.now = e.now.Add(time.Minute)
	if err := e.svc.Test(e.ctx, owner, id); err != nil {
		t.Fatal(err)
	}
	ch, _ := e.st.Notifications().GetNotificationChannel(e.ctx, owner, id)
	if ch.LastError != "" || ch.LastSentAt == nil || !ch.LastSentAt.Equal(e.now) {
		t.Errorf("channel %+v", ch)
	}
	// Another owner's channel and unknown IDs.
	if err := e.svc.Test(e.ctx, "o2", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("other owner: %v", err)
	}
	if err := e.svc.Test(e.ctx, owner, "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
	if e.srv.count() != 2 {
		t.Errorf("%d requests, want 2", e.srv.count())
	}
}

// Secrets must not show up in the log when a check's sending fails.
func TestFailingCheckDoesNotLogSecrets(t *testing.T) {
	e := newNtfyEnv(t)
	e.create(owner, true)
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.srv.setStatus(401)
	e.check(owner)
	if e.srv.count() == 0 {
		t.Fatal("nothing sent")
	}
	if !strings.Contains(e.log.String(), "sending failed") {
		t.Errorf("failure not logged: %q", e.log.String())
	}
	for _, s := range []string{secretToken, secretTopic} {
		if strings.Contains(e.log.String(), s) {
			t.Errorf("log contains %q:\n%s", s, e.log.String())
		}
	}
	// Same for an unreachable server.
	e.srv.Close()
	e.now = e.now.Add(days(2))
	e.check(owner)
	for _, s := range []string{secretToken, secretTopic} {
		if strings.Contains(e.log.String(), s) {
			t.Errorf("log contains %q:\n%s", s, e.log.String())
		}
	}
}

func TestCreateChannelValidation(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("ä", 101)
	for _, tc := range []struct {
		name  string
		kind  string
		in    notify.ChannelInput
		field string // "" means an error that is no *FieldError
		err   error
	}{
		{"unknown kind", "nope", notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}}, "", notify.ErrUnknownKind},
		{"empty name", "a", notify.ChannelInput{Name: "", Config: notify.Config{"to": "me"}}, "name", nil},
		{"blank name", "a", notify.ChannelInput{Name: "  \t", Config: notify.Config{"to": "me"}}, "name", nil},
		{"name too long", "a", notify.ChannelInput{Name: long, Config: notify.Config{"to": "me"}}, "name", nil},
		{"invalid config", "a", notify.ChannelInput{Name: "x", Config: notify.Config{}}, "to", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := e.svc.CreateChannel(e.ctx, owner, tc.kind, tc.in)
			var fe *notify.FieldError
			switch {
			case err == nil:
				t.Fatalf("accepted, id %q", id)
			case tc.err != nil && !errors.Is(err, tc.err):
				t.Errorf("error %v, want %v", err, tc.err)
			case tc.field != "" && (!errors.As(err, &fe) || fe.Field != tc.field):
				t.Errorf("error %v, want field %s", err, tc.field)
			}
			if list, _ := e.svc.Channels(e.ctx, owner); len(list) != 0 {
				t.Errorf("channel stored: %+v", list)
			}
			if _, err := e.st.Notifications().GetNotificationSettings(e.ctx, owner); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("settings stored on failure: %v", err)
			}
		})
	}
	// 100 characters (not bytes) are fine; the name is trimmed.
	id, err := e.svc.CreateChannel(e.ctx, owner, "a", notify.ChannelInput{Name: " " + strings.Repeat("ä", 100) + " ", Config: notify.Config{"to": "me"}})
	if err != nil {
		t.Fatal(err)
	}
	if info := e.channelInfo(owner, id); info.Name != strings.Repeat("ä", 100) || info.Enabled || info.Kind != "a" {
		t.Errorf("channel %+v", info)
	}
}

func TestCreateChannelSavesDefaultSettings(t *testing.T) {
	e := newEnv(t)
	e.must2(e.svc.CreateChannel(e.ctx, owner, "a", notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}, Language: "de"}))
	ns, err := e.st.Notifications().GetNotificationSettings(e.ctx, owner)
	if err != nil || ns.Language != "de" || ns.Threshold != notify.ThresholdSoon || ns.OpenDays != 7 ||
		!slices.Equal(ns.Events, []string{notify.EventMaintenance, notify.EventWear}) {
		t.Errorf("settings %+v, %v", ns, err)
	}
	// Existing settings are not replaced by a second channel.
	ns.Events, ns.Language = []string{notify.EventGarmin}, "en"
	e.must(e.svc.SaveSettings(e.ctx, ns))
	e.must2(e.svc.CreateChannel(e.ctx, owner, "b", notify.ChannelInput{Name: "y", Config: notify.Config{"to": "me"}, Language: "de"}))
	if got, _ := e.svc.Settings(e.ctx, owner, ""); got.Language != "en" || !slices.Equal(got.Events, []string{notify.EventGarmin}) {
		t.Errorf("settings overwritten: %+v", got)
	}
	// Unknown language: the default.
	e.must2(e.svc.CreateChannel(e.ctx, "o2", "a", notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}, Language: "fr"}))
	if got, _ := e.svc.Settings(e.ctx, "o2", ""); got.Language != "en" {
		t.Errorf("language %q", got.Language)
	}
}

func (e *env) must2(_ string, err error) {
	e.t.Helper()
	e.must(err)
}

// Update keeps the secret token when left empty; Clear removes it.
func TestUpdateChannel(t *testing.T) {
	e := newNtfyEnv(t)
	id := e.create(owner, true)
	in := func(token string, clear ...string) notify.ChannelInput {
		return notify.ChannelInput{
			Name: "Renamed", Enabled: true, Clear: clear,
			Config: notify.Config{"server": e.srv.URL, "topic": "newtopic", "token": token},
		}
	}
	send := func() string {
		t.Helper()
		before := e.srv.count()
		e.must(e.svc.Test(e.ctx, owner, id))
		if e.srv.count() != before+1 {
			t.Fatal("not sent")
		}
		return e.srv.reqs[before].Header.Get("Authorization")
	}

	// Empty token keeps the stored one.
	e.must(e.svc.UpdateChannel(e.ctx, owner, id, in("")))
	info := e.channelInfo(owner, id)
	if info.Name != "Renamed" || info.Settings["topic"] != "newtopic" || !info.SecretSet["token"] {
		t.Errorf("after update %+v", info)
	}
	if got := send(); got != "Bearer "+secretToken {
		t.Errorf("authorization %q after keeping the token", got)
	}
	if got := e.srv.reqs[len(e.srv.reqs)-1].URL.Path; got != "/newtopic" {
		t.Errorf("path %q", got)
	}
	// A new token replaces it.
	e.must(e.svc.UpdateChannel(e.ctx, owner, id, in("tk_new")))
	if got := send(); got != "Bearer tk_new" {
		t.Errorf("authorization %q after replacing", got)
	}
	// Clear removes it, even with a value entered for it.
	e.must(e.svc.UpdateChannel(e.ctx, owner, id, in("", "token")))
	if info := e.channelInfo(owner, id); info.SecretSet["token"] {
		t.Errorf("token still set: %+v", info)
	}
	if got := send(); got != "" {
		t.Errorf("authorization %q after clearing", got)
	}
	// Empty again: still none.
	e.must(e.svc.UpdateChannel(e.ctx, owner, id, in("")))
	if got := send(); got != "" {
		t.Errorf("authorization %q", got)
	}

	// Disabling.
	e.must(e.svc.UpdateChannel(e.ctx, owner, id, notify.ChannelInput{Name: "Renamed", Config: notify.Config{"server": e.srv.URL, "topic": "newtopic"}}))
	if info := e.channelInfo(owner, id); info.Enabled {
		t.Errorf("still enabled: %+v", info)
	}

	// Invalid input changes nothing.
	for field, bad := range map[string]notify.ChannelInput{
		"name":   {Name: "", Config: notify.Config{"server": e.srv.URL, "topic": "ok"}},
		"topic":  {Name: "x", Config: notify.Config{"server": e.srv.URL, "topic": "a/b"}},
		"server": {Name: "x", Config: notify.Config{"server": "ftp://x", "topic": "ok"}},
	} {
		err := e.svc.UpdateChannel(e.ctx, owner, id, bad)
		var fe *notify.FieldError
		if !errors.As(err, &fe) || fe.Field != field {
			t.Errorf("%s: %v", field, err)
		}
	}
	if info := e.channelInfo(owner, id); info.Name != "Renamed" || info.Settings["topic"] != "newtopic" {
		t.Errorf("invalid update changed %+v", info)
	}
	if err := e.svc.UpdateChannel(e.ctx, owner, "missing", in("")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

// Another owner never sees or changes a channel.
func TestChannelOwnerIsolation(t *testing.T) {
	e := newNtfyEnv(t)
	id := e.create(owner, true)
	if _, err := e.svc.Channel(e.ctx, "o2", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("Channel: %v", err)
	}
	err := e.svc.UpdateChannel(e.ctx, "o2", id, notify.ChannelInput{Name: "Hijack", Config: notify.Config{"server": e.srv.URL, "topic": "x"}})
	if !errors.Is(err, store.ErrNotFound) {
		t.Errorf("UpdateChannel: %v", err)
	}
	if err := e.svc.DeleteChannel(e.ctx, "o2", id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("DeleteChannel: %v", err)
	}
	if list, _ := e.svc.Channels(e.ctx, "o2"); len(list) != 0 {
		t.Errorf("Channels: %+v", list)
	}
	if info := e.channelInfo(owner, id); info.Name != "Phone" || info.Settings["topic"] != secretTopic {
		t.Errorf("changed: %+v", info)
	}
	if err := e.svc.DeleteChannel(e.ctx, owner, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Channel(e.ctx, owner, id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted: %v", err)
	}
}

// Criterion 4: settings.
func TestSettingsDefaults(t *testing.T) {
	e := newEnv(t)
	for lang, want := range map[string]string{"": "en", "de": "de", "en": "en", "fr": "en", "garbage!": "en"} {
		ns, err := e.svc.Settings(e.ctx, owner, lang)
		if err != nil {
			t.Fatal(err)
		}
		if ns.OwnerID != owner || !slices.Equal(ns.Events, []string{notify.EventMaintenance, notify.EventWear}) ||
			ns.Threshold != notify.ThresholdSoon || ns.OpenDays != 7 || ns.Language != want || ns.LastRunAt != nil {
			t.Errorf("lang %q: %+v", lang, ns)
		}
	}
	// Defaults are not stored by reading them.
	if _, err := e.st.Notifications().GetNotificationSettings(e.ctx, owner); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("settings stored by reading: %v", err)
	}
	if l := e.svc.Languages(); !slices.Contains(l, "en") || !slices.Contains(l, "de") {
		t.Errorf("languages %v", l)
	}
}

func TestSaveSettings(t *testing.T) {
	e := newEnv(t)
	ns := store.NotificationSettings{
		OwnerID: owner, Threshold: notify.ThresholdOverdue, OpenDays: 30, Language: "de",
		// Unknown and double events are dropped, the order is the standard one.
		Events:    []string{notify.EventGarmin, "bogus", notify.EventMaintenance, notify.EventGarmin},
		LastRunAt: ptr(date("2020-01-01")),
	}
	e.must(e.svc.SaveSettings(e.ctx, ns))
	got, err := e.svc.Settings(e.ctx, owner, "en")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Events, []string{notify.EventMaintenance, notify.EventGarmin}) || got.Threshold != "overdue" ||
		got.OpenDays != 30 || got.Language != "de" || got.LastRunAt != nil {
		t.Errorf("saved %+v", got)
	}
	// Another owner has the defaults.
	if o2, _ := e.svc.Settings(e.ctx, "o2", ""); o2.Threshold != "soon" || o2.OpenDays != 7 {
		t.Errorf("other owner %+v", o2)
	}
	// All events off is allowed.
	ns.Events = nil
	e.must(e.svc.SaveSettings(e.ctx, ns))
	if got, _ := e.svc.Settings(e.ctx, owner, ""); len(got.Events) != 0 {
		t.Errorf("events %v", got.Events)
	}
	// Boundaries of the days.
	for _, days := range []int{1, 365} {
		ns.OpenDays = days
		if err := e.svc.SaveSettings(e.ctx, ns); err != nil {
			t.Errorf("%d days: %v", days, err)
		}
	}
}

func TestSettingsValidation(t *testing.T) {
	e := newEnv(t)
	good := notify.Defaults(owner)
	for _, tc := range []struct {
		name  string
		mod   func(*store.NotificationSettings)
		field string
	}{
		{"empty threshold", func(s *store.NotificationSettings) { s.Threshold = "" }, "threshold"},
		{"unknown threshold", func(s *store.NotificationSettings) { s.Threshold = "never" }, "threshold"},
		{"zero days", func(s *store.NotificationSettings) { s.OpenDays = 0 }, "open_days"},
		{"negative days", func(s *store.NotificationSettings) { s.OpenDays = -3 }, "open_days"},
		{"too many days", func(s *store.NotificationSettings) { s.OpenDays = 366 }, "open_days"},
		{"empty language", func(s *store.NotificationSettings) { s.Language = "" }, "language"},
		{"malformed language", func(s *store.NotificationSettings) { s.Language = "e n!" }, "language"},
		{"non-canonical language", func(s *store.NotificationSettings) { s.Language = "EN" }, "language"},
		{"language without catalog", func(s *store.NotificationSettings) { s.Language = "fr" }, "language"},
		{"very long language", func(s *store.NotificationSettings) { s.Language = strings.Repeat("en-", 20) }, "language"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := good
			tc.mod(&ns)
			err := e.svc.SaveSettings(e.ctx, ns)
			var fe *notify.FieldError
			if !errors.As(err, &fe) || fe.Field != tc.field {
				t.Errorf("error %v, want field %s", err, tc.field)
			}
			if _, err := e.st.Notifications().GetNotificationSettings(e.ctx, owner); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("invalid settings stored: %v", err)
			}
		})
	}
	// CheckSettings alone accepts any well-formed language tag.
	ns := good
	ns.Language = "fr"
	if _, err := notify.CheckSettings(ns); err != nil {
		t.Errorf("CheckSettings(fr): %v", err)
	}
}

func TestKinds(t *testing.T) {
	e := newNtfyEnv(t)
	if len(e.svc.Kinds()) != 1 || e.svc.Kinds()[0].ID != "ntfy" {
		t.Fatalf("kinds %+v", e.svc.Kinds())
	}
	k, ok := e.svc.Kind("ntfy")
	if !ok || k.ID != ntfy.ID {
		t.Fatalf("kind %+v, %v", k, ok)
	}
	if _, ok := e.svc.Kind("email"); ok {
		t.Error("unknown kind found")
	}
	server, _ := k.Field("server")
	topic, _ := k.Field("topic")
	token, _ := k.Field("token")
	if !server.Required || server.Default != ntfy.DefaultServer || !topic.Required || topic.Secret || token.Required || !token.Secret || token.Type != "password" {
		t.Errorf("fields %+v %+v %+v", server, topic, token)
	}
	if _, ok := k.Field("nope"); ok {
		t.Error("unknown field found")
	}
	if k.Validate == nil || k.Open == nil {
		t.Error("kind incomplete")
	}
	// Kinds can be mixed: the two fake kinds are both served.
	e2 := newEnv(t)
	if len(e2.svc.Kinds()) != 2 {
		t.Errorf("kinds %d", len(e2.svc.Kinds()))
	}
}

func TestErrorCode(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{notify.ErrUnauthorized, notify.ErrorUnauthorized},
		{notify.ErrRejected, notify.ErrorRejected},
		{notify.ErrUnreachable, notify.ErrorUnreachable},
		{notify.ErrBlocked, notify.ErrorBlocked},
		// Blocked is more precise than unreachable.
		{errors.Join(notify.ErrUnreachable, notify.ErrBlocked), notify.ErrorBlocked},
		{errors.Join(errors.New("x"), notify.ErrUnreachable), notify.ErrorUnreachable},
		{errors.New("boom"), notify.ErrorFailed},
		{notify.ErrUnknownKind, notify.ErrorFailed},
	} {
		if got := notify.ErrorCode(tc.err); got != tc.want {
			t.Errorf("ErrorCode(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

// Every message ID the notify package builds from events, thresholds,
// error codes and fields exists in each language.
func TestMessageIDsExist(t *testing.T) {
	bundle, err := i18n.Load(web.Locales, "locales")
	if err != nil {
		t.Fatal(err)
	}
	ids := []string{
		"notify.kind." + ntfy.ID, "notify.test.title", "notify.test.body", "notify.bike.title",
		"notify.unassigned.title", "notify.unassigned.body.one", "notify.unassigned.body.other",
		"notify.garmin.title", "notify.garmin.unauthorized", "notify.garmin.secret", "notify.item.open",
		"notify.error.name", "notify.error.threshold", "notify.error.open_days", "notify.error.language",
		"notify.error.max_channels", "notify.ntfy.error.server_address",
	}
	for _, ev := range notify.Events {
		ids = append(ids, "notify.event."+ev)
	}
	for _, th := range notify.Thresholds {
		ids = append(ids, "notify.threshold."+th)
	}
	for _, code := range []string{
		notify.ErrorUnauthorized, notify.ErrorRejected, notify.ErrorUnreachable, notify.ErrorBlocked, notify.ErrorSecret,
		notify.ErrorFailed,
	} {
		ids = append(ids, "notify.channel_error."+code)
	}
	for _, st := range []string{"soon", "overdue"} {
		ids = append(ids, "notify.item.level."+st, "notify.item.step."+st, "notify.item.wear."+st)
	}
	k := ntfy.Kind(nil)
	for _, f := range k.Fields {
		ids = append(ids, f.Label)
		if f.Hint != "" {
			ids = append(ids, f.Hint)
		}
	}
	for _, f := range []string{ntfy.FieldServer, ntfy.FieldTopic, ntfy.FieldToken} {
		ids = append(ids, "notify.ntfy.error."+f)
	}
	for _, tag := range bundle.Languages() {
		keys := bundle.Keys(tag)
		for _, id := range ids {
			if !slices.Contains(keys, id) {
				t.Errorf("%s: message %q missing", tag, id)
			}
		}
	}
}
