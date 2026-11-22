package sqlite_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

func newChannel(owner, name string, enabled bool, at time.Time) store.NotificationChannel {
	return store.NotificationChannel{
		ID: newID(), OwnerID: owner, Kind: "ntfy", Name: name, Config: []byte("sealed:" + name),
		Enabled: enabled, CreatedAt: at,
	}
}

func TestNotificationChannels(t *testing.T) {
	ctx := context.Background()
	ns := newStore(t).Notifications()
	base := time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)

	second := newChannel("o1", "Second", true, base.Add(time.Hour))
	first := newChannel("o1", "First", false, base)
	for _, c := range []store.NotificationChannel{second, first} {
		if err := ns.CreateNotificationChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	other := newChannel("o2", "Other", true, base)
	if err := ns.CreateNotificationChannel(ctx, other); err != nil {
		t.Fatal(err)
	}

	// Oldest first, only the owner's.
	list, err := ns.ListNotificationChannels(ctx, "o1")
	if err != nil || len(list) != 2 || list[0].ID != first.ID || list[1].ID != second.ID {
		t.Fatalf("list %+v, %v", list, err)
	}
	got, err := ns.GetNotificationChannel(ctx, "o1", second.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Second" || got.Kind != "ntfy" || !got.Enabled || !bytes.Equal(got.Config, second.Config) ||
		got.LastError != "" || got.LastSentAt != nil || !got.CreatedAt.Equal(second.CreatedAt) {
		t.Errorf("get %+v", got)
	}
	if got.CreatedAt.Location() != time.UTC {
		t.Errorf("created at not UTC: %v", got.CreatedAt.Location())
	}
	if f, _ := ns.GetNotificationChannel(ctx, "o1", first.ID); f.Enabled {
		t.Errorf("disabled channel is enabled: %+v", f)
	}

	// Update changes name, config and enabled, nothing else.
	upd := first
	upd.Name, upd.Config, upd.Enabled = "Renamed", []byte("new-blob"), true
	upd.Kind, upd.CreatedAt = "other", base.Add(99*time.Hour)
	if err := ns.UpdateNotificationChannel(ctx, upd); err != nil {
		t.Fatal(err)
	}
	got, _ = ns.GetNotificationChannel(ctx, "o1", first.ID)
	if got.Name != "Renamed" || string(got.Config) != "new-blob" || !got.Enabled || got.Kind != "ntfy" || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("after update %+v", got)
	}

	// Result: a failure keeps the last time sent, a success sets it.
	sent := base.Add(5 * time.Hour)
	if err := ns.SetNotificationChannelResult(ctx, "o1", first.ID, "", &sent); err != nil {
		t.Fatal(err)
	}
	if err := ns.SetNotificationChannelResult(ctx, "o1", first.ID, "unreachable", nil); err != nil {
		t.Fatal(err)
	}
	got, _ = ns.GetNotificationChannel(ctx, "o1", first.ID)
	if got.LastError != "unreachable" || got.LastSentAt == nil || !got.LastSentAt.Equal(sent) {
		t.Errorf("after failure %+v", got)
	}
	if err := ns.SetNotificationChannelResult(ctx, "o1", first.ID, "", &sent); err != nil {
		t.Fatal(err)
	}
	if got, _ = ns.GetNotificationChannel(ctx, "o1", first.ID); got.LastError != "" {
		t.Errorf("error not cleared: %+v", got)
	}

	// Another owner can't see or change a channel.
	if l, _ := ns.ListNotificationChannels(ctx, "o3"); len(l) != 0 {
		t.Errorf("stranger lists %+v", l)
	}
	if _, err := ns.GetNotificationChannel(ctx, "o2", first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get other owner: %v", err)
	}
	hijack := first
	hijack.OwnerID, hijack.Name = "o2", "Hijacked"
	for name, err := range map[string]error{
		"update": ns.UpdateNotificationChannel(ctx, hijack),
		"result": ns.SetNotificationChannelResult(ctx, "o2", first.ID, "failed", nil),
		"delete": ns.DeleteNotificationChannel(ctx, "o2", first.ID),
	} {
		if !errors.Is(err, store.ErrNotFound) {
			t.Errorf("%s by other owner: %v", name, err)
		}
	}
	if got, _ = ns.GetNotificationChannel(ctx, "o1", first.ID); got.Name != "Renamed" || got.LastError != "" {
		t.Errorf("changed by other owner: %+v", got)
	}

	// Not found.
	if _, err := ns.GetNotificationChannel(ctx, "o1", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("get missing: %v", err)
	}
	missing := first
	missing.ID = "missing"
	if err := ns.UpdateNotificationChannel(ctx, missing); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("update missing: %v", err)
	}
	if err := ns.DeleteNotificationChannel(ctx, "o1", "missing"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete missing: %v", err)
	}

	// Delete.
	if err := ns.DeleteNotificationChannel(ctx, "o1", first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ns.GetNotificationChannel(ctx, "o1", first.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("deleted channel: %v", err)
	}
	if l, _ := ns.ListNotificationChannels(ctx, "o2"); len(l) != 1 {
		t.Errorf("other owner's channel gone: %+v", l)
	}
}

func TestNotificationChannelConstraints(t *testing.T) {
	ctx := context.Background()
	ns := newStore(t).Notifications()
	now := time.Now().UTC()
	for name, mod := range map[string]func(*store.NotificationChannel){
		"empty name": func(c *store.NotificationChannel) { c.Name = "" },
		"empty kind": func(c *store.NotificationChannel) { c.Kind = "" },
	} {
		c := newChannel("o1", "x", true, now)
		mod(&c)
		if err := ns.CreateNotificationChannel(ctx, c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	dup := newChannel("o1", "x", true, now)
	if err := ns.CreateNotificationChannel(ctx, dup); err != nil {
		t.Fatal(err)
	}
	if err := ns.CreateNotificationChannel(ctx, dup); !errors.Is(err, store.ErrConflict) {
		t.Errorf("duplicate ID: %v", err)
	}
}

func TestNotificationSettings(t *testing.T) {
	ctx := context.Background()
	ns := newStore(t).Notifications()

	if _, err := ns.GetNotificationSettings(ctx, "o1"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("settings before saving: %v", err)
	}
	if err := ns.SetNotificationLastRun(ctx, "o1", time.Now()); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("last run without settings: %v", err)
	}

	s := store.NotificationSettings{
		OwnerID: "o1", Events: []string{"maintenance", "wear"}, Threshold: "soon", OpenDays: 7, Language: "de",
		LastRunAt: ptrTime(time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)),
	}
	if err := ns.SaveNotificationSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := ns.GetNotificationSettings(ctx, "o1")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Events, s.Events) || got.Threshold != "soon" || got.OpenDays != 7 || got.Language != "de" || got.LastRunAt != nil {
		t.Errorf("saved %+v (LastRunAt must be ignored when saving)", got)
	}

	// Last run survives saving again.
	run := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	if err := ns.SetNotificationLastRun(ctx, "o1", run); err != nil {
		t.Fatal(err)
	}
	s.Events, s.Threshold, s.OpenDays, s.Language = []string{"garmin"}, "overdue", 30, "en"
	if err := ns.SaveNotificationSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, _ = ns.GetNotificationSettings(ctx, "o1")
	if !slices.Equal(got.Events, []string{"garmin"}) || got.Threshold != "overdue" || got.OpenDays != 30 || got.Language != "en" ||
		got.LastRunAt == nil || !got.LastRunAt.Equal(run) {
		t.Errorf("updated %+v", got)
	}

	// No events at all.
	s.Events = nil
	if err := ns.SaveNotificationSettings(ctx, s); err != nil {
		t.Fatal(err)
	}
	if got, _ = ns.GetNotificationSettings(ctx, "o1"); len(got.Events) != 0 {
		t.Errorf("events %v", got.Events)
	}

	// Constraints.
	bad := s
	bad.Threshold = "never"
	if err := ns.SaveNotificationSettings(ctx, bad); err == nil {
		t.Error("unknown threshold accepted")
	}
	bad = s
	bad.OpenDays = 0
	if err := ns.SaveNotificationSettings(ctx, bad); err == nil {
		t.Error("0 open days accepted")
	}

	// Owners are separate.
	if _, err := ns.GetNotificationSettings(ctx, "o2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("other owner's settings: %v", err)
	}
}

func ptrTime(t time.Time) *time.Time { return &t }

func TestNotificationOwners(t *testing.T) {
	ctx := context.Background()
	ns := newStore(t).Notifications()
	now := time.Now().UTC()

	if got, err := ns.ListNotificationOwners(ctx); err != nil || len(got) != 0 {
		t.Fatalf("no channels: %+v, %v", got, err)
	}
	// o1: one disabled and one enabled channel (listed once), o2: only a
	// disabled one, o3: enabled with settings and last run.
	for _, c := range []store.NotificationChannel{
		newChannel("o1", "a", false, now), newChannel("o1", "b", true, now), newChannel("o1", "c", true, now),
		newChannel("o2", "d", false, now), newChannel("o3", "e", true, now),
	} {
		if err := ns.CreateNotificationChannel(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	run := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	if err := ns.SaveNotificationSettings(ctx, store.NotificationSettings{
		OwnerID: "o3", Events: []string{"wear"}, Threshold: "soon", OpenDays: 7, Language: "en",
	}); err != nil {
		t.Fatal(err)
	}
	if err := ns.SetNotificationLastRun(ctx, "o3", run); err != nil {
		t.Fatal(err)
	}
	got, err := ns.ListNotificationOwners(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].OwnerID != "o1" || got[0].LastRunAt != nil || got[1].OwnerID != "o3" ||
		got[1].LastRunAt == nil || !got[1].LastRunAt.Equal(run) {
		t.Errorf("owners %+v", got)
	}
}

func TestNotificationStates(t *testing.T) {
	ctx := context.Background()
	ns := newStore(t).Notifications()
	at := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)

	for _, st := range []store.NotificationState{{Key: "b", State: "soon"}, {Key: "a", State: "overdue"}} {
		st.UpdatedAt = at
		if err := ns.SetNotificationState(ctx, "o1", st); err != nil {
			t.Fatal(err)
		}
	}
	if err := ns.SetNotificationState(ctx, "o2", store.NotificationState{Key: "a", State: "soon", UpdatedAt: at}); err != nil {
		t.Fatal(err)
	}
	// Setting again changes the state.
	if err := ns.SetNotificationState(ctx, "o1", store.NotificationState{Key: "b", State: "overdue", UpdatedAt: at.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	got, err := ns.ListNotificationStates(ctx, "o1")
	if err != nil || len(got) != 2 || got[0].Key != "a" || got[0].State != "overdue" || got[1].Key != "b" || got[1].State != "overdue" ||
		!got[1].UpdatedAt.Equal(at.Add(time.Hour)) {
		t.Fatalf("states %+v, %v", got, err)
	}
	// Deleting is by owner and key; a missing key is no error.
	if err := ns.DeleteNotificationState(ctx, "o2", "b"); err != nil {
		t.Errorf("delete missing: %v", err)
	}
	if err := ns.DeleteNotificationState(ctx, "o1", "a"); err != nil {
		t.Fatal(err)
	}
	if got, _ = ns.ListNotificationStates(ctx, "o1"); len(got) != 1 || got[0].Key != "b" {
		t.Errorf("after delete %+v", got)
	}
	if got, _ = ns.ListNotificationStates(ctx, "o2"); len(got) != 1 || got[0].State != "soon" {
		t.Errorf("other owner %+v", got)
	}
	// Deleting all is by owner; none left is no error.
	if err := ns.DeleteNotificationStates(ctx, "o1"); err != nil {
		t.Fatal(err)
	}
	if err := ns.DeleteNotificationStates(ctx, "o1"); err != nil {
		t.Errorf("delete none: %v", err)
	}
	if got, _ = ns.ListNotificationStates(ctx, "o1"); len(got) != 0 {
		t.Errorf("after delete all %+v", got)
	}
	if got, _ = ns.ListNotificationStates(ctx, "o2"); len(got) != 1 {
		t.Errorf("other owner after delete all %+v", got)
	}
}

func TestNotificationsInTx(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	c := newChannel("o1", "x", true, time.Now().UTC())
	errStop := errors.New("stop")
	err := s.WithTx(ctx, func(tx store.Stores) error {
		if err := tx.Notifications().CreateNotificationChannel(ctx, c); err != nil {
			return err
		}
		if err := tx.Notifications().SetNotificationState(ctx, "o1", store.NotificationState{Key: "k", State: "soon", UpdatedAt: time.Now()}); err != nil {
			return err
		}
		return errStop
	})
	if !errors.Is(err, errStop) {
		t.Fatal(err)
	}
	if l, _ := s.Notifications().ListNotificationChannels(ctx, "o1"); len(l) != 0 {
		t.Errorf("channel kept after rollback: %+v", l)
	}
	if l, _ := s.Notifications().ListNotificationStates(ctx, "o1"); len(l) != 0 {
		t.Errorf("state kept after rollback: %+v", l)
	}
}
