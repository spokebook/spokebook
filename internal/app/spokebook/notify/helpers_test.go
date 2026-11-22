package notify_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/spokebook/spokebook/internal/app/spokebook/catalog"
	"github.com/spokebook/spokebook/internal/app/spokebook/components"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
	"github.com/spokebook/spokebook/internal/app/spokebook/store/sqlite"
)

const (
	baseURL = "https://bikes.example"
	// testKey is APP_SECRET_KEY in tests.
	testKey = "0123456789abcdef0123456789abcdef"
)

// sink is a kind of channel that records the messages it takes. failFn
// may refuse a message.
type sink struct {
	mu     sync.Mutex
	sent   []notify.Message
	failFn func(notify.Message) error
}

func (s *sink) kind(id string) notify.Kind {
	return notify.Kind{
		ID:     id,
		Fields: []notify.Field{{Name: "to", Required: true}},
		Validate: func(c notify.Config) (notify.Config, error) {
			if c["to"] == "" {
				return nil, &notify.FieldError{Field: "to", Msg: "x"}
			}
			return notify.Config{"to": c["to"]}, nil
		},
		Open: func(notify.Config) (notify.Channel, error) { return s, nil },
	}
}

func (s *sink) Send(_ context.Context, m notify.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failFn != nil {
		if err := s.failFn(m); err != nil {
			return err
		}
	}
	s.sent = append(s.sent, m)
	return nil
}

// take returns and forgets the messages sent so far.
func (s *sink) take() []notify.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.sent
	s.sent = nil
	return out
}

func (s *sink) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.failFn = nil
		return
	}
	s.failFn = func(notify.Message) error { return err }
}

// env is a service on an in-memory store with the fake kinds "a" and "b"
// and a clock the test sets.
type env struct {
	t    *testing.T
	ctx  context.Context
	st   *sqlite.Store
	svc  *notify.Service
	a, b *sink
	now  time.Time
	log  *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{
		t: t, ctx: context.Background(), a: &sink{}, b: &sink{}, log: &bytes.Buffer{},
		now: time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC),
	}
	st, err := sqlite.Open(e.ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	e.st = st
	for _, o := range []string{owner, "o2"} {
		if err := catalog.SeedSteps(e.ctx, st, o); err != nil {
			t.Fatal(err)
		}
		if err := components.SeedTypes(e.ctx, st, o); err != nil {
			t.Fatal(err)
		}
	}
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	e.svc = notify.New(st, box, notify.Options{
		BaseURL: baseURL + "/", Kinds: []notify.Kind{e.a.kind("a"), e.b.kind("b")},
		Log: slog.New(slog.NewTextHandler(e.log, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now: func() time.Time { return e.now },
	})
	return e
}

func (e *env) must(err error) {
	e.t.Helper()
	if err != nil {
		e.t.Fatal(err)
	}
}

// channel creates an enabled channel of kind "a" for owner.
func (e *env) channel(ownerID, kind string) string {
	e.t.Helper()
	id, err := e.svc.CreateChannel(e.ctx, ownerID, kind, notify.ChannelInput{
		Name: "Phone", Config: notify.Config{"to": "me"}, Enabled: true, Language: "en",
	})
	e.must(err)
	return id
}

// settings saves settings with the events and the other values of the
// defaults.
func (e *env) settings(ownerID string, threshold string, events ...string) {
	e.t.Helper()
	ns := notify.Defaults(ownerID)
	ns.Events, ns.Threshold = events, threshold
	e.must(e.svc.SaveSettings(e.ctx, ns))
}

func (e *env) setLanguage(ownerID, lang string) {
	e.t.Helper()
	ns, err := e.svc.Settings(e.ctx, ownerID, "")
	e.must(err)
	ns.Language = lang
	e.must(e.svc.SaveSettings(e.ctx, ns))
}

// bike adds a bike in service since the date.
func (e *env) bike(ownerID, name string, inService time.Time) store.Bike {
	e.t.Helper()
	b, err := e.st.Bikes().CreateBike(e.ctx, store.Bike{
		ID: uuid.NewString(), OwnerID: ownerID, Name: name, InServiceAt: &inService, CreatedAt: inService,
	})
	e.must(err)
	return b
}

// states returns the stored states by key.
func (e *env) states(ownerID string) map[string]string {
	e.t.Helper()
	list, err := e.st.Notifications().ListNotificationStates(e.ctx, ownerID)
	e.must(err)
	out := map[string]string{}
	for _, s := range list {
		out[s.Key] = s.State
	}
	return out
}

func (e *env) check(ownerID string) {
	e.t.Helper()
	e.must(e.svc.Check(e.ctx, ownerID))
}

// checkSent runs a check and returns the messages sent on channel a.
func (e *env) checkSent(ownerID string) []notify.Message {
	e.t.Helper()
	e.check(ownerID)
	return e.a.take()
}

func (e *env) channelInfo(ownerID, id string) notify.ChannelInfo {
	e.t.Helper()
	info, err := e.svc.Channel(e.ctx, ownerID, id)
	e.must(err)
	return info
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

func date(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func ptr[T any](v T) *T { return &v }

func contains(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
