package server_test

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spokebook/spokebook/internal/app/spokebook/auth"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify/ntfy"
	"github.com/spokebook/spokebook/internal/app/spokebook/secret"
	"github.com/spokebook/spokebook/internal/app/spokebook/server"
	"github.com/spokebook/spokebook/internal/app/spokebook/store"
)

// newNotifyClient starts a server with notifications enabled and an ntfy
// server that answers with status; it returns the ntfy server's URL and
// the Authorization header of the last request.
func newNotifyClient(t *testing.T, status *atomic.Int32) (*client, string, *atomic.Value) {
	t.Helper()
	var auth atomic.Value
	auth.Store("")
	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		w.WriteHeader(int(status.Load()))
	}))
	t.Cleanup(ntfySrv.Close)
	box, err := secret.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	c := newSeededClientWith(t, func(st store.Store) server.Config {
		return server.Config{BaseURL: "http://localhost", Notify: notify.New(st, box, notify.Options{
			BaseURL: "http://localhost", Kinds: []notify.Kind{ntfy.Kind(ntfySrv.Client())}, Log: slog.New(slog.DiscardHandler),
		})}
	})
	return c, ntfySrv.URL, &auth
}

func TestNotificationsDisabled(t *testing.T) {
	c := newClient(t)
	status, _, body := c.do("GET", "/notifications", nil)
	if status != http.StatusOK {
		t.Fatalf("status %d", status)
	}
	mustContain(t, body, "APP_SECRET_KEY", `action="/notifications/settings"`)
	mustNotContain(t, body, `href="/notifications/channels/new"`)
	if status, path, _ := c.do("GET", "/notifications/channels/new", nil); status != http.StatusOK || path != "/notifications" {
		t.Errorf("new channel without key: %d %s", status, path)
	}
}

func TestNotificationChannelFlow(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, authHeader := newNotifyClient(t, &answer)
	c.lang = "en"
	ctx := context.Background()

	_, _, body := c.do("GET", "/notifications/channels/new", nil)
	mustContain(t, body, `name="config.server" value="https://ntfy.sh"`, `type="password" name="config.token" value=""`)

	status, _, body := c.do("POST", "/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {"Phone"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"bad topic"},
		"config.token": {"tk_secret"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("invalid topic: %d", status)
	}
	mustContain(t, body, `aria-invalid="true"`, "Please enter a topic")
	mustNotContain(t, body, "tk_secret")

	status, path, _ := c.do("POST", "/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {"Phone"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"},
		"config.token": {"tk_secret"},
	})
	if status != http.StatusOK || path != "/notifications" {
		t.Fatalf("create: %d %s", status, path)
	}
	list, err := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID)
	if err != nil || len(list) != 1 {
		t.Fatalf("channels: %v %v", list, err)
	}
	id := list[0].ID

	_, _, body = c.do("GET", "/notifications/channels/"+id+"/edit", nil)
	mustContain(t, body, `value="spokebook_test"`, `name="clear" value="token"`)
	mustNotContain(t, body, "tk_secret")

	// An empty token keeps the stored one.
	c.do("POST", "/notifications/channels/"+id, url.Values{
		"name": {"Phone 2"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"},
	})
	_, _, body = c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
	mustContain(t, body, "Phone 2: Test message sent.")
	if got := authHeader.Load(); got != "Bearer tk_secret" {
		t.Errorf("auth after keeping the token: %q", got)
	}

	// Clearing removes it; a failed test shows at the channel.
	c.do("POST", "/notifications/channels/"+id, url.Values{
		"name": {"Phone 2"}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"}, "clear": {"token"},
	})
	answer.Store(http.StatusForbidden)
	_, _, body = c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
	if got := authHeader.Load(); got != "" {
		t.Errorf("auth after clearing the token: %q", got)
	}
	mustContain(t, body, "The test message could not be sent", "Paused", "The server refused access")

	_, _, body = c.do("GET", "/notifications/channels/"+id+"/delete", nil)
	mustContain(t, body, `action="/notifications/channels/`+id+`/delete"`)
	c.do("POST", "/notifications/channels/"+id+"/delete", url.Values{})
	if list, _ := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID); len(list) != 0 {
		t.Errorf("not deleted: %v", list)
	}
}

func TestNotificationSettings(t *testing.T) {
	var answer atomic.Int32
	c, _, _ := newNotifyClient(t, &answer)
	_, _, body := c.do("GET", "/notifications", nil)
	// Defaults: maintenance and wear, soon, 7 days, the browser's language.
	mustContain(t, body, `value="maintenance" checked`, `value="wear" checked`, `value="soon" checked`,
		`name="open_days" value="7"`, `value="de" selected`)
	mustNotContain(t, body, `value="garmin" checked`)

	status, _, body := c.do("POST", "/notifications/settings", url.Values{
		"event": {"garmin"}, "threshold": {"overdue"}, "open_days": {"0"}, "language": {"en"},
	})
	if status != http.StatusUnprocessableEntity || !strings.Contains(body, `aria-describedby="open_days-error"`) {
		t.Fatalf("invalid days: %d", status)
	}
	mustContain(t, body, `value="garmin" checked`, `value="overdue" checked`)

	status, path, _ := c.do("POST", "/notifications/settings", url.Values{
		"event": {"garmin"}, "threshold": {"overdue"}, "open_days": {"3"}, "language": {"en"},
	})
	if status != http.StatusOK || path != "/notifications" {
		t.Fatalf("save: %d %s", status, path)
	}
	ns, err := c.st.Notifications().GetNotificationSettings(context.Background(), auth.FirstUserID)
	if err != nil || ns.Threshold != "overdue" || ns.OpenDays != 3 || ns.Language != "en" || len(ns.Events) != 1 {
		t.Errorf("settings: %+v %v", ns, err)
	}
}
