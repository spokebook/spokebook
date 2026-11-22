package backup_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/backup"
	"github.com/spokebook/spokebook/internal/app/spokebook/catalog"
	"github.com/spokebook/spokebook/internal/app/spokebook/components"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

func saveSettings(t *testing.T, st store.Store, owner string, ns store.NotificationSettings) {
	t.Helper()
	ns.OwnerID = owner
	must(t, st.Notifications().SaveNotificationSettings(context.Background(), ns))
}

// dataJSON returns the JSON file of a written backup.
func dataJSON(t *testing.T, st store.Store, owner string) []byte {
	t.Helper()
	var buf bytes.Buffer
	must(t, backup.Write(context.Background(), st, owner, &buf))
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	must(t, err)
	for _, f := range zr.File {
		if f.Name == backup.DataFile {
			rc, err := f.Open()
			must(t, err)
			defer func() { _ = rc.Close() }()
			data, err := io.ReadAll(rc)
			must(t, err)
			return data
		}
	}
	t.Fatal("no data file")
	return nil
}

func readImport(t *testing.T, st store.Store, owner string, data []byte) error {
	t.Helper()
	f, err := backup.Read(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	return backup.Import(context.Background(), st, owner, f)
}

// Settings are exported and imported; channels and states are not.
func TestNotificationSettingsRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := open(t, "alice")
	saveSettings(t, src, "alice", store.NotificationSettings{
		Events: []string{"maintenance", "garmin"}, Threshold: "overdue", OpenDays: 21, Language: "de",
	})
	must(t, src.Notifications().CreateNotificationChannel(ctx, store.NotificationChannel{
		ID: "ch1", OwnerID: "alice", Kind: "ntfy", Name: "Phone", Config: []byte("sealed-topic-secret"), Enabled: true, CreatedAt: time.Now(),
	}))
	must(t, src.Notifications().SetNotificationState(ctx, "alice", store.NotificationState{Key: "garmin", State: "error", UpdatedAt: time.Now()}))
	must(t, src.Notifications().SetNotificationLastRun(ctx, "alice", time.Now()))

	f := export(t, src, "alice")
	want := &backup.NotificationSettings{Events: []string{"maintenance", "garmin"}, Threshold: "overdue", OpenDays: 21, Language: "de"}
	if f.Notifications == nil || !slices.Equal(f.Notifications.Events, want.Events) || f.Notifications.Threshold != "overdue" ||
		f.Notifications.OpenDays != 21 || f.Notifications.Language != "de" {
		t.Fatalf("exported %+v", f.Notifications)
	}
	// Neither channels, their sealed configuration, states nor the time of
	// the last run are in the file.
	data := dataJSON(t, src, "alice")
	for _, s := range []string{"sealed-topic-secret", "ch1", "Phone", "ntfy", "last_run", "state", "channels", "error"} {
		if strings.Contains(string(data), s) {
			t.Errorf("backup contains %q:\n%s", s, data)
		}
	}
	if !strings.Contains(string(data), `"notifications"`) {
		t.Errorf("no notifications in backup:\n%s", data)
	}

	// The target has other settings, a channel and states of its own.
	dst := open(t, "bob")
	saveSettings(t, dst, "bob", store.NotificationSettings{
		Events: []string{"wear"}, Threshold: "soon", OpenDays: 3, Language: "en",
	})
	must(t, dst.Notifications().CreateNotificationChannel(ctx, store.NotificationChannel{
		ID: "mine", OwnerID: "bob", Kind: "ntfy", Name: "Mine", Config: []byte("my-sealed"), Enabled: true, CreatedAt: time.Now(),
	}))
	must(t, dst.Notifications().SetNotificationState(ctx, "bob", store.NotificationState{Key: "unassigned", State: "open", UpdatedAt: time.Now()}))
	run := time.Date(2026, 6, 1, 3, 0, 0, 0, time.UTC)
	must(t, dst.Notifications().SetNotificationLastRun(ctx, "bob", run))

	must(t, readImport(t, dst, "bob", data))

	got, err := dst.Notifications().GetNotificationSettings(ctx, "bob")
	must(t, err)
	if !slices.Equal(got.Events, want.Events) || got.Threshold != "overdue" || got.OpenDays != 21 || got.Language != "de" {
		t.Errorf("imported settings %+v", got)
	}
	// Channels and the last run of the target stay; its states go, so the
	// conditions of the imported data are notified fresh.
	if got.LastRunAt == nil || !got.LastRunAt.Equal(run) {
		t.Errorf("last run %v", got.LastRunAt)
	}
	chs, err := dst.Notifications().ListNotificationChannels(ctx, "bob")
	if err != nil || len(chs) != 1 || chs[0].ID != "mine" || string(chs[0].Config) != "my-sealed" || !chs[0].Enabled {
		t.Errorf("channels %+v, %v", chs, err)
	}
	if states, err := dst.Notifications().ListNotificationStates(ctx, "bob"); err != nil || len(states) != 0 {
		t.Errorf("states %+v, %v", states, err)
	}
	// Nothing leaked from the source owner.
	if chs, _ := dst.Notifications().ListNotificationChannels(ctx, "alice"); len(chs) != 0 {
		t.Errorf("channels for alice %+v", chs)
	}
	if _, err := dst.Notifications().GetNotificationSettings(ctx, "alice"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("settings for alice: %v", err)
	}
	// Exporting again gives the same.
	if g := export(t, dst, "bob"); g.Notifications == nil || !slices.Equal(g.Notifications.Events, want.Events) || g.Notifications.OpenDays != 21 {
		t.Errorf("re-export %+v", g.Notifications)
	}
}

// An owner without saved settings exports none and importing such a backup
// keeps the target's.
func TestBackupWithoutNotifications(t *testing.T) {
	ctx := context.Background()
	src := open(t, "alice")
	fill(t, src, "alice")
	if f := export(t, src, "alice"); f.Notifications != nil {
		t.Errorf("exported %+v", f.Notifications)
	}
	if data := dataJSON(t, src, "alice"); strings.Contains(string(data), "notifications") {
		t.Errorf("backup has notifications:\n%s", data)
	}
	var buf bytes.Buffer
	must(t, backup.Write(ctx, src, "alice", &buf))
	data := buf.Bytes()

	dst := open(t, "bob")
	mine := store.NotificationSettings{Events: []string{"unassigned"}, Threshold: "overdue", OpenDays: 9, Language: "de"}
	saveSettings(t, dst, "bob", mine)
	must(t, readImport(t, dst, "bob", data))
	got, err := dst.Notifications().GetNotificationSettings(ctx, "bob")
	if err != nil || !slices.Equal(got.Events, mine.Events) || got.Threshold != "overdue" || got.OpenDays != 9 || got.Language != "de" {
		t.Errorf("settings %+v, %v", got, err)
	}

	// A target without settings gets none made up.
	fresh := open(t, "carol")
	must(t, readImport(t, fresh, "carol", data))
	if _, err := fresh.Notifications().GetNotificationSettings(ctx, "carol"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("settings made up: %v", err)
	}

	// A backup of an older version (no such field) imports as well.
	for _, old := range []string{
		`{"format":"spokebook-backup","version":1}`,
		`{"format":"spokebook-backup","version":2,"bikes":[{"id":"b","name":"x"}]}`,
		`{"format":"spokebook-backup","version":2,"notifications":null}`,
	} {
		st := open(t, "dave")
		saveSettings(t, st, "dave", mine)
		if err := readImport(t, st, "dave", []byte(old)); err != nil {
			t.Errorf("%s: %v", old, err)
		}
		if got, err := st.Notifications().GetNotificationSettings(ctx, "dave"); err != nil || got.OpenDays != 9 {
			t.Errorf("%s: settings %+v, %v", old, got, err)
		}
	}
}

// Settings of a backup are checked like when saving them; a failed import
// changes nothing.
func TestInvalidNotificationSettings(t *testing.T) {
	ctx := context.Background()
	const head = `{"format":"spokebook-backup","version":2,"bikes":[{"id":"b","name":"x"}],"notifications":`
	for name, tc := range map[string]struct {
		json string
		ok   bool
	}{
		"ok":            {`{"events":["maintenance"],"threshold":"soon","open_days":7,"language":"en"}`, true},
		"no events":     {`{"events":[],"threshold":"soon","open_days":7,"language":"en"}`, true},
		"unknown event": {`{"events":["maintenance","bogus"],"threshold":"soon","open_days":7,"language":"en"}`, true},
		"bad threshold": {`{"events":[],"threshold":"never","open_days":7,"language":"en"}`, false},
		"no threshold":  {`{"events":[],"open_days":7,"language":"en"}`, false},
		"zero days":     {`{"events":[],"threshold":"soon","open_days":0,"language":"en"}`, false},
		"too many days": {`{"events":[],"threshold":"soon","open_days":366,"language":"en"}`, false},
		"negative days": {`{"events":[],"threshold":"soon","open_days":-1,"language":"en"}`, false},
		// A language without catalog falls back to the default one.
		"no catalog":       {`{"events":[],"threshold":"soon","open_days":7,"language":"fr"}`, true},
		"empty language":   {`{"events":[],"threshold":"soon","open_days":7,"language":""}`, true},
		"garbage language": {`{"events":[],"threshold":"soon","open_days":7,"language":"x y!"}`, true},
		"empty object":     {`{}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			st := open(t, "o")
			_, err := st.Bikes().CreateBike(ctx, store.Bike{ID: "keep", OwnerID: "o", Name: "Keep", CreatedAt: time.Now()})
			must(t, err)
			mine := store.NotificationSettings{Events: []string{"garmin"}, Threshold: "overdue", OpenDays: 9, Language: "de"}
			saveSettings(t, st, "o", mine)

			err = readImport(t, st, "o", []byte(head+tc.json+`}`))
			got, gerr := st.Notifications().GetNotificationSettings(ctx, "o")
			must(t, gerr)
			if tc.ok {
				if err != nil {
					t.Fatal(err)
				}
				if got.OpenDays != 7 || got.Threshold != "soon" {
					t.Errorf("settings %+v", got)
				}
				if name == "unknown event" && !slices.Equal(got.Events, []string{"maintenance"}) {
					t.Errorf("events %v", got.Events)
				}
				if want := map[string]string{"no catalog": "en", "empty language": "en", "garbage language": "en"}[name]; want != "" && got.Language != want {
					t.Errorf("language %q, want %q", got.Language, want)
				}
				return
			}
			if !errors.Is(err, backup.ErrInvalid) {
				t.Fatalf("err = %v, want ErrInvalid", err)
			}
			// Rolled back: old settings and data.
			if !slices.Equal(got.Events, mine.Events) || got.OpenDays != 9 || got.Threshold != "overdue" {
				t.Errorf("settings changed: %+v", got)
			}
			if _, err := st.Bikes().GetBike(ctx, "o", "keep"); err != nil {
				t.Errorf("data lost after failed import: %v", err)
			}
			if _, err := st.Bikes().GetBike(ctx, "o", "b"); !errors.Is(err, store.ErrNotFound) {
				t.Errorf("bike imported despite error: %v", err)
			}
		})
	}
}

// Importing replaces all other data but not the channels of the owner; the
// settings of another owner are never touched.
func TestImportLeavesOtherOwnersNotifications(t *testing.T) {
	ctx := context.Background()
	src := open(t, "alice")
	saveSettings(t, src, "alice", store.NotificationSettings{Events: []string{"wear"}, Threshold: "overdue", OpenDays: 2, Language: "de"})
	data := dataJSON(t, src, "alice")

	dst := open(t, "bob")
	other := store.NotificationSettings{Events: []string{"garmin"}, Threshold: "soon", OpenDays: 11, Language: "en"}
	saveSettings(t, dst, "bob", other)
	saveSettings(t, dst, "carol", other)
	must(t, dst.Notifications().CreateNotificationChannel(ctx, store.NotificationChannel{
		ID: "cc", OwnerID: "carol", Kind: "ntfy", Name: "C", Config: []byte("c"), Enabled: true, CreatedAt: time.Now(),
	}))
	must(t, dst.Notifications().SetNotificationState(ctx, "carol", store.NotificationState{Key: "garmin", State: "error", UpdatedAt: time.Now()}))
	must(t, readImport(t, dst, "bob", data))
	if states, _ := dst.Notifications().ListNotificationStates(ctx, "carol"); len(states) != 1 {
		t.Errorf("carol's states %+v", states)
	}
	got, err := dst.Notifications().GetNotificationSettings(ctx, "carol")
	if err != nil || got.OpenDays != 11 || !slices.Equal(got.Events, other.Events) {
		t.Errorf("carol's settings %+v, %v", got, err)
	}
	if chs, _ := dst.Notifications().ListNotificationChannels(ctx, "carol"); len(chs) != 1 {
		t.Errorf("carol's channels %+v", chs)
	}
	// JSON field names are part of the file format.
	var raw map[string]json.RawMessage
	must(t, json.Unmarshal(data, &raw))
	var n map[string]any
	must(t, json.Unmarshal(raw["notifications"], &n))
	for _, k := range []string{"events", "threshold", "open_days", "language"} {
		if _, ok := n[k]; !ok {
			t.Errorf("notifications has no %q: %v", k, n)
		}
	}
}

// blocker is a channel kind whose Send reports that it started and then
// waits until released.
type blocker struct {
	entered chan struct{}
	release chan struct{}
}

func (b *blocker) kind() notify.Kind {
	return notify.Kind{
		ID:       "block",
		Fields:   []notify.Field{{Name: "to", Required: true}},
		Validate: func(c notify.Config) (notify.Config, error) { return c, nil },
		Open:     func(notify.Config) (notify.Channel, error) { return b, nil },
	}
}

func (b *blocker) Send(ctx context.Context, _ notify.Message) error {
	close(b.entered)
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// An import waits for a running notification check of its owner, so the
// check cannot store states of the replaced data afterwards; imports of
// other owners are not held up.
func TestImportWaitsForCheck(t *testing.T) {
	ctx := context.Background()
	st := open(t, "alice")
	must(t, components.SeedTypes(ctx, st, "bob"))
	must(t, catalog.SeedSteps(ctx, st, "bob"))

	blk := &blocker{entered: make(chan struct{}), release: make(chan struct{})}
	box, err := secret.New("0123456789abcdef0123456789abcdef")
	must(t, err)
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	svc := notify.New(st, box, notify.Options{
		BaseURL: "https://bikes.example", Kinds: []notify.Kind{blk.kind()}, Now: func() time.Time { return now },
	})
	_, err = svc.CreateChannel(ctx, "alice", "block", notify.ChannelInput{
		Name: "Phone", Config: notify.Config{"to": "me"}, Enabled: true, Language: "en",
	})
	must(t, err)
	ns := notify.Defaults("alice")
	ns.Events, ns.Threshold = []string{notify.EventMaintenance}, notify.ThresholdSoon
	must(t, svc.SaveSettings(ctx, ns))
	inService := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	_, err = st.Bikes().CreateBike(ctx, store.Bike{ID: "b1", OwnerID: "alice", Name: "Gravel", InServiceAt: &inService, CreatedAt: inService})
	must(t, err)

	archive := func() backup.Archive {
		data := []byte(`{"format":"spokebook-backup","version":2}`)
		a, err := backup.Read(bytes.NewReader(data), int64(len(data)))
		must(t, err)
		return a
	}
	checkDone := make(chan error, 1)
	go func() { checkDone <- svc.Check(ctx, "alice") }()
	select {
	case <-blk.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("check does not send")
	}

	importDone := make(chan error, 1)
	go func() { importDone <- backup.Import(ctx, st, "alice", archive()) }()
	// Another owner is not blocked by the running check.
	otherDone := make(chan error, 1)
	go func() { otherDone <- backup.Import(ctx, st, "bob", archive()) }()
	select {
	case err := <-otherDone:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("import of another owner waits for the check")
	}

	select {
	case err := <-importDone:
		t.Fatalf("import returned while the check sends: %v", err)
	case <-time.After(150 * time.Millisecond):
	}

	close(blk.release)
	must(t, <-checkDone)
	select {
	case err := <-importDone:
		must(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("import does not finish after the check")
	}
	// The import ran after the check and dropped the states it stored.
	states, err := st.Notifications().ListNotificationStates(ctx, "alice")
	must(t, err)
	if len(states) != 0 {
		t.Errorf("states after import: %+v", states)
	}
	if _, err := st.Bikes().GetBike(ctx, "alice", "b1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("bike survived the import: %v", err)
	}
}
