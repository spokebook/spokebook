package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spokebook/spokebook/internal/app/spokebook/auth"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

// createNtfyChannel creates a channel through the form and returns its ID.
func createNtfyChannel(t *testing.T, c *client, ntfyURL, name, token string) string {
	t.Helper()
	ctx := context.Background()
	before, err := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID)
	if err != nil {
		t.Fatal(err)
	}
	status, path, _ := c.do("POST", "/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {name}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"},
		"config.token": {token},
	})
	if status != http.StatusOK || path != "/notifications" {
		t.Fatalf("create channel: %d %s", status, path)
	}
	after, err := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID)
	if err != nil || len(after) != len(before)+1 {
		t.Fatalf("channels: %v %v", after, err)
	}
	return after[len(after)-1].ID
}

// loginAsOther creates a second account and makes the client use its
// session; it returns the new owner's ID.
func loginAsOther(t *testing.T, c *client) string {
	t.Helper()
	ctx := context.Background()
	u, err := auth.CreateUser(ctx, c.st, "other", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	token, _, err := auth.StartSession(ctx, c.st, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	su, _ := url.Parse(c.srv.URL)
	c.srv.Client().Jar.SetCookies(su, []*http.Cookie{{Name: "spokebook_session", Value: token}})
	return u.ID
}

// Criterion: another owner's channel is never visible or changeable.
func TestNotificationChannelOtherOwner(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, authHeader := newNotifyClient(t, &answer)
	c.lang = "en"
	id := createNtfyChannel(t, c, ntfyURL, "Secret phone", "tk_owner_secret")
	otherID := loginAsOther(t, c)

	_, _, body := c.do("GET", "/notifications", nil)
	mustNotContain(t, body, "Secret phone", id)

	form := url.Values{
		"name": {"Hijacked"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"hijacked"},
	}
	for _, tc := range []struct {
		method, path string
		form         url.Values
	}{
		{"GET", "/notifications/channels/" + id + "/edit", nil},
		{"POST", "/notifications/channels/" + id, form},
		{"POST", "/notifications/channels/" + id + "/test", url.Values{}},
		{"GET", "/notifications/channels/" + id + "/delete", nil},
		{"POST", "/notifications/channels/" + id + "/delete", url.Values{}},
	} {
		status, _, body := c.do(tc.method, tc.path, tc.form)
		if status != http.StatusNotFound {
			t.Errorf("%s %s: status %d, want 404", tc.method, tc.path, status)
		}
		mustNotContain(t, body, "Secret phone", "spokebook_test", "tk_owner_secret")
	}
	if got := authHeader.Load(); got != "" {
		t.Errorf("another owner's test sent a message: %q", got)
	}
	ctx := context.Background()
	list, err := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID)
	if err != nil || len(list) != 1 || list[0].Name != "Secret phone" || list[0].LastSentAt != nil {
		t.Errorf("channel of the first owner changed: %+v %v", list, err)
	}
	if mine, _ := c.st.Notifications().ListNotificationChannels(ctx, otherID); len(mine) != 0 {
		t.Errorf("channel appeared at the other owner: %+v", mine)
	}

	// The notice of the page does not name another owner's channel.
	_, _, body = c.do("GET", "/notifications?tested="+id, nil)
	mustNotContain(t, body, "Secret phone", "Test message sent")

	// Settings are per owner.
	c.do("POST", "/notifications/settings", url.Values{
		"event": {"garmin"}, "threshold": {"overdue"}, "open_days": {"2"}, "language": {"en"},
	})
	ns, err := c.st.Notifications().GetNotificationSettings(ctx, auth.FirstUserID)
	if err == nil && ns.OpenDays == 2 {
		t.Errorf("settings of the first owner overwritten: %+v", ns)
	}
}

// Criterion: unknown ids and error codes in the query can't spoof the
// notice at the top of the page.
func TestNotificationNoticeNotSpoofable(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	id := createNtfyChannel(t, c, ntfyURL, "Phone <b>x</b>", "")

	for _, q := range []string{
		"tested=unknown&error=rejected",
		"tested=" + url.QueryEscape("<script>alert(1)</script>") + "&error=failed",
		"error=rejected",
		"tested=&error=failed",
	} {
		_, _, body := c.do("GET", "/notifications?"+q, nil)
		mustNotContain(t, body, "could not be sent", "Test message sent", "<script>alert")
	}

	// A known channel with an unknown error code names no error text and
	// the code is not echoed.
	_, _, body := c.do("GET", "/notifications?tested="+id+"&error="+url.QueryEscape("<i>boom</i>"), nil)
	mustNotContain(t, body, "<i>boom</i>", "could not be sent", "notify.channel_error")
	// The channel name is escaped.
	mustContain(t, body, "Phone &lt;b&gt;x&lt;/b&gt;")
	mustNotContain(t, body, "Phone <b>x</b>")

	// Every known code has a translated text in both languages.
	for _, lang := range []string{"en", "de"} {
		c.lang = lang
		for _, code := range []string{"unauthorized", "rejected", "unreachable", "secret", "failed"} {
			_, _, body := c.do("GET", "/notifications?tested="+id+"&error="+code, nil)
			mustContain(t, body, `role="alert"`)
			mustNotContain(t, body, "notify.channel_error", "notify.channel.test_failed")
		}
	}
}

// Criterion: failures show at the channel for each kind of error.
func TestNotificationTestErrors(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   string
	}{
		{http.StatusUnauthorized, "The server refused access"},
		{http.StatusForbidden, "The server refused access"},
		{http.StatusBadRequest, "The server refused the message"},
		{http.StatusInternalServerError, "The server"},
	} {
		var answer atomic.Int32
		answer.Store(http.StatusOK)
		c, ntfyURL, _ := newNotifyClient(t, &answer)
		c.lang = "en"
		id := createNtfyChannel(t, c, ntfyURL, "Phone", "")
		answer.Store(int32(tc.status))
		status, path, body := c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
		if status != http.StatusOK || path != "/notifications" {
			t.Fatalf("%d: test: %d %s", tc.status, status, path)
		}
		mustContain(t, body, "Phone: The test message could not be sent", tc.want)
		// The error stays at the channel, also on a later visit without notice.
		_, _, body = c.do("GET", "/notifications", nil)
		mustContain(t, body, `class="alert"`, tc.want)
		mustNotContain(t, body, "could not be sent")
	}

	// Unreachable server.
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, _, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	id := createNtfyChannel(t, c, deadURL, "Gone", "")
	_, _, body := c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
	mustContain(t, body, "Gone: The test message could not be sent", "could not be reached")
}

// A successful test after a failure removes the error and shows the
// last sending.
func TestNotificationTestRecovers(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusForbidden)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	id := createNtfyChannel(t, c, ntfyURL, "Phone", "")
	_, _, body := c.do("GET", "/notifications", nil)
	mustContain(t, body, "Never")
	c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
	answer.Store(http.StatusOK)
	_, _, body = c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
	mustContain(t, body, "Phone: Test message sent.")
	mustNotContain(t, body, "The server refused access", "Never")
	ch, err := c.st.Notifications().GetNotificationChannel(context.Background(), auth.FirstUserID, id)
	if err != nil || ch.LastError != "" || ch.LastSentAt == nil || time.Since(*ch.LastSentAt) > time.Minute {
		t.Errorf("channel after the test: %+v %v", ch, err)
	}
}

// Criterion: the token is never echoed, not after a validation error either.
func TestNotificationTokenNeverEchoed(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	id := createNtfyChannel(t, c, ntfyURL, "Phone", "tk_stored_token")

	// Edit page: the stored token is not shown.
	_, _, body := c.do("GET", "/notifications/channels/"+id+"/edit", nil)
	mustNotContain(t, body, "tk_stored_token")
	mustContain(t, body, `type="password" name="config.token" value=""`, `name="clear" value="token"`)
	// A stored token makes the field optional.
	if m := regexp.MustCompile(`<input[^>]*name="config.token"[^>]*>`).FindString(body); strings.Contains(m, "required") {
		t.Errorf("token field is required: %s", m)
	}

	// Update with an invalid topic and a new token: error, no echo.
	status, _, body := c.do("POST", "/notifications/channels/"+id, url.Values{
		"name": {"Phone"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"bad topic"},
		"config.token": {"tk_typed_new"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid update: %d", status)
	}
	mustContain(t, body, `aria-invalid="true"`, `value="bad topic"`)
	mustNotContain(t, body, "tk_typed_new", "tk_stored_token")
	// The stored token is still there (the field stays optional).
	mustContain(t, body, `name="clear" value="token"`)

	// Empty name: 422, name error.
	status, _, body = c.do("POST", "/notifications/channels/"+id, url.Values{
		"name": {"  "}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"}, "config.token": {"tk_typed_new"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("empty name: %d", status)
	}
	mustContain(t, body, `aria-describedby="name-error"`)
	mustNotContain(t, body, "tk_typed_new", "tk_stored_token")

	// An invalid update changed nothing.
	ch, _ := c.st.Notifications().GetNotificationChannel(context.Background(), auth.FirstUserID, id)
	if ch.Name != "Phone" {
		t.Errorf("name after invalid update: %q", ch.Name)
	}

	// Clear checkbox stays checked after an error.
	_, _, body = c.do("POST", "/notifications/channels/"+id, url.Values{
		"name": {"Phone"}, "config.server": {ntfyURL}, "config.topic": {"bad topic"}, "clear": {"token"},
	})
	mustContain(t, body, `name="clear" value="token" checked`)

	// A new channel without a token is fine; with a bad URL it shows the error.
	status, _, body = c.do("POST", "/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {"New"}, "config.server": {"not a url"}, "config.topic": {"abc"}, "config.token": {"tk_new_chan"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("bad server: %d", status)
	}
	mustContain(t, body, `aria-invalid="true"`, `value="not a url"`)
	mustNotContain(t, body, "tk_new_chan")
}

// Criterion: the form checks required fields and keeps the entered values;
// a disabled channel stays paused.
func TestNotificationChannelValidationAndPause(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	ctx := context.Background()

	for name, form := range map[string]url.Values{
		"no name":      {"kind": {"ntfy"}, "name": {""}, "config.server": {ntfyURL}, "config.topic": {"abc"}},
		"no topic":     {"kind": {"ntfy"}, "name": {"A"}, "config.server": {ntfyURL}, "config.topic": {""}},
		"long name":    {"kind": {"ntfy"}, "name": {strings.Repeat("x", 101)}, "config.server": {ntfyURL}, "config.topic": {"abc"}},
		"topic slash":  {"kind": {"ntfy"}, "name": {"A"}, "config.server": {ntfyURL}, "config.topic": {"a/b"}},
		"ftp server":   {"kind": {"ntfy"}, "name": {"A"}, "config.server": {"ftp://example.org"}, "config.topic": {"abc"}},
		"long topic":   {"kind": {"ntfy"}, "name": {"A"}, "config.server": {ntfyURL}, "config.topic": {strings.Repeat("t", 65)}},
		"missing kind": {"name": {"A"}, "config.server": {ntfyURL}, "config.topic": {"abc"}},
	} {
		status, _, _ := c.do("POST", "/notifications/channels", form)
		want := http.StatusUnprocessableEntity
		if name == "missing kind" {
			want = http.StatusNotFound
		}
		if status != want {
			t.Errorf("%s: status %d, want %d", name, status, want)
		}
	}
	if list, _ := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID); len(list) != 0 {
		t.Errorf("invalid channels stored: %+v", list)
	}

	// Created without "enabled": paused, with the badge.
	status, _, _ := c.do("POST", "/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {"Quiet"}, "config.server": {ntfyURL}, "config.topic": {"abc"},
	})
	if status != http.StatusOK {
		t.Fatalf("create: %d", status)
	}
	list, _ := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID)
	if len(list) != 1 || list[0].Enabled {
		t.Fatalf("channel: %+v", list)
	}
	_, _, body := c.do("GET", "/notifications", nil)
	mustContain(t, body, "Paused", "Quiet")
	// Ticking the box enables the channel.
	c.do("POST", "/notifications/channels/"+list[0].ID, url.Values{
		"name": {"Quiet"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"abc"},
	})
	if ch, _ := c.st.Notifications().GetNotificationChannel(ctx, auth.FirstUserID, list[0].ID); !ch.Enabled {
		t.Errorf("channel not enabled by the form")
	}
	_, _, body = c.do("GET", "/notifications/channels/"+list[0].ID+"/edit", nil)
	mustContain(t, body, `name="enabled" value="1" checked`)

	// An empty server is the default server.
	if status, _, _ := c.do("POST", "/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {"Default"}, "config.server": {""}, "config.topic": {"abc"},
	}); status != http.StatusOK {
		t.Errorf("empty server: %d", status)
	}
}

// The kind chosen by the query: unknown kinds are 404, no kind with one
// kind available is the form of that kind.
func TestNotificationNewChannelKind(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, _, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	if status, _, _ := c.do("GET", "/notifications/channels/new?kind=carrier-pigeon", nil); status != http.StatusNotFound {
		t.Errorf("unknown kind: %d", status)
	}
	status, _, body := c.do("GET", "/notifications/channels/new?kind=ntfy", nil)
	if status != http.StatusOK {
		t.Fatalf("ntfy kind: %d", status)
	}
	mustContain(t, body, `name="kind" value="ntfy"`, `name="config.topic"`, `<span class="req" aria-hidden="true">*</span>`)
	mustNotContain(t, body, `href="/notifications/channels/`)
	// No delete button for a new channel.
	mustNotContain(t, body, "danger")
	if status, _, _ := c.do("GET", "/notifications/channels/does-not-exist/edit", nil); status != http.StatusNotFound {
		t.Errorf("unknown channel: %d", status)
	}
	if status, _, _ := c.do("POST", "/notifications/channels/does-not-exist", url.Values{"name": {"x"}}); status != http.StatusNotFound {
		t.Errorf("update unknown channel: %d", status)
	}
	if status, _, _ := c.do("POST", "/notifications/channels/does-not-exist/test", url.Values{}); status != http.StatusNotFound {
		t.Errorf("test unknown channel: %d", status)
	}
	if status, _, _ := c.do("GET", "/notifications/channels/does-not-exist/delete", nil); status != http.StatusNotFound {
		t.Errorf("confirm delete unknown channel: %d", status)
	}
	if status, _, _ := c.do("POST", "/notifications/channels/does-not-exist/delete", url.Values{}); status != http.StatusNotFound {
		t.Errorf("delete unknown channel: %d", status)
	}
}

// Criterion: the delete page asks, cancel leads back, only the POST deletes.
func TestNotificationChannelDeleteConfirm(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	id := createNtfyChannel(t, c, ntfyURL, "Phone", "")

	_, _, body := c.do("GET", "/notifications/channels/"+id+"/edit", nil)
	mustContain(t, body, `href="/notifications/channels/`+id+`/delete"`, `outline danger`)
	// The delete button is the last action of the page.
	if strings.LastIndex(body, "/delete") < strings.LastIndex(body, `type="submit"`) {
		t.Errorf("the delete button should come after the save button")
	}

	status, _, body := c.do("GET", "/notifications/channels/"+id+"/delete", nil)
	if status != http.StatusOK {
		t.Fatalf("confirm: %d", status)
	}
	mustContain(t, body, "Delete the channel", "Phone", `action="/notifications/channels/`+id+`/delete"`,
		`href="/notifications/channels/`+id+`/edit"`)
	if list, _ := c.st.Notifications().ListNotificationChannels(context.Background(), auth.FirstUserID); len(list) != 1 {
		t.Errorf("GET deleted the channel")
	}
	status, path, _ := c.do("POST", "/notifications/channels/"+id+"/delete", url.Values{})
	if status != http.StatusOK || path != "/notifications" {
		t.Errorf("delete: %d %s", status, path)
	}
	_, _, body = c.do("GET", "/notifications", nil)
	mustNotContain(t, body, "Phone")
	mustContain(t, body, "No channel")
	// Deleting again is 404.
	if status, _, _ := c.do("POST", "/notifications/channels/"+id+"/delete", url.Values{}); status != http.StatusNotFound {
		t.Errorf("second delete: %d", status)
	}
}

// Criterion: settings can be saved, invalid input is 422 and keeps what
// was entered; without a language catalog or a number it is refused.
func TestNotificationSettingsValidation(t *testing.T) {
	var answer atomic.Int32
	c, _, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	ctx := context.Background()

	for name, form := range map[string]url.Values{
		"threshold":     {"event": {"garmin"}, "threshold": {"never"}, "open_days": {"7"}, "language": {"en"}},
		"no threshold":  {"event": {"garmin"}, "open_days": {"7"}, "language": {"en"}},
		"days text":     {"threshold": {"soon"}, "open_days": {"abc"}, "language": {"en"}},
		"days empty":    {"threshold": {"soon"}, "open_days": {""}, "language": {"en"}},
		"days negative": {"threshold": {"soon"}, "open_days": {"-1"}, "language": {"en"}},
		"days huge":     {"threshold": {"soon"}, "open_days": {"100000"}, "language": {"en"}},
		"language":      {"threshold": {"soon"}, "open_days": {"7"}, "language": {"xx"}},
		"language bad":  {"threshold": {"soon"}, "open_days": {"7"}, "language": {"not a tag"}},
		"language none": {"threshold": {"soon"}, "open_days": {"7"}},
	} {
		status, _, body := c.do("POST", "/notifications/settings", form)
		if status != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d, want 422", name, status)
			continue
		}
		mustContain(t, body, `class="alert"`)
		mustNotContain(t, body, "notify.error")
	}
	if _, err := c.st.Notifications().GetNotificationSettings(ctx, auth.FirstUserID); err == nil {
		t.Errorf("invalid settings were saved")
	}

	// An entered value survives the error.
	_, _, body := c.do("POST", "/notifications/settings", url.Values{
		"event": {"garmin"}, "threshold": {"overdue"}, "open_days": {"9999"}, "language": {"en"},
	})
	mustContain(t, body, `name="open_days" value="9999"`, `value="overdue" checked`, `value="garmin" checked`)

	// No event at all is valid; unknown events are dropped.
	status, _, body := c.do("POST", "/notifications/settings", url.Values{
		"event": {"bogus"}, "threshold": {"soon"}, "open_days": {"30"}, "language": {"de"},
	})
	if status != http.StatusOK {
		t.Fatalf("no events: %d", status)
	}
	mustContain(t, body, "Settings saved.", `name="open_days" value="30"`, `value="de" selected`)
	mustNotContain(t, body, `name="event" value="maintenance" checked`, `name="event" value="wear" checked`, "bogus")
	ns, err := c.st.Notifications().GetNotificationSettings(ctx, auth.FirstUserID)
	if err != nil || len(ns.Events) != 0 || ns.OpenDays != 30 || ns.Language != "de" || ns.Threshold != "soon" {
		t.Errorf("settings: %+v %v", ns, err)
	}
	// The notice is a query parameter only: later visits do not show it.
	_, _, body = c.do("GET", "/notifications", nil)
	mustNotContain(t, body, "Settings saved.")
}

// Defaults follow the browser's language until settings are saved.
func TestNotificationSettingsDefaultLanguage(t *testing.T) {
	var answer atomic.Int32
	c, _, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	_, _, body := c.do("GET", "/notifications", nil)
	mustContain(t, body, `value="en" selected`)
	mustNotContain(t, body, `value="de" selected`)
	c.lang = "de"
	_, _, body = c.do("GET", "/notifications", nil)
	mustContain(t, body, `value="de" selected`)
	// An unsupported language falls back to the default.
	c.lang = "fr"
	if status, _, body := c.do("GET", "/notifications", nil); status != http.StatusOK || !strings.Contains(body, " selected") {
		t.Errorf("unsupported browser language: %d", status)
	}
}

// Without APP_SECRET_KEY nothing about channels can be created, edited or
// tested, but settings and deleting still work.
func TestNotificationsDisabledFlows(t *testing.T) {
	c := newClient(t)
	c.lang = "en"
	ctx := context.Background()
	st := c.st.Notifications()
	if err := st.CreateNotificationChannel(ctx, store.NotificationChannel{
		ID: "11111111-1111-7111-8111-111111111111", OwnerID: auth.FirstUserID, Kind: "ntfy", Name: "Old phone",
		Config: []byte("sealed"), Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	const id = "11111111-1111-7111-8111-111111111111"

	_, _, body := c.do("GET", "/notifications", nil)
	mustContain(t, body, "APP_SECRET_KEY", "Old phone", `href="/notifications/channels/`+id+`/delete"`)
	mustNotContain(t, body, `/notifications/channels/new`, `/test"`, `/edit"`)

	for _, tc := range []struct {
		method, path string
		form         url.Values
	}{
		{"GET", "/notifications/channels/new?kind=ntfy", nil},
		{"POST", "/notifications/channels", url.Values{"kind": {"ntfy"}, "name": {"A"}, "config.server": {"https://ntfy.sh"}, "config.topic": {"abc"}}},
		{"GET", "/notifications/channels/" + id + "/edit", nil},
		{"POST", "/notifications/channels/" + id, url.Values{"name": {"X"}}},
		{"POST", "/notifications/channels/" + id + "/test", url.Values{}},
	} {
		status, path, _ := c.do(tc.method, tc.path, tc.form)
		if status != http.StatusOK || path != "/notifications" {
			t.Errorf("%s %s: %d %s, want redirect to /notifications", tc.method, tc.path, status, path)
		}
	}
	if list, _ := st.ListNotificationChannels(ctx, auth.FirstUserID); len(list) != 1 || list[0].Name != "Old phone" {
		t.Errorf("channels changed: %+v", list)
	}

	// Settings work.
	status, path, _ := c.do("POST", "/notifications/settings", url.Values{
		"event": {"wear"}, "threshold": {"overdue"}, "open_days": {"5"}, "language": {"en"},
	})
	if status != http.StatusOK || path != "/notifications" {
		t.Errorf("settings: %d %s", status, path)
	}

	// Delete works with a cancel link that does not lead to the disabled edit page.
	_, _, body = c.do("GET", "/notifications/channels/"+id+"/delete", nil)
	mustContain(t, body, `action="/notifications/channels/`+id+`/delete"`, `href="/notifications"`)
	mustNotContain(t, body, `/edit"`)
	c.do("POST", "/notifications/channels/"+id+"/delete", url.Values{})
	if list, _ := st.ListNotificationChannels(ctx, auth.FirstUserID); len(list) != 0 {
		t.Errorf("not deleted: %+v", list)
	}
}

// A channel whose stored settings can't be opened (APP_SECRET_KEY changed)
// is marked and can be repaired by entering the settings again.
func TestNotificationBrokenChannel(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, authHeader := newNotifyClient(t, &answer)
	c.lang = "en"
	ctx := context.Background()
	const id = "22222222-2222-7222-8222-222222222222"
	if err := c.st.Notifications().CreateNotificationChannel(ctx, store.NotificationChannel{
		ID: id, OwnerID: auth.FirstUserID, Kind: "ntfy", Name: "Broken one", Config: []byte("garbage"),
		Enabled: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	_, _, body := c.do("GET", "/notifications", nil)
	mustContain(t, body, "Broken one", "can&#39;t be decrypted")
	status, _, body := c.do("GET", "/notifications/channels/"+id+"/edit", nil)
	if status != http.StatusOK {
		t.Fatalf("edit: %d", status)
	}
	mustContain(t, body, `role="alert"`, "can&#39;t be decrypted")
	mustNotContain(t, body, `name="clear"`)

	_, _, body = c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
	mustContain(t, body, "Broken one: The test message could not be sent")
	if got := authHeader.Load(); got != "" {
		t.Errorf("a broken channel sent something: %q", got)
	}

	status, path, _ := c.do("POST", "/notifications/channels/"+id, url.Values{
		"name": {"Repaired"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"abc"}, "config.token": {"tk_x"},
	})
	if status != http.StatusOK || path != "/notifications" {
		t.Fatalf("repair: %d %s", status, path)
	}
	_, _, body = c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
	mustContain(t, body, "Repaired: Test message sent.")
	if got := authHeader.Load(); got != "Bearer tk_x" {
		t.Errorf("auth after repair: %q", got)
	}
}

// Every state-changing notification request is refused from another origin.
func TestNotificationsCrossOriginRejected(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, authHeader := newNotifyClient(t, &answer)
	id := createNtfyChannel(t, c, ntfyURL, "Phone", "")
	for _, path := range []string{
		"/notifications/settings",
		"/notifications/channels",
		"/notifications/channels/" + id,
		"/notifications/channels/" + id + "/test",
		"/notifications/channels/" + id + "/delete",
	} {
		req, _ := http.NewRequest("POST", c.srv.URL+path, strings.NewReader("name=x&threshold=soon&open_days=1&language=en"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://evil.example")
		res, err := c.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s: status %d, want 403", path, res.StatusCode)
		}
	}
	list, _ := c.st.Notifications().ListNotificationChannels(context.Background(), auth.FirstUserID)
	if len(list) != 1 || list[0].Name != "Phone" {
		t.Errorf("channel changed by a cross-origin request: %+v", list)
	}
	if got := authHeader.Load(); got != "" {
		t.Errorf("cross-origin request sent a message: %q", got)
	}
}

// Requests without a session go to the login, for all notification routes.
func TestNotificationsNeedLogin(t *testing.T) {
	c := newAnonymousClient(t)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/notifications"},
		{"GET", "/notifications/channels/new"},
		{"GET", "/notifications/channels/x/edit"},
		{"GET", "/notifications/channels/x/delete"},
	} {
		_, path, body := c.do(tc.method, tc.path, nil)
		if path == tc.path || strings.Contains(body, "notify.") {
			t.Errorf("%s %s served without login (ended at %s)", tc.method, tc.path, path)
		}
	}
}

// Both languages render the pages without missing message IDs, the account
// page links to them, and the pages follow the UI conventions.
func TestNotificationPagesLanguagesAndConventions(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	id := createNtfyChannel(t, c, ntfyURL, "Phone", "tk")
	idRE := regexp.MustCompile(`\b(notify|account|action)\.[a-z_]+(\.[a-z_]+)*\b`)

	for _, lang := range []string{"en", "de"} {
		c.lang = lang
		for _, path := range []string{
			"/notifications", "/notifications/channels/new", "/notifications/channels/new?kind=ntfy",
			"/notifications/channels/" + id + "/edit", "/notifications/channels/" + id + "/delete",
			"/notifications?saved=1", "/notifications?tested=" + id, "/notifications?tested=" + id + "&error=failed",
			"/account",
		} {
			status, _, body := c.do("GET", path, nil)
			if status != http.StatusOK {
				t.Errorf("%s %s: status %d", lang, path, status)
				continue
			}
			if m := idRE.FindString(body); m != "" && !strings.Contains(m, "notifications.") {
				t.Errorf("%s %s: message ID shown in the page: %s", lang, path, m)
			}
			if !strings.Contains(body, `<html lang="`+lang+`"`) {
				t.Errorf("%s %s: html lang is not %s", lang, path, lang)
			}
		}
	}

	c.lang = "en"
	_, _, body := c.do("GET", "/notifications", nil)
	mustContain(t, body, `<nav aria-label="breadcrumb">`, `href="/account"`, `<div class="page-head">`,
		`href="/notifications/channels/new" role="button"`, `<div class="overflow-auto">`, `<th>`,
		`data-label="Type"`, `data-label="Status"`, `data-label="Last sent"`, `class="row-action"`,
		`class="req" aria-hidden="true"`, `<button type="submit">Save</button>`, `<button type="submit" class="outline">`)
	// The "+ New" button is filled, not outline.
	if m := regexp.MustCompile(`<a href="/notifications/channels/new"[^>]*>`).FindString(body); strings.Contains(m, "outline") {
		t.Errorf("new button is not filled: %s", m)
	}
	// Every list cell but the first and the actions carries a label.
	row := regexp.MustCompile(`(?s)<tbody>.*?</tbody>`).FindString(body)
	for _, td := range regexp.MustCompile(`<td[^>]*>`).FindAllString(row, -1) {
		if !strings.Contains(td, "data-label") && !strings.Contains(td, "row-action") && td != "<td>" {
			t.Errorf("cell without data-label: %s", td)
		}
	}
	// The forms post to the server, so they work without JavaScript; no
	// inline scripts (strict CSP).
	mustNotContain(t, body, "<script>", "onclick", "style=")

	_, _, body = c.do("GET", "/notifications/channels/"+id+"/edit", nil)
	mustContain(t, body, `<nav aria-label="breadcrumb">`, `href="/notifications"`, `<span class="req" aria-hidden="true">*</span>`,
		`<button type="submit">Save</button>`, `class="outline danger"`)
	mustNotContain(t, body, "<script>", "onclick", "style=")

	// The account page links to the notifications.
	_, _, body = c.do("GET", "/account", nil)
	mustContain(t, body, `href="/notifications"`)
}
