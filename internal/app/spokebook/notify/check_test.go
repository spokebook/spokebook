package notify_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/spokebook/spokebook/internal/app/spokebook/components"
	"github.com/spokebook/spokebook/internal/app/spokebook/maintenance"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/rides/garmin"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

// installed adds a component of the type with a distance limit (km, 0 for
// none) and installs it on the bike at odometer 0.
func (e *env) installed(ownerID string, b store.Bike, typ, model string, limitKM int64, replaceBy *time.Time) store.Component {
	e.t.Helper()
	c := store.Component{
		ID: uuid.NewString(), OwnerID: ownerID, Type: typ, Manufacturer: "Shimano", Model: model,
		ReplaceBy: replaceBy, CreatedAt: e.now,
	}
	if limitKM > 0 {
		c.MaxDistanceM = ptr(limitKM * 1000)
	}
	c, err := e.st.Components().CreateComponent(e.ctx, c)
	e.must(err)
	_, err = e.st.Components().AddInstallation(e.ctx, store.Installation{
		ID: uuid.NewString(), OwnerID: ownerID, ComponentID: c.ID, BikeID: b.ID, InstalledAt: date("2026-05-01"),
		InstalledM: 0, CreatedAt: e.now,
	})
	e.must(err)
	return c
}

// reading records the bike's odometer in km at the date.
func (e *env) reading(ownerID string, b store.Bike, at string, km int64) {
	e.t.Helper()
	_, err := e.st.Odometer().AddReading(e.ctx, store.OdometerReading{
		ID: uuid.NewString(), OwnerID: ownerID, BikeID: b.ID, RecordedAt: date(at), DistanceM: km * 1000,
		Source: store.SourceManual, CreatedAt: e.now,
	})
	e.must(err)
}

func (e *env) ride(ownerID, bikeID string) store.Ride {
	e.t.Helper()
	at := e.now.Add(-time.Hour)
	r, err := e.st.Rides().CreateRide(e.ctx, store.Ride{
		ID: uuid.NewString(), OwnerID: ownerID, BikeID: bikeID, Source: "fit", ExternalID: uuid.NewString(),
		StartedAt: at, EndedAt: at.Add(time.Hour), RiddenOn: at, DistanceM: 10_000, Name: "Ride", CreatedAt: e.now,
	})
	e.must(err)
	return r
}

func (e *env) garminSource(ownerID, lastError string) {
	e.t.Helper()
	err := e.st.Rides().CreateRideSource(e.ctx, store.RideSource{
		OwnerID: ownerID, Source: garmin.Source, Credentials: []byte("sealed"), Since: date("2026-01-01"), CreatedAt: e.now,
	})
	if !errors.Is(err, store.ErrConflict) {
		e.must(err)
	}
	e.setGarminError(ownerID, lastError)
}

func (e *env) setGarminError(ownerID, lastError string) {
	e.t.Helper()
	e.must(e.st.Rides().SetRideSourceRun(e.ctx, ownerID, garmin.Source, store.RideSourceRun{LastError: lastError}))
}

// openMaintenance adds an open maintenance created age ago.
func (e *env) openMaintenance(ownerID string, b store.Bike, age time.Duration) store.Maintenance {
	e.t.Helper()
	m := store.Maintenance{
		ID: uuid.NewString(), OwnerID: ownerID, BikeID: b.ID, Title: "Winter service", PerformedAt: e.now.Add(-age),
		CreatedAt: e.now.Add(-age),
	}
	e.must(e.st.Maintenances().CreateMaintenance(e.ctx, m))
	return m
}

func only(t *testing.T, got []notify.Message) notify.Message {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("got %d messages, want 1: %+v", len(got), got)
	}
	return got[0]
}

func none(t *testing.T, got []notify.Message) {
	t.Helper()
	if len(got) != 0 {
		t.Fatalf("got %d messages, want none: %+v", len(got), got)
	}
}

// Criterion 5: each state of a due level is notified once; completing the
// maintenance resets it.
func TestMaintenanceNotifiedOncePerState(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	key := "level:" + b.ID + ":monthly"

	// 85 % of the month: due soon.
	m := only(t, e.checkSent(owner))
	if !contains(m.Title, "Gravel") || !contains(m.Body, "Monthly check due soon") || contains(m.Body, "overdue") ||
		m.Priority != notify.PriorityDefault || m.Link != baseURL+"/bikes/"+b.ID {
		t.Errorf("soon message %+v", m)
	}
	if got := e.states(owner); len(got) != 1 || got[key] != "soon" {
		t.Errorf("states %v", got)
	}
	// Same state, any number of runs: nothing.
	for range 3 {
		e.now = e.now.Add(time.Hour)
		none(t, e.checkSent(owner))
	}

	// Overdue: one more message, high priority.
	e.now = date("2026-07-02").Add(12 * time.Hour)
	m = only(t, e.checkSent(owner))
	if !contains(m.Body, "Monthly check overdue") || m.Priority != notify.PriorityHigh || len(m.Tags) != 1 || m.Tags[0] != "warning" {
		t.Errorf("overdue message %+v", m)
	}
	if got := e.states(owner); got[key] != "overdue" {
		t.Errorf("states %v", got)
	}
	none(t, e.checkSent(owner))
	e.now = e.now.Add(days(3))
	none(t, e.checkSent(owner))

	// A completed maintenance resets the level; its state is forgotten.
	started, err := maintenance.Start(e.ctx, e.st, owner, b.ID, "monthly", nil, e.now, 0)
	e.must(err)
	// While it is open the level is still overdue and was notified.
	none(t, e.checkSent(owner))
	e.must(maintenance.Save(e.ctx, e.st, owner, started.ID, maintenance.Progress{}, true))
	none(t, e.checkSent(owner))
	if got := e.states(owner); len(got) != 0 {
		t.Errorf("states after completing: %v", got)
	}
	// Due again a month later: notified again.
	e.now = e.now.AddDate(0, 0, 27) // 27 of 31 days
	m = only(t, e.checkSent(owner))
	if !contains(m.Body, "Monthly check due soon") {
		t.Errorf("second round %+v", m)
	}
	none(t, e.checkSent(owner))
}

// Criterion 6.
func TestThresholdOverdueIgnoresSoon(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdOverdue, notify.EventMaintenance)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	none(t, e.checkSent(owner)) // 85 %
	if got := e.states(owner); len(got) != 0 {
		t.Errorf("states %v", got)
	}
	e.now = date("2026-07-02")
	m := only(t, e.checkSent(owner))
	if !contains(m.Body, "overdue") || m.Link != baseURL+"/bikes/"+b.ID {
		t.Errorf("message %+v", m)
	}
	none(t, e.checkSent(owner))

	// Wear, too.
	e = newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdOverdue, notify.EventWear)
	b = e.bike(owner, "Gravel", date("2026-06-01"))
	e.installed(owner, b, "chain", "CN-HG701", 10_000, nil)
	e.reading(owner, b, "2026-06-20", 8_500)
	none(t, e.checkSent(owner))
	e.reading(owner, b, "2026-06-25", 10_000)
	only(t, e.checkSent(owner))
}

// Threshold "soon" counts soon and overdue (criterion 6, other side).
func TestThresholdSoonCountsBoth(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.bike(owner, "Road", date("2026-05-01")) // 56 days of 30: overdue at once
	got := e.checkSent(owner)
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	var soon, over int
	for _, m := range got {
		switch {
		case contains(m.Title, "Gravel") && contains(m.Body, "due soon"):
			soon++
		case contains(m.Title, "Road") && contains(m.Body, "overdue"):
			over++
		}
	}
	if soon != 1 || over != 1 {
		t.Errorf("messages %+v", got)
	}
}

// Criterion 7: component wear.
func TestWear(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventWear)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	c := e.installed(owner, b, "chain", "CN-HG701", 10_000, nil)
	key := "component:" + c.ID

	// 79 %: nothing, 80 %: soon.
	e.reading(owner, b, "2026-06-10", 7_900)
	none(t, e.checkSent(owner))
	e.reading(owner, b, "2026-06-11", 8_000)
	m := only(t, e.checkSent(owner))
	if !contains(m.Title, "Gravel") || !contains(m.Body, "Chain (Shimano CN-HG701) nearly worn") || m.Link != baseURL+"/bikes/"+b.ID {
		t.Errorf("soon message %+v", m)
	}
	if e.states(owner)[key] != "soon" {
		t.Errorf("states %v", e.states(owner))
	}
	none(t, e.checkSent(owner))
	// 100 %: at its limit.
	e.reading(owner, b, "2026-06-20", 10_000)
	m = only(t, e.checkSent(owner))
	if !contains(m.Body, "at its limit") || m.Priority != notify.PriorityHigh {
		t.Errorf("overdue message %+v", m)
	}
	none(t, e.checkSent(owner))
	if e.states(owner)[key] != "overdue" {
		t.Errorf("states %v", e.states(owner))
	}
}

func TestWearByDate(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventWear)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	// A date limit in the future and no distance limit: nothing yet.
	c := e.installed(owner, b, "tire", "GP5000", 0, ptr(date("2026-12-01")))
	none(t, e.checkSent(owner))
	// Reached: overdue even though no distance limit exists.
	c.ReplaceBy = ptr(date("2026-06-26"))
	e.must(e.st.Components().UpdateComponent(e.ctx, c))
	m := only(t, e.checkSent(owner))
	if !contains(m.Body, "at its limit") {
		t.Errorf("message %+v", m)
	}
	// A component without any limit never notifies.
	e.installed(owner, b, "saddle", "Saddle", 0, nil)
	e.reading(owner, b, "2026-06-25", 50_000)
	none(t, e.checkSent(owner))
}

func TestWearGotBetterThenWorse(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventWear)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	c := e.installed(owner, b, "chain", "CN-HG701", 10_000, nil)
	e.reading(owner, b, "2026-06-20", 10_500)
	only(t, e.checkSent(owner)) // overdue
	// The limit is raised: soon again; getting better is quiet but remembered.
	c.MaxDistanceM = ptr(int64(12_000_000))
	e.must(e.st.Components().UpdateComponent(e.ctx, c))
	none(t, e.checkSent(owner))
	if got := e.states(owner)["component:"+c.ID]; got != "soon" {
		t.Errorf("state %q, want soon", got)
	}
	none(t, e.checkSent(owner))
	// And worse again: notified again.
	c.MaxDistanceM = ptr(int64(10_000_000))
	e.must(e.st.Components().UpdateComponent(e.ctx, c))
	m := only(t, e.checkSent(owner))
	if !contains(m.Body, "at its limit") {
		t.Errorf("message %+v", m)
	}
}

// Criterion 7: removed and retired components and retired bikes never
// notify; an identical active one does.
func TestRetiredAndRemovedNeverNotify(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance, notify.EventWear, notify.EventOpenMaintenance)
	active := e.bike(owner, "Gravel", date("2026-05-01"))
	old := e.bike(owner, "OldBike", date("2026-05-01"))
	e.openMaintenance(owner, old, days(30))
	e.reading(owner, active, "2026-06-20", 20_000)
	e.reading(owner, old, "2026-06-20", 20_000)

	retired := e.installed(owner, active, "chain", "Retired", 10_000, nil)
	removed := e.installed(owner, active, "cassette", "Removed", 10_000, nil)
	oldComp := e.installed(owner, old, "chain", "OnOldBike", 10_000, nil)
	e.installed(owner, active, "brake_pad", "Alive", 10_000, nil)
	// A component that was never installed.
	loose, err := e.st.Components().CreateComponent(e.ctx, store.Component{
		ID: uuid.NewString(), OwnerID: owner, Type: "chain", Model: "Loose", MaxDistanceM: ptr(int64(1)), CreatedAt: e.now,
	})
	e.must(err)

	at := date("2026-06-21")
	e.must(components.RetireComponent(e.ctx, e.st, owner, retired.ID, at, components.ComponentRetireReasons[0], ptr(int64(20_000_000))))
	insts, err := e.st.Components().ListInstallations(e.ctx, owner, removed.ID)
	e.must(err)
	e.must(components.Remove(e.ctx, e.st, owner, insts[0].ID, at, ptr(int64(20_000_000))))
	e.must(components.RetireBike(e.ctx, e.st, owner, old.ID, at, components.BikeRetireReasons[0], ptr(int64(20_000_000))))

	m := only(t, e.checkSent(owner))
	if !contains(m.Title, "Gravel") || contains(m.Title, "OldBike") {
		t.Errorf("message for the wrong bike: %+v", m)
	}
	for _, s := range []string{"Retired", "Removed", "OnOldBike", "Loose", "Winter"} {
		if strings.Contains(m.Body, s) || strings.Contains(m.Title, s) {
			t.Errorf("message mentions %q: %+v", s, m)
		}
	}
	if !contains(m.Body, "Alive") {
		t.Errorf("active component missing: %+v", m)
	}
	for key := range e.states(owner) {
		for _, id := range []string{retired.ID, removed.ID, oldComp.ID, loose.ID, old.ID} {
			if strings.Contains(key, id) {
				t.Errorf("state for %s", key)
			}
		}
	}
}

// Criterion 8.
func TestGarmin(t *testing.T) {
	for _, tc := range []struct {
		lastError string
		want      bool
	}{
		{garmin.ErrorUnauthorized, true},
		{garmin.ErrorSecret, true},
		{garmin.ErrorRateLimited, false},
		{garmin.ErrorFailed, false},
		{"", false},
	} {
		t.Run("error="+tc.lastError, func(t *testing.T) {
			e := newEnv(t)
			e.channel(owner, "a")
			e.settings(owner, notify.ThresholdSoon, notify.EventGarmin)
			e.garminSource(owner, tc.lastError)
			got := e.checkSent(owner)
			if !tc.want {
				none(t, got)
				return
			}
			m := only(t, got)
			if !contains(m.Title, "Garmin") || m.Link != baseURL+"/rides/garmin" || m.Body == "" ||
				(tc.lastError == garmin.ErrorUnauthorized) != contains(m.Body, "refused") {
				t.Errorf("message %+v", m)
			}
			// Once, however often it is checked and whichever of the two it is.
			e.now = e.now.Add(days(2))
			none(t, e.checkSent(owner))
			other := garmin.ErrorSecret
			if tc.lastError == other {
				other = garmin.ErrorUnauthorized
			}
			e.setGarminError(owner, other)
			none(t, e.checkSent(owner))
			// Fixed, then broken again: notified again.
			e.setGarminError(owner, "")
			none(t, e.checkSent(owner))
			e.setGarminError(owner, tc.lastError)
			only(t, e.checkSent(owner))
		})
	}
	t.Run("not connected", func(t *testing.T) {
		e := newEnv(t)
		e.channel(owner, "a")
		e.settings(owner, notify.ThresholdSoon, notify.EventGarmin)
		none(t, e.checkSent(owner))
	})
	t.Run("other owner's source", func(t *testing.T) {
		e := newEnv(t)
		e.channel(owner, "a")
		e.settings(owner, notify.ThresholdSoon, notify.EventGarmin)
		e.garminSource("o2", garmin.ErrorUnauthorized)
		none(t, e.checkSent(owner))
	})
}

// Criterion 9.
func TestUnassignedRides(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventUnassigned)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	none(t, e.checkSent(owner))
	e.ride(owner, b.ID)
	discarded := e.ride(owner, "")
	e.must(e.st.Rides().SetRideDiscarded(e.ctx, owner, discarded.ID, true))
	e.ride("o2", "")
	none(t, e.checkSent(owner))

	r1, r2 := e.ride(owner, ""), e.ride(owner, "")
	m := only(t, e.checkSent(owner))
	if m.Title != "Rides without bike" || m.Body != "2 rides are not assigned to a bike." || m.Link != baseURL+"/rides?bike=none" {
		t.Errorf("message %+v", m)
	}
	if got := e.states(owner); len(got) != 1 || got["unassigned"] != "2" {
		t.Errorf("states %v", got)
	}
	// Again when more come: the state holds the number.
	none(t, e.checkSent(owner))
	r3 := e.ride(owner, "")
	if m := only(t, e.checkSent(owner)); m.Body != "3 rides are not assigned to a bike." {
		t.Errorf("body %q", m.Body)
	}
	// Fewer: stored quietly, so only growing above it notifies again.
	e.must(e.st.Rides().SetRideBike(e.ctx, owner, r1.ID, b.ID, false))
	none(t, e.checkSent(owner))
	if got := e.states(owner); got["unassigned"] != "2" {
		t.Errorf("states %v", got)
	}
	e.must(e.st.Rides().SetRideBike(e.ctx, owner, r3.ID, b.ID, false))
	none(t, e.checkSent(owner))
	e.ride(owner, "")
	if m := only(t, e.checkSent(owner)); m.Body != "2 rides are not assigned to a bike." {
		t.Errorf("body %q", m.Body)
	}

	// None left: forgotten. New ones: notified again, now just one ride.
	e.must(e.st.Rides().SetRideBike(e.ctx, owner, r2.ID, b.ID, false))
	list, _ := e.st.Rides().ListRides(e.ctx, owner)
	for _, r := range list {
		if r.BikeID == "" && !r.Discarded {
			e.must(e.st.Rides().SetRideDiscarded(e.ctx, owner, r.ID, true))
		}
	}
	none(t, e.checkSent(owner))
	if got := e.states(owner); len(got) != 0 {
		t.Errorf("states after assigning all: %v", got)
	}
	e.ride(owner, "")
	m = only(t, e.checkSent(owner))
	if m.Body != "1 ride is not assigned to a bike." {
		t.Errorf("body %q", m.Body)
	}
}

// Criterion 10.
func TestOpenMaintenance(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventOpenMaintenance) // 7 days
	b := e.bike(owner, "Gravel", date("2026-05-01"))
	// Younger and exactly as old as the limit: not yet.
	e.openMaintenance(owner, b, days(6))
	e.openMaintenance(owner, b, days(7))
	none(t, e.checkSent(owner))
	// A completed one doesn't count however old.
	done := e.openMaintenance(owner, b, days(100))
	e.must(e.st.Maintenances().SetMaintenanceCompleted(e.ctx, owner, done.ID, ptr(e.now)))
	none(t, e.checkSent(owner))
	// Another owner's bike.
	other := e.bike("o2", "Other", date("2026-05-01"))
	e.openMaintenance("o2", other, days(30))
	none(t, e.checkSent(owner))

	// Older than the limit: once.
	old := e.openMaintenance(owner, b, days(8))
	m := only(t, e.checkSent(owner))
	if !contains(m.Title, "Gravel") || !contains(m.Body, "still open") || m.Link != baseURL+"/bikes/"+b.ID {
		t.Errorf("message %+v", m)
	}
	if got := e.states(owner); len(got) != 1 || got["open:"+old.ID] != "open" {
		t.Errorf("states %v", got)
	}
	none(t, e.checkSent(owner))
	// Younger ones age into it: notified once more, together in one message.
	e.now = e.now.Add(2 * days(1))
	m = only(t, e.checkSent(owner))
	if strings.Count(m.Body, "still open") != 2 {
		t.Errorf("message %+v", m)
	}
	none(t, e.checkSent(owner))
	// Completing it forgets the state.
	e.must(e.st.Maintenances().SetMaintenanceCompleted(e.ctx, owner, old.ID, ptr(e.now)))
	e.check(owner)
	if got := e.states(owner); got["open:"+old.ID] != "" {
		t.Errorf("states %v", got)
	}
}

func TestOpenMaintenanceDays(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	ns := notify.Defaults(owner)
	ns.Events, ns.OpenDays = []string{notify.EventOpenMaintenance}, 1
	e.must(e.svc.SaveSettings(e.ctx, ns))
	b := e.bike(owner, "Gravel", date("2026-05-01"))
	e.openMaintenance(owner, b, 25*time.Hour)
	only(t, e.checkSent(owner))

	ns.OpenDays = 30
	e.must(e.svc.SaveSettings(e.ctx, ns))
	e.must(e.st.Notifications().DeleteNotificationState(e.ctx, owner, "open:"+e.mustMaintenanceID(owner, b)))
	none(t, e.checkSent(owner))
}

func (e *env) mustMaintenanceID(ownerID string, b store.Bike) string {
	e.t.Helper()
	list, err := e.st.Maintenances().ListMaintenances(e.ctx, ownerID, b.ID)
	e.must(err)
	return list[0].ID
}

// Criterion 11: events that are off are not notified.
func TestDisabledEvents(t *testing.T) {
	// Everything is due or wrong.
	prepare := func(t *testing.T, events ...string) *env {
		e := newEnv(t)
		e.channel(owner, "a")
		e.settings(owner, notify.ThresholdSoon, events...)
		b := e.bike(owner, "Gravel", date("2026-05-01"))
		e.reading(owner, b, "2026-06-20", 20_000)
		e.installed(owner, b, "chain", "CN", 10_000, nil)
		e.openMaintenance(owner, b, days(30))
		e.ride(owner, "")
		e.garminSource(owner, garmin.ErrorUnauthorized)
		return e
	}
	t.Run("none", func(t *testing.T) {
		e := prepare(t)
		none(t, e.checkSent(owner))
		if got := e.states(owner); len(got) != 0 {
			t.Errorf("states %v", got)
		}
	})
	for _, ev := range notify.Events {
		t.Run("only "+ev, func(t *testing.T) {
			e := prepare(t, ev)
			got := e.checkSent(owner)
			if len(got) == 0 {
				t.Fatal("event not notified")
			}
			states := e.states(owner)
			if len(states) == 0 {
				t.Fatal("no states")
			}
			prefix := map[string]string{
				notify.EventMaintenance: "level:", notify.EventWear: "component:", notify.EventOpenMaintenance: "open:",
				notify.EventUnassigned: "unassigned", notify.EventGarmin: "garmin",
			}[ev]
			for key := range states {
				if !strings.HasPrefix(key, prefix) {
					t.Errorf("state %q for event %s", key, ev)
				}
			}
			for _, m := range got {
				switch ev {
				case notify.EventMaintenance:
					if contains(m.Body, "Chain") || contains(m.Body, "still open") {
						t.Errorf("message %+v", m)
					}
				case notify.EventWear:
					if !contains(m.Body, "Chain") || contains(m.Body, "Monthly") {
						t.Errorf("message %+v", m)
					}
				}
			}
		})
	}
	// Events that are switched off later forget nothing; they come back as
	// "gone" states and are notified again when switched on.
	t.Run("switched off and on", func(t *testing.T) {
		e := prepare(t, notify.EventUnassigned)
		only(t, e.checkSent(owner))
		e.settings(owner, notify.ThresholdSoon)
		none(t, e.checkSent(owner))
		e.settings(owner, notify.ThresholdSoon, notify.EventUnassigned)
		only(t, e.checkSent(owner))
	})
}

// Criterion 12: one message per bike, in the language of the settings.
func TestOneMessagePerBike(t *testing.T) {
	for _, tc := range []struct {
		lang                  string
		title, soon, wear, ov string
		unassigned            string
	}{
		{"en", "Gravel needs attention", "Monthly check due soon", "Chain (Shimano CN) nearly worn", "overdue", "1 ride is not assigned to a bike."},
		{"de", "Gravel braucht Aufmerksamkeit", "Monatscheck bald fällig", "Kette (Shimano CN) bald verschlissen", "überfällig", "1 Fahrt ist keinem Rad zugeordnet."},
	} {
		t.Run(tc.lang, func(t *testing.T) {
			e := newEnv(t)
			e.channel(owner, "a")
			e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance, notify.EventWear, notify.EventUnassigned)
			e.setLanguage(owner, tc.lang)
			gravel := e.bike(owner, "Gravel", date("2026-06-01"))
			road := e.bike(owner, "Road", date("2026-05-01"))
			e.reading(owner, gravel, "2026-06-20", 8_500)
			e.installed(owner, gravel, "chain", "CN", 10_000, nil)
			e.ride(owner, "")

			got := e.checkSent(owner)
			if len(got) != 3 {
				t.Fatalf("got %d messages: %+v", len(got), got)
			}
			var g, r, u *notify.Message
			for i, m := range got {
				switch {
				case m.Link == baseURL+"/bikes/"+gravel.ID:
					g = &got[i]
				case m.Link == baseURL+"/bikes/"+road.ID:
					r = &got[i]
				case strings.HasPrefix(m.Link, baseURL+"/rides"):
					u = &got[i]
				}
			}
			if g == nil || r == nil || u == nil {
				t.Fatalf("messages %+v", got)
			}
			lines := strings.Split(g.Body, "\n")
			if g.Title != tc.title || len(lines) != 2 || !strings.HasPrefix(lines[0], "- ") || !strings.HasPrefix(lines[1], "- ") ||
				!strings.Contains(g.Body, tc.soon) || !strings.Contains(g.Body, tc.wear) {
				t.Errorf("gravel message %+v", g)
			}
			if !strings.Contains(r.Body, tc.ov) || !strings.Contains(r.Title, "Road") || strings.Contains(r.Body, "Chain") {
				t.Errorf("road message %+v", r)
			}
			if u.Body != tc.unassigned {
				t.Errorf("unassigned message %+v", u)
			}
			// A bike's message is as urgent as its most urgent item.
			if g.Priority != notify.PriorityDefault || r.Priority != notify.PriorityHigh {
				t.Errorf("priorities %d %d", g.Priority, r.Priority)
			}
			// Bikes first, then messages without a bike.
			if got[2].Link != u.Link {
				t.Errorf("order %+v", got)
			}
		})
	}
}

// One bike's message is urgent if any item is, and sends all of its lines.
func TestMessagePriorityFollowsMostUrgent(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance, notify.EventWear)
	b := e.bike(owner, "Gravel", date("2026-06-01")) // monthly: soon
	e.installed(owner, b, "chain", "CN", 10_000, nil)
	e.reading(owner, b, "2026-06-20", 11_000) // chain: overdue
	m := only(t, e.checkSent(owner))
	if m.Priority != notify.PriorityHigh || len(m.Tags) != 1 || m.Tags[0] != "warning" || strings.Count(m.Body, "\n") != 1 {
		t.Errorf("message %+v", m)
	}
}

// New items of a bike that was notified before: only the new ones are sent.
func TestOnlyNewItemsAreSent(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance, notify.EventWear)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	only(t, e.checkSent(owner))
	e.installed(owner, b, "chain", "CN", 10_000, nil)
	e.reading(owner, b, "2026-06-20", 9_000)
	m := only(t, e.checkSent(owner))
	if contains(m.Body, "Monthly") || !contains(m.Body, "Chain") {
		t.Errorf("message %+v", m)
	}
}

// Criterion 13: delivery.
func TestAllChannelsFailStatesNotSaved(t *testing.T) {
	e := newEnv(t)
	ida := e.channel(owner, "a")
	idb := e.channel(owner, "b")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	e.a.fail(notify.ErrUnreachable)
	e.b.fail(notify.ErrRejected)
	e.check(owner)
	if got := e.states(owner); len(got) != 0 {
		t.Errorf("states saved: %v", got)
	}
	if got := e.channelInfo(owner, ida); got.LastError != notify.ErrorUnreachable || got.LastSentAt != nil {
		t.Errorf("channel a %+v", got)
	}
	if got := e.channelInfo(owner, idb); got.LastError != notify.ErrorRejected || got.LastSentAt != nil {
		t.Errorf("channel b %+v", got)
	}
	// The next run (a day later) sends again, on both.
	e.a.fail(nil)
	e.b.fail(nil)
	e.now = e.now.Add(time.Hour)
	e.check(owner)
	for name, s := range map[string]*sink{"a": e.a, "b": e.b} {
		m := only(t, s.take())
		if !contains(m.Title, "Gravel") || m.Link != baseURL+"/bikes/"+b.ID {
			t.Errorf("channel %s: %+v", name, m)
		}
	}
	if got := e.states(owner); len(got) != 1 {
		t.Errorf("states %v", got)
	}
	if got := e.channelInfo(owner, ida); got.LastError != "" || got.LastSentAt == nil || !got.LastSentAt.Equal(e.now) {
		t.Errorf("channel a %+v", got)
	}
	e.check(owner)
	none(t, e.a.take())
	none(t, e.b.take())
}

func TestOneChannelFailsAnotherSucceeds(t *testing.T) {
	e := newEnv(t)
	ida := e.channel(owner, "a")
	idb := e.channel(owner, "b")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.a.fail(notify.ErrUnauthorized)
	e.check(owner)
	only(t, e.b.take())
	none(t, e.a.take())
	if got := e.states(owner); len(got) != 1 {
		t.Errorf("states %v", got)
	}
	if got := e.channelInfo(owner, ida); got.LastError != notify.ErrorUnauthorized || got.LastSentAt != nil {
		t.Errorf("channel a %+v", got)
	}
	if got := e.channelInfo(owner, idb); got.LastError != "" || got.LastSentAt == nil {
		t.Errorf("channel b %+v", got)
	}
	// The saved state means nothing is sent again, not even to the failed
	// channel.
	e.a.fail(nil)
	e.check(owner)
	none(t, e.a.take())
	none(t, e.b.take())
}

// Only the messages a channel took are remembered.
func TestPartialDelivery(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance, notify.EventUnassigned)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	e.ride(owner, "")
	e.a.failFn = func(m notify.Message) error {
		if strings.HasPrefix(m.Link, baseURL+"/rides") {
			return notify.ErrUnreachable
		}
		return nil
	}
	m := only(t, e.checkSent(owner))
	if m.Link != baseURL+"/bikes/"+b.ID {
		t.Errorf("message %+v", m)
	}
	if got := e.states(owner); len(got) != 1 || got["level:"+b.ID+":monthly"] != "soon" {
		t.Errorf("states %v", got)
	}
	e.a.fail(nil)
	m = only(t, e.checkSent(owner))
	if !strings.HasPrefix(m.Link, baseURL+"/rides") {
		t.Errorf("message %+v", m)
	}
}

// A disabled channel receives nothing and is not touched; a broken or
// unknown kind does not stop the others.
func TestChannelsOfAnOwner(t *testing.T) {
	e := newEnv(t)
	off, err := e.svc.CreateChannel(e.ctx, owner, "a", notify.ChannelInput{Name: "Off", Config: notify.Config{"to": "x"}})
	e.must(err)
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	e.bike(owner, "Gravel", date("2026-06-01"))
	none(t, e.checkSent(owner))
	if ch := e.channelInfo(owner, off); ch.LastSentAt != nil || ch.LastError != "" {
		t.Errorf("disabled channel touched: %+v", ch)
	}
	if owners, _ := e.st.Notifications().ListNotificationOwners(e.ctx); len(owners) != 0 {
		t.Errorf("owners %+v", owners)
	}

	// A channel of an unknown kind (e.g. removed from the app) fails alone.
	e.must(e.st.Notifications().CreateNotificationChannel(e.ctx, store.NotificationChannel{
		ID: "legacy", OwnerID: owner, Kind: "pigeon", Name: "Pigeon", Config: []byte("x"), Enabled: true, CreatedAt: e.now.Add(-time.Hour),
	}))
	e.channel(owner, "a")
	only(t, e.checkSent(owner))
	ch, _ := e.st.Notifications().GetNotificationChannel(e.ctx, owner, "legacy")
	if ch.LastError != notify.ErrorFailed {
		t.Errorf("legacy channel %+v", ch)
	}
	if got := e.states(owner); len(got) != 1 {
		t.Errorf("states %v", got)
	}
}

// Owners are separate.
func TestCheckOwnerIsolation(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance, notify.EventUnassigned)
	e.bike("o2", "Foreign", date("2026-05-01"))
	e.ride("o2", "")
	// o2 has no channel: nothing for anyone, nothing stored for o2.
	none(t, e.checkSent(owner))
	e.check("o2")
	none(t, e.a.take())
	if _, err := e.st.Notifications().GetNotificationSettings(e.ctx, "o2"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("settings for o2: %v", err)
	}
	if got := e.states("o2"); len(got) != 0 {
		t.Errorf("states for o2: %v", got)
	}
	// An owner that does not exist at all.
	e.check("nobody")

	// o2 gets its own, never the other owner's.
	e.channel("o2", "b")
	e.bike(owner, "Mine", date("2026-05-01"))
	e.check("o2")
	got := e.b.take() // the default events: no unassigned rides
	if len(got) != 1 || !contains(got[0].Title, "Foreign") {
		t.Fatalf("o2 got %+v", got)
	}
	for _, m := range got {
		if contains(m.Title, "Mine") || contains(m.Body, "Mine") {
			t.Errorf("o2 got a message about another owner's bike: %+v", m)
		}
	}
	none(t, e.a.take())
	m := only(t, e.checkSent(owner))
	if !contains(m.Title, "Mine") {
		t.Errorf("owner got %+v", m)
	}
}

// Check without settings saves the defaults and records the run.
func TestCheckDefaultsAndLastRun(t *testing.T) {
	e := newEnv(t)
	e.must(e.st.Notifications().CreateNotificationChannel(e.ctx, store.NotificationChannel{
		ID: "c", OwnerID: owner, Kind: "a", Name: "x", Config: mustSeal(t), Enabled: true, CreatedAt: e.now,
	}))
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.ride(owner, "")
	m := only(t, e.checkSent(owner)) // defaults: maintenance and wear, not unassigned
	if !contains(m.Title, "Gravel") {
		t.Errorf("message %+v", m)
	}
	ns, err := e.st.Notifications().GetNotificationSettings(e.ctx, owner)
	if err != nil || ns.Threshold != "soon" || ns.OpenDays != 7 || ns.Language != "en" || ns.LastRunAt == nil || !ns.LastRunAt.Equal(e.now) {
		t.Errorf("settings %+v, %v", ns, err)
	}
}

// mustSeal seals the configuration of channel "c" of the owner the way
// the service does.
func mustSeal(t *testing.T) []byte {
	t.Helper()
	box, err := secret.New(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return box.Seal([]byte(`{"to":"me"}`), owner+"/notify/c")
}

// Criterion 16: once a day per owner.
func TestRunDue(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.channel("o2", "b")
	// An owner whose only channel is disabled, one with settings but no
	// channel, and one with nothing.
	_, err := e.svc.CreateChannel(e.ctx, "o3", "a", notify.ChannelInput{Name: "x", Config: notify.Config{"to": "me"}})
	e.must(err)
	e.settings("o4", notify.ThresholdSoon, notify.EventMaintenance)
	for _, o := range []string{owner, "o2", "o3", "o4"} {
		e.bike(o, "Bike-"+o, date("2026-06-01"))
	}
	lastRun := func(o string) *time.Time {
		ns, err := e.st.Notifications().GetNotificationSettings(e.ctx, o)
		if errors.Is(err, store.ErrNotFound) {
			return nil
		}
		e.must(err)
		return ns.LastRunAt
	}

	start := e.now
	e.svc.RunDue(e.ctx)
	if got := e.a.take(); len(got) != 1 || !contains(got[0].Title, "Bike-"+owner) {
		t.Fatalf("a got %+v", got)
	}
	if got := e.b.take(); len(got) != 1 || !contains(got[0].Title, "Bike-o2") {
		t.Fatalf("b got %+v", got)
	}
	for _, o := range []string{owner, "o2"} {
		if at := lastRun(o); at == nil || !at.Equal(start) {
			t.Errorf("last run of %s: %v", o, at)
		}
	}
	// Skipped: no enabled channel.
	for _, o := range []string{"o3", "o4"} {
		if at := lastRun(o); at != nil {
			t.Errorf("last run of %s: %v", o, at)
		}
	}

	// New items appear; 23 h 59 min later nothing is checked, at 24 h it is.
	for _, o := range []string{owner, "o2"} {
		e.bike(o, "Second-"+o, date("2026-05-01"))
	}
	e.now = start.Add(24*time.Hour - time.Minute)
	e.svc.RunDue(e.ctx)
	none(t, e.a.take())
	none(t, e.b.take())
	if at := lastRun(owner); !at.Equal(start) {
		t.Errorf("last run moved to %v", at)
	}
	e.now = start.Add(24 * time.Hour)
	e.svc.RunDue(e.ctx)
	if got := e.a.take(); len(got) != 1 || !contains(got[0].Title, "Second-"+owner) {
		t.Errorf("a got %+v", got)
	}
	if got := e.b.take(); len(got) != 1 {
		t.Errorf("b got %+v", got)
	}
	if at := lastRun(owner); !at.Equal(e.now) {
		t.Errorf("last run %v", at)
	}
	// Repeated at once: nothing.
	e.svc.RunDue(e.ctx)
	none(t, e.a.take())
}

// An owner is checked on their own schedule.
func TestRunDueOwnersIndependently(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.svc.RunDue(e.ctx)
	only(t, e.a.take())
	e.now = e.now.Add(12 * time.Hour)
	e.channel("o2", "b")
	e.bike("o2", "Road", date("2026-06-01"))
	e.bike(owner, "Second", date("2026-05-01"))
	e.svc.RunDue(e.ctx)
	none(t, e.a.take()) // o1 is not due, o2 never was checked
	only(t, e.b.take())
}

// A failed send is not retried before the next day (the check is recorded
// either way), but then it is.
func TestRunDueRetriesFailureNextDay(t *testing.T) {
	e := newEnv(t)
	id := e.channel(owner, "a")
	e.bike(owner, "Gravel", date("2026-06-01"))
	e.a.fail(notify.ErrUnreachable)
	start := e.now
	e.svc.RunDue(e.ctx)
	if got := e.channelInfo(owner, id); got.LastError != notify.ErrorUnreachable {
		t.Errorf("channel %+v", got)
	}
	e.a.fail(nil)
	e.now = start.Add(time.Hour)
	e.svc.RunDue(e.ctx)
	none(t, e.a.take())
	e.now = start.Add(24 * time.Hour)
	e.svc.RunDue(e.ctx)
	only(t, e.a.take())
}

func TestRunStopsWithContext(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.bike(owner, "Gravel", date("2026-06-01"))
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() { e.svc.Run(ctx); close(done) }()
	// The first run happens at once.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		e.a.mu.Lock()
		n := len(e.a.sent)
		e.a.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run does not stop")
	}
	only(t, e.a.take())

	// An already canceled context: Run returns and sends nothing new.
	e2 := newEnv(t)
	e2.channel(owner, "a")
	e2.bike(owner, "Gravel", date("2026-06-01"))
	e2.svc.Run(ctx)
}

// Concurrent checks of an owner send each message once.
func TestConcurrentChecksSendOnce(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance)
	e.bike(owner, "Gravel", date("2026-06-01"))
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { errs <- e.svc.Check(e.ctx, owner) })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	only(t, e.a.take())
}

// The link uses the base URL without a trailing slash.
func TestLinksUseBaseURL(t *testing.T) {
	e := newEnv(t)
	e.channel(owner, "a")
	e.settings(owner, notify.ThresholdSoon, notify.EventMaintenance, notify.EventUnassigned, notify.EventGarmin)
	b := e.bike(owner, "Gravel", date("2026-06-01"))
	e.ride(owner, "")
	e.garminSource(owner, garmin.ErrorSecret)
	var links []string
	for _, m := range e.checkSent(owner) {
		links = append(links, m.Link)
		if strings.Contains(strings.TrimPrefix(m.Link, "https://"), "//") {
			t.Errorf("double slash in %q", m.Link)
		}
	}
	want := []string{baseURL + "/bikes/" + b.ID, baseURL + "/rides?bike=none", baseURL + "/rides/garmin"}
	if fmt.Sprint(links) != fmt.Sprint(want) {
		t.Errorf("links %v, want %v", links, want)
	}
}
