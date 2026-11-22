package server_test

import (
	"context"
	"io"
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

// getNoRedirect fetches path in the client's language without following
// redirects and returns status, headers, and body.
func (c *client) getNoRedirect(path string) (int, http.Header, string) {
	c.t.Helper()
	hc := *c.srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequest("GET", c.srv.URL+path, nil)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept-Language", c.lang)
	res, err := hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(body)
}

// postHeader posts a form without following redirects and returns status,
// headers, and body.
func (c *client) postHeader(path string, form url.Values) (int, http.Header, string) {
	c.t.Helper()
	hc := *c.srv.Client()
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequest("POST", c.srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Accept-Language", c.lang)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", c.srv.URL)
	res, err := hc.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, _ := io.ReadAll(res.Body)
	return res.StatusCode, res.Header, string(body)
}

// The channel form shows the configuration (e.g. the ntfy topic, which
// works like a password), so it must not be cached.
func TestNotificationChannelFormNoStore(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	c.lang = "en"
	id := createNtfyChannel(t, c, ntfyURL, "Phone", "tk")

	valid := url.Values{
		"name": {"Phone"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"bad topic"},
	}
	createInvalid := url.Values{"kind": {"ntfy"}, "name": {"New"}, "config.server": {ntfyURL}, "config.topic": {"bad topic"}}
	for _, tc := range []struct {
		name   string
		get    string
		post   string
		form   url.Values
		status int
	}{
		{"new", "/notifications/channels/new", "", nil, http.StatusOK},
		{"new with kind", "/notifications/channels/new?kind=ntfy", "", nil, http.StatusOK},
		{"edit", "/notifications/channels/" + id + "/edit", "", nil, http.StatusOK},
		{"create with error", "", "/notifications/channels", createInvalid, http.StatusUnprocessableEntity},
		{"update with error", "", "/notifications/channels/" + id, valid, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var status int
			var h http.Header
			if tc.get != "" {
				status, h, _ = c.getNoRedirect(tc.get)
			} else {
				status, h, _ = c.postHeader(tc.post, tc.form)
			}
			if status != tc.status {
				t.Fatalf("status %d, want %d", status, tc.status)
			}
			if got := h.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control %q, want no-store", got)
			}
		})
	}
}

// The limit of channels per owner: no 11th channel through the form, no
// "+ New" and a text that tells why.
func TestNotificationMaxChannels(t *testing.T) {
	var answer atomic.Int32
	answer.Store(http.StatusOK)
	c, ntfyURL, _ := newNotifyClient(t, &answer)
	ctx := context.Background()

	for _, lang := range []string{"en", "de"} {
		c.lang = lang
		_, _, body := c.do("GET", "/notifications", nil)
		mustContain(t, body, `href="/notifications/channels/new"`)
	}
	c.lang = "en"
	for i := range notify.MaxChannels {
		if i == notify.MaxChannels-1 {
			// Nine channels: still room.
			if status, path, _ := c.do("GET", "/notifications/channels/new", nil); status != http.StatusOK || path != "/notifications/channels/new" {
				t.Fatalf("new at 9 channels: %d %s", status, path)
			}
		}
		createNtfyChannel(t, c, ntfyURL, "Phone", "")
	}

	// 11th create: 422 with the limit text, form kept, nothing created.
	status, h, body := c.postHeader("/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {"Eleventh"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("11th create: %d, want 422", status)
	}
	mustContain(t, body, "You can have at most 10 channels.", `value="Eleventh"`)
	if h.Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", h.Get("Cache-Control"))
	}
	if list, _ := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID); len(list) != notify.MaxChannels {
		t.Errorf("%d channels, want %d", len(list), notify.MaxChannels)
	}

	// German text.
	c.lang = "de"
	status, _, body = c.postHeader("/notifications/channels", url.Values{
		"kind": {"ntfy"}, "name": {"Elf"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"},
	})
	if status != http.StatusUnprocessableEntity {
		t.Fatalf("11th create (de): %d", status)
	}
	mustContain(t, body, "Du kannst höchstens 10 Kanäle anlegen.")

	// GET new at the limit redirects to the list (with and without kind).
	for _, p := range []string{"/notifications/channels/new", "/notifications/channels/new?kind=ntfy"} {
		status, h, _ := c.getNoRedirect(p)
		if status != http.StatusSeeOther || h.Get("Location") != "/notifications" {
			t.Errorf("GET %s at the limit: %d %q, want 303 /notifications", p, status, h.Get("Location"))
		}
	}

	// The list hides "+ New" and shows the limit text.
	c.lang = "en"
	_, _, body = c.do("GET", "/notifications", nil)
	mustContain(t, body, "You can have at most 10 channels.")
	mustNotContain(t, body, `href="/notifications/channels/new"`)
	c.lang = "de"
	_, _, body = c.do("GET", "/notifications", nil)
	mustContain(t, body, "Du kannst höchstens 10 Kanäle anlegen.")
	mustNotContain(t, body, `href="/notifications/channels/new"`)

	// Updating a channel at the limit still works, and deleting one frees a slot.
	list, _ := c.st.Notifications().ListNotificationChannels(ctx, auth.FirstUserID)
	status, path, _ := c.do("POST", "/notifications/channels/"+list[0].ID, url.Values{
		"name": {"Renamed"}, "enabled": {"1"}, "config.server": {ntfyURL}, "config.topic": {"spokebook_test"},
	})
	if status != http.StatusOK || path != "/notifications" {
		t.Errorf("update at the limit: %d %s", status, path)
	}
	c.do("POST", "/notifications/channels/"+list[1].ID+"/delete", url.Values{})
	_, _, body = c.do("GET", "/notifications", nil)
	mustContain(t, body, `href="/notifications/channels/new"`)
	mustNotContain(t, body, "Du kannst höchstens 10 Kanäle anlegen.")

	// Another owner has their own limit.
	loginAsOther(t, c)
	_, _, body = c.do("GET", "/notifications", nil)
	mustContain(t, body, `href="/notifications/channels/new"`)
	mustNotContain(t, body, "Du kannst höchstens 10 Kanäle anlegen.")
}

// Without APP_SECRET_KEY and without channels the page shows only the
// hint, no channel section.
func TestNotificationsDisabledHidesChannelSection(t *testing.T) {
	c := newClient(t)
	for lang, want := range map[string]struct{ heading, empty, hint string }{
		"en": {"Channels", "No channels yet.", "APP_SECRET_KEY"},
		"de": {"Kanäle", "Noch keine Kanäle.", "APP_SECRET_KEY"},
	} {
		c.lang = lang
		_, _, body := c.do("GET", "/notifications", nil)
		mustContain(t, body, want.hint, `role="alert"`, `action="/notifications/settings"`)
		mustNotContain(t, body, "<h2>"+want.heading+"</h2>", want.empty, "<table>", `/notifications/channels/new`)
	}
}

// newBlockingClient starts a server whose ntfy client refuses private
// addresses, like the default of the app; the ntfy server runs on loopback.
func newBlockingClient(t *testing.T) (*client, string) {
	t.Helper()
	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(ntfySrv.Close)
	box, err := secret.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	c := newSeededClientWith(t, func(st store.Store) server.Config {
		return server.Config{BaseURL: "http://localhost", Notify: notify.New(st, box, notify.Options{
			BaseURL: "http://localhost", Kinds: []notify.Kind{ntfy.Kind(ntfy.NewClient(false))}, Log: slog.New(slog.DiscardHandler),
		})}
	})
	return c, ntfySrv.URL
}

// A blocked address shows its message at the channel and in the notice of
// a test, in both languages.
func TestNotificationBlockedAddress(t *testing.T) {
	c, ntfyURL := newBlockingClient(t)
	c.lang = "en"
	id := createNtfyChannel(t, c, ntfyURL, "Home server", "")

	for lang, want := range map[string]string{
		"en": "address is not allowed: link-local",
		"de": "Die Adresse des Servers ist nicht erlaubt",
	} {
		c.lang = lang
		// The test notice.
		status, path, body := c.do("POST", "/notifications/channels/"+id+"/test", url.Values{})
		if status != http.StatusOK || path != "/notifications" {
			t.Fatalf("%s test: %d %s", lang, status, path)
		}
		mustContain(t, body, `role="alert"`, "Home server: ", want, "APP_NOTIFY_ALLOW_PRIVATE=true")
		mustNotContain(t, body, "notify.channel_error", "notify.channel.test_failed")
		// The error stays at the channel on a later visit without notice.
		_, _, body = c.do("GET", "/notifications", nil)
		mustContain(t, body, `<small class="alert">`, want)
		mustNotContain(t, body, "notify.channel_error")
		// The code in the query alone works for the notice, too.
		_, _, body = c.do("GET", "/notifications?tested="+id+"&error=blocked", nil)
		mustContain(t, body, want)
	}
	ch, err := c.st.Notifications().GetNotificationChannel(context.Background(), auth.FirstUserID, id)
	if err != nil || ch.LastError != notify.ErrorBlocked || ch.LastSentAt != nil {
		t.Errorf("channel after the blocked test: %+v %v", ch, err)
	}
}
