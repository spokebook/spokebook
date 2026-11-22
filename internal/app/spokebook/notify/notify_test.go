package notify_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/catalog"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
	"github.com/spokebook/spokebook/internal/app/spokebook/store/sqlite"
)

const owner = "o1"

// fake is a kind of channel that records the messages it sends.
type fake struct {
	mu   sync.Mutex
	sent []notify.Message
	fail error
}

func (f *fake) kind() notify.Kind {
	return notify.Kind{
		ID:     "fake",
		Fields: []notify.Field{{Name: "to", Required: true}, {Name: "token", Secret: true}},
		Validate: func(c notify.Config) (notify.Config, error) {
			if c["to"] == "" {
				return nil, &notify.FieldError{Field: "to", Msg: "x"}
			}
			return notify.Config{"to": c["to"], "token": c["token"]}, nil
		},
		Open: func(notify.Config) (notify.Channel, error) { return f, nil },
	}
}

func (f *fake) Send(_ context.Context, m notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.sent = append(f.sent, m)
	return nil
}

func (f *fake) take() []notify.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

func setup(t *testing.T, now *time.Time) (*sqlite.Store, *notify.Service, *fake) {
	t.Helper()
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := catalog.SeedSteps(ctx, st, owner); err != nil {
		t.Fatal(err)
	}
	box, err := secret.New(strings.Repeat("k", 32))
	if err != nil {
		t.Fatal(err)
	}
	f := &fake{}
	svc := notify.New(st, box, notify.Options{
		BaseURL: "https://bikes.example", Kinds: []notify.Kind{f.kind()}, Now: func() time.Time { return *now },
	})
	return st, svc, f
}

func TestCheckMaintenanceOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 6, 26, 12, 0, 0, 0, time.UTC)
	st, svc, f := setup(t, &now)
	start := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	b, err := st.Bikes().CreateBike(ctx, store.Bike{
		ID: "b1", OwnerID: owner, Name: "Gravel", InServiceAt: &start, CreatedAt: start,
	})
	if err != nil {
		t.Fatal(err)
	}
	id, err := svc.CreateChannel(ctx, owner, "fake", notify.ChannelInput{
		Name: "Phone", Config: notify.Config{"to": "me", "token": "secret-token"}, Enabled: true, Language: "de",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
	got := f.take()
	if len(got) != 1 || !strings.Contains(got[0].Title, "Gravel") || got[0].Link != "https://bikes.example/bikes/"+b.ID {
		t.Fatalf("first check sent %+v", got)
	}
	if !strings.Contains(got[0].Body, "bald fällig") {
		t.Errorf("body %q not German", got[0].Body)
	}
	if err := svc.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) != 0 {
		t.Fatalf("second check sent %+v", got)
	}
	// Overdue: notified again, but a failure is retried next time.
	now = now.AddDate(0, 0, 10)
	f.fail = notify.ErrUnreachable
	if err := svc.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
	info, err := svc.Channel(ctx, owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if info.LastError != notify.ErrorUnreachable || info.SecretSet["token"] != true || info.Settings["token"] != "" {
		t.Errorf("channel %+v", info)
	}
	f.fail = nil
	if err := svc.Check(ctx, owner); err != nil {
		t.Fatal(err)
	}
	if got := f.take(); len(got) != 1 || !strings.Contains(got[0].Body, "überfällig") {
		t.Fatalf("overdue check sent %+v", got)
	}
}

func TestDisabled(t *testing.T) {
	ctx := context.Background()
	st, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	svc := notify.New(st, nil, notify.Options{})
	_, err = svc.CreateChannel(ctx, owner, "fake", notify.ChannelInput{Name: "x"})
	if !errors.Is(err, notify.ErrDisabled) {
		t.Errorf("CreateChannel: %v", err)
	}
	if err := svc.Test(ctx, owner, "x"); !errors.Is(err, notify.ErrDisabled) {
		t.Errorf("Test: %v", err)
	}
}
