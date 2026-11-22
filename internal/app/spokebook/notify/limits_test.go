package notify_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/catalog"
	"github.com/spokebook/spokebook/internal/app/spokebook/maintenance"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

func notifyState(key string) store.NotificationState {
	return store.NotificationState{Key: key, State: "error", UpdatedAt: time.Now()}
}

// Of the pending levels of a bike only the highest is notified (it
// includes the lower ones); the states of all are stored, so none of them
// is sent later.
func TestHighestLevelOnly(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	b := e.bike(owner, "Gravel", date("2025-01-01"))

	m := only(t, e.checkSent(owner))
	if m.Body != "- Major service overdue" || m.Priority != notify.PriorityHigh {
		t.Errorf("message %+v", m)
	}
	got := e.states(owner)
	for _, l := range []string{"monthly", "minor", "major"} {
		if got["level:"+b.ID+":"+l] != "overdue" {
			t.Errorf("state of %s: %v", l, got)
		}
	}
	none(t, e.checkSent(owner))
	e.now = e.now.Add(days(2))
	none(t, e.checkSent(owner))
}

func (e *env) setEnabled(ownerID, id string, on bool) {
	e.t.Helper()
	e.must(e.svc.UpdateChannel(e.ctx, ownerID, id, notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}, Enabled: on}))
}

// Without enabled channel nothing is checked, so the states are forgotten:
// once a channel is enabled again, current conditions are notified fresh.
func TestStatesForgottenWithoutEnabledChannel(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		e := newEnv(t)
		id := e.channel(owner, "a")
		e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
		b := e.bike(owner, "Gravel", date("2026-06-01"))
		e.now = date("2026-07-02").Add(12 * time.Hour)
		if m := only(t, e.checkSent(owner)); !contains(m.Body, "Monthly check overdue") {
			t.Errorf("message %+v", m)
		}
		e.must(e.st.Notifications().SetNotificationState(e.ctx, "o2", notifyState("garmin")))

		e.setEnabled(owner, id, false)
		if got := e.states(owner); len(got) != 0 {
			t.Errorf("states after disabling: %v", got)
		}
		// Another owner's states stay.
		if got := e.states("o2"); len(got) != 1 {
			t.Errorf("states of o2: %v", got)
		}
		// Done and overdue again while disabled.
		started, err := maintenance.Start(e.ctx, e.st, owner, b.ID, "monthly", nil, e.now, 0)
		e.must(err)
		e.must(maintenance.Save(e.ctx, e.st, owner, started.ID, maintenance.Progress{}, true))
		e.now = e.now.AddDate(0, 0, 33)
		none(t, e.checkSent(owner))

		e.setEnabled(owner, id, true)
		if m := only(t, e.checkSent(owner)); !contains(m.Body, "Monthly check overdue") {
			t.Errorf("message after enabling %+v", m)
		}
	})
	t.Run("deleted", func(t *testing.T) {
		e := newEnv(t)
		first := e.channel(owner, "a")
		second := e.channel(owner, "b")
		e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
		e.bike(owner, "Gravel", date("2026-06-01"))
		only(t, e.checkSent(owner))
		e.b.take()

		// Another enabled channel: the states stay.
		e.must(e.svc.DeleteChannel(e.ctx, owner, second))
		if got := e.states(owner); len(got) != 1 {
			t.Errorf("states after deleting one: %v", got)
		}
		// Disabling the last enabled one forgets them; a disabled one left
		// doesn't matter.
		e.channel(owner, "b")
		e.setEnabled(owner, first, false)
		e.must(e.st.Notifications().SetNotificationState(e.ctx, owner, notifyState("x")))
		list, err := e.svc.Channels(e.ctx, owner)
		e.must(err)
		for _, c := range list {
			if c.Kind == "b" {
				e.must(e.svc.DeleteChannel(e.ctx, owner, c.ID))
			}
		}
		if got := e.states(owner); len(got) != 0 {
			t.Errorf("states after deleting the last enabled: %v", got)
		}
		e.setEnabled(owner, first, true)
		only(t, e.checkSent(owner))
	})
}

func TestMaxChannels(t *testing.T) {
	e := newEnv(t)
	for range notify.MaxChannels {
		e.channel(owner, "a")
	}
	_, err := e.svc.CreateChannel(e.ctx, owner, "a", notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}})
	if !errors.Is(err, notify.ErrTooManyChannels) {
		t.Fatalf("11th channel: %v", err)
	}
	if list, _ := e.svc.Channels(e.ctx, owner); len(list) != notify.MaxChannels {
		t.Errorf("%d channels", len(list))
	}
	// Per owner.
	e.channel("o2", "a")
	// After deleting one there is room again.
	list, _ := e.svc.Channels(e.ctx, owner)
	e.must(e.svc.DeleteChannel(e.ctx, owner, list[0].ID))
	e.channel(owner, "a")
}

// tarpit is a channel that takes messages only when released, or gives up
// when its context ends.
type tarpit struct {
	entered chan struct{}
	release chan struct{}
}

func (p *tarpit) Send(ctx context.Context, _ notify.Message) error {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	select {
	case <-p.release:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", notify.ErrUnreachable, ctx.Err())
	}
}

func (p *tarpit) kind() notify.Kind {
	return notify.Kind{
		ID: "slow", Fields: []notify.Field{{Name: "to"}},
		Validate: func(c notify.Config) (notify.Config, error) { return c, nil },
		Open:     func(notify.Config) (notify.Channel, error) { return p, nil },
	}
}

// A slow receiver stalls neither the check of its owner for ever nor the
// checks of other owners.
func TestSlowChannel(t *testing.T) {
	e := newEnv(t)
	pit := &tarpit{entered: make(chan struct{}, 1), release: make(chan struct{})}
	box, err := secret.New(testKey)
	e.must(err)
	e.svc = notify.New(e.st, box, notify.Options{
		BaseURL: baseURL, Kinds: []notify.Kind{pit.kind(), e.a.kind("a")}, Now: func() time.Time { return e.now },
	})
	id := e.channel(owner, "slow")
	e.channel("o2", "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	e.settings("o2", notify.ThresholdSoon, notify.EventMaintenance)
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.bike("o2", "Road", date("2026-06-01"))

	t.Run("other owners go on", func(t *testing.T) {
		done := make(chan error, 1)
		go func() { done <- e.svc.Check(e.ctx, owner) }()
		select {
		case <-pit.entered:
		case <-time.After(5 * time.Second):
			t.Fatal("not sending")
		}
		check := make(chan error, 1)
		go func() { check <- e.svc.Check(e.ctx, "o2") }()
		select {
		case err := <-check:
			e.must(err)
		case <-time.After(5 * time.Second):
			t.Fatal("o2 waits for owner")
		}
		only(t, e.a.take())
		close(pit.release)
		e.must(<-done)
		pit.release = make(chan struct{})
	})

	t.Run("deadline", func(t *testing.T) {
		defer notify.SetSendTimeout(50 * time.Millisecond)()
		e.now = date("2026-07-02").Add(12 * time.Hour) // overdue: new message
		start := time.Now()
		e.check(owner)
		if d := time.Since(start); d > 5*time.Second {
			t.Errorf("check took %v", d)
		}
		info := e.channelInfo(owner, id)
		if info.LastError != notify.ErrorUnreachable {
			t.Errorf("last error %q", info.LastError)
		}
		for k, v := range e.states(owner) {
			if strings.HasPrefix(k, "level:") && v == "overdue" {
				t.Errorf("state saved without delivery: %s %s", k, v)
			}
		}
	})
}

// A lower level that is more urgent than the highest pending one is
// notified as well.
func TestLowerLevelMoreUrgent(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	b := e.bike(owner, "Gravel", date("2025-01-01"))
	e.must(catalog.SetLevelIntervals(e.ctx, e.st, owner, map[string]store.Interval{
		"minor": {Months: ptr(int64(12))}, "major": {Months: ptr(int64(20))},
	}))
	// 18 of 20 months: major soon; minor overdue.
	e.now = date("2026-07-05")
	m := only(t, e.checkSent(owner))
	if !contains(m.Body, "Major service due soon", "Minor service overdue") || m.Priority != notify.PriorityHigh {
		t.Errorf("message %+v", m)
	}
	if got := e.states(owner); got["level:"+b.ID+":minor"] != "overdue" || got["level:"+b.ID+":major"] != "soon" {
		t.Errorf("states %v", got)
	}
	none(t, e.checkSent(owner))
}

// Channels not tried before the deadline keep their last result.
func TestDeadlineSkipsUntriedChannels(t *testing.T) {
	defer notify.SetSendTimeout(50 * time.Millisecond)()
	e := newEnv(t)
	pit := &tarpit{entered: make(chan struct{}, 1), release: make(chan struct{})}
	box, err := secret.New(testKey)
	e.must(err)
	e.svc = notify.New(e.st, box, notify.Options{
		BaseURL: baseURL, Kinds: []notify.Kind{pit.kind(), e.a.kind("a")}, Now: func() time.Time { return e.now },
	})
	slow := e.channel(owner, "slow")
	e.now = e.now.Add(time.Second) // created later: tried second
	working := e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	e.bike(owner, "Gravel", date("2026-06-01"))

	e.check(owner)
	if info := e.channelInfo(owner, slow); info.LastError != notify.ErrorUnreachable {
		t.Errorf("slow channel: %q", info.LastError)
	}
	if info := e.channelInfo(owner, working); info.LastError != "" || info.LastSentAt != nil {
		t.Errorf("untried channel: %q %v", info.LastError, info.LastSentAt)
	}
	// Not delivered: the next check tries again.
	if got := e.states(owner); len(got) != 0 {
		t.Errorf("states %v", got)
	}
}

// A check whose owner disables the last channel while it sends stores no
// states, so they stay forgotten.
func TestDisabledDuringCheck(t *testing.T) {
	e := newEnv(t)
	id := e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.a.failFn = func(notify.Message) error {
		// Runs while Check sends, outside its transactions.
		e.setEnabled(owner, id, false)
		return nil
	}
	e.check(owner)
	if len(e.a.take()) != 1 {
		t.Fatal("not sent")
	}
	if got := e.states(owner); len(got) != 0 {
		t.Errorf("states %v", got)
	}
}
