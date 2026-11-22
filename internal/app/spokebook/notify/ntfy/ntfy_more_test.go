package ntfy_test

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify/ntfy"
)

func TestValidateRules(t *testing.T) {
	long := "https://" + strings.Repeat("a", 2000) + ".example"
	for _, tc := range []struct {
		name  string
		cfg   notify.Config
		field string // "" is valid
		want  notify.Config
	}{
		{"defaults", notify.Config{"topic": "ok"}, "", notify.Config{"server": "https://ntfy.sh", "topic": "ok"}},
		{"blank server is default", notify.Config{"server": "  ", "topic": "ok"}, "", notify.Config{"server": "https://ntfy.sh", "topic": "ok"}},
		{"trims and drops slash", notify.Config{"server": " https://ntfy.example.org/ ", "topic": " ok ", "token": " tk "}, "", notify.Config{"server": "https://ntfy.example.org", "topic": "ok", "token": "tk"}},
		{"http with port and path", notify.Config{"server": "http://10.0.0.5:8080/ntfy", "topic": "ok"}, "", notify.Config{"server": "http://10.0.0.5:8080/ntfy", "topic": "ok"}},
		{"unknown fields dropped", notify.Config{"topic": "ok", "evil": "x"}, "", notify.Config{"server": "https://ntfy.sh", "topic": "ok"}},
		{"topic chars", notify.Config{"topic": "aZ09_-"}, "", notify.Config{"server": "https://ntfy.sh", "topic": "aZ09_-"}},
		{"topic 64", notify.Config{"topic": strings.Repeat("a", 64)}, "", nil},

		{"ftp", notify.Config{"server": "ftp://x", "topic": "ok"}, "server", nil},
		{"no scheme", notify.Config{"server": "ntfy.sh", "topic": "ok"}, "server", nil},
		{"javascript", notify.Config{"server": "javascript:alert(1)", "topic": "ok"}, "server", nil},
		{"file", notify.Config{"server": "file:///etc/passwd", "topic": "ok"}, "server", nil},
		{"no host", notify.Config{"server": "https://", "topic": "ok"}, "server", nil},
		{"path only", notify.Config{"server": "/ntfy", "topic": "ok"}, "server", nil},
		{"userinfo", notify.Config{"server": "https://u:p@x.example", "topic": "ok"}, "server", nil},
		{"query", notify.Config{"server": "https://x.example?a=1", "topic": "ok"}, "server", nil},
		{"empty query", notify.Config{"server": "https://x.example/?", "topic": "ok"}, "server", nil},
		{"fragment", notify.Config{"server": "https://x.example#f", "topic": "ok"}, "server", nil},
		{"opaque", notify.Config{"server": "https:x.example", "topic": "ok"}, "server", nil},
		{"control char", notify.Config{"server": "https://x.example/\x00", "topic": "ok"}, "server", nil},
		{"too long", notify.Config{"server": long, "topic": "ok"}, "server", nil},

		{"no topic", notify.Config{}, "topic", nil},
		{"blank topic", notify.Config{"topic": "  "}, "topic", nil},
		{"slash", notify.Config{"topic": "a/b"}, "topic", nil},
		{"dot dot", notify.Config{"topic": ".."}, "topic", nil},
		{"space", notify.Config{"topic": "a b"}, "topic", nil},
		{"umlaut", notify.Config{"topic": "räder"}, "topic", nil},
		{"newline", notify.Config{"topic": "a\nb"}, "topic", nil},
		{"query char", notify.Config{"topic": "a?b"}, "topic", nil},
		{"topic 65", notify.Config{"topic": strings.Repeat("a", 65)}, "topic", nil},

		{"token with space", notify.Config{"topic": "ok", "token": "a b"}, "token", nil},
		{"token with newline", notify.Config{"topic": "ok", "token": "a\r\nX-Evil: 1"}, "token", nil},
		{"token non-ascii", notify.Config{"topic": "ok", "token": "tk_äö"}, "token", nil},
		{"token too long", notify.Config{"topic": "ok", "token": strings.Repeat("a", 513)}, "token", nil},
		{"token 512", notify.Config{"topic": "ok", "token": strings.Repeat("a", 512)}, "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ntfy.Validate(tc.cfg)
			var fe *notify.FieldError
			if tc.field != "" {
				if !errors.As(err, &fe) || fe.Field != tc.field || fe.Msg == "" {
					t.Fatalf("got %v, %v; want error on %s", got, err, tc.field)
				}
				if strings.Contains(err.Error(), "tk_") {
					t.Errorf("error contains token: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("%s = %q, want %q (%v)", k, got[k], v, got)
				}
			}
			if tc.want != nil && len(got) != len(tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
	// Validating does not change the input.
	in := notify.Config{"server": " https://x.example/ ", "topic": " ok "}
	if _, err := ntfy.Validate(in); err != nil || in["topic"] != " ok " {
		t.Errorf("input changed: %v, %v", in, err)
	}
}

type recorded struct {
	r    *http.Request
	body string
}

func server(t *testing.T, status int) (*httptest.Server, *[]recorded) {
	t.Helper()
	var got []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, recorded{r, string(b)})
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func open(t *testing.T, cfg notify.Config) notify.Channel {
	t.Helper()
	ch, err := ntfy.Kind(ntfy.NewClient(true)).Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestSendOptionalHeaders(t *testing.T) {
	srv, got := server(t, 200)
	ch := open(t, notify.Config{"server": srv.URL, "topic": "t"})
	if err := ch.Send(context.Background(), notify.Message{Body: "plain"}); err != nil {
		t.Fatal(err)
	}
	r := (*got)[0].r
	for _, h := range []string{"Authorization", "Title", "Tags", "Click", "Priority"} {
		if v := r.Header.Get(h); v != "" {
			t.Errorf("header %s = %q on a message without it", h, v)
		}
	}
	if (*got)[0].body != "plain" || r.Header.Get("Content-Type") != "text/plain; charset=utf-8" {
		t.Errorf("request %q %v", (*got)[0].body, r.Header)
	}
	// Priority out of range is dropped; several tags are joined.
	_ = ch.Send(context.Background(), notify.Message{Body: "x", Priority: 9, Tags: []string{"warning", "bike"}})
	r = (*got)[1].r
	if r.Header.Get("Priority") != "" || r.Header.Get("Tags") != "warning,bike" {
		t.Errorf("headers %v", r.Header)
	}
	_ = ch.Send(context.Background(), notify.Message{Body: "x", Priority: 1})
	if (*got)[2].r.Header.Get("Priority") != "1" {
		t.Errorf("priority %v", (*got)[2].r.Header)
	}
}

func TestSendEncodesTitle(t *testing.T) {
	srv, got := server(t, 200)
	ch := open(t, notify.Config{"server": srv.URL, "topic": "t"})
	for _, title := range []string{"Rad Müller braucht Aufmerksamkeit", "Rennrad 🚴", "A\r\nX-Injected: 1"} {
		*got = nil
		if err := ch.Send(context.Background(), notify.Message{Title: title, Body: "x", Link: "https://x.example/bikes/1\r\nX-Injected: 1"}); err != nil {
			t.Fatal(err)
		}
		r := (*got)[0].r
		if r.Header.Get("X-Injected") != "" {
			t.Errorf("header injected for %q: %v", title, r.Header)
		}
		dec, err := new(mime.WordDecoder).DecodeHeader(r.Header.Get("Title"))
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Join(strings.Fields(title), " ")
		if dec != want {
			t.Errorf("title %q decodes to %q, want %q", r.Header.Get("Title"), dec, want)
		}
	}
	// Plain ASCII stays readable.
	*got = nil
	_ = ch.Send(context.Background(), notify.Message{Title: "Gravel needs attention", Body: "x"})
	if v := (*got)[0].r.Header.Get("Title"); v != "Gravel needs attention" {
		t.Errorf("title %q", v)
	}
}

func TestSendTruncatesBody(t *testing.T) {
	srv, got := server(t, 200)
	ch := open(t, notify.Config{"server": srv.URL, "topic": "t"})
	// ä is two bytes: 4001 bytes would split one.
	body := strings.Repeat("ä", 2500)
	if err := ch.Send(context.Background(), notify.Message{Body: body}); err != nil {
		t.Fatal(err)
	}
	b := (*got)[0].body
	if len(b) > 4000 || !utf8.ValidString(b) || len(b) < 3998 {
		t.Errorf("body of %d bytes, valid %v", len(b), utf8.ValidString(b))
	}
	_ = ch.Send(context.Background(), notify.Message{Body: strings.Repeat("a", 4000)})
	if len((*got)[1].body) != 4000 {
		t.Errorf("4000 bytes became %d", len((*got)[1].body))
	}
}

func TestSendStatuses(t *testing.T) {
	for _, tc := range []struct {
		status int
		want   error
	}{
		{200, nil},
		{204, nil},
		{299, nil},
		{400, notify.ErrRejected},
		{404, notify.ErrRejected},
		{429, notify.ErrRejected},
		{500, notify.ErrRejected},
		{503, notify.ErrRejected},
		{401, notify.ErrUnauthorized},
		{403, notify.ErrUnauthorized},
		{301, notify.ErrRejected},
		{302, notify.ErrRejected},
		{307, notify.ErrRejected},
		{308, notify.ErrRejected},
	} {
		srv, _ := server(t, tc.status)
		ch := open(t, notify.Config{"server": srv.URL, "topic": "mytopic", "token": "tk_secret"})
		err := ch.Send(context.Background(), notify.Message{Body: "x"})
		switch {
		case tc.want == nil && err != nil:
			t.Errorf("%d: %v", tc.status, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%d: %v, want %v", tc.status, err, tc.want)
		}
		if err != nil && (strings.Contains(err.Error(), "tk_secret") || strings.Contains(err.Error(), "mytopic")) {
			t.Errorf("%d: error leaks secrets: %v", tc.status, err)
		}
		if err != nil && notify.ErrorCode(err) == notify.ErrorFailed {
			t.Errorf("%d: no code for %v", tc.status, err)
		}
	}
}

// A redirect is not followed: the token must not go to another host.
func TestSendDoesNotFollowRedirects(t *testing.T) {
	var hits atomic.Int32
	var auth atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		auth.Store(r.Header.Get("Authorization"))
	}))
	defer target.Close()
	for _, code := range []int{301, 302, 307, 308} {
		redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL+"/stolen", code)
		}))
		ch := open(t, notify.Config{"server": redirect.URL, "topic": "t", "token": "tk_secret"})
		err := ch.Send(context.Background(), notify.Message{Body: "x"})
		redirect.Close()
		if !errors.Is(err, notify.ErrRejected) {
			t.Errorf("%d: %v", code, err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("redirect followed %d times, Authorization %v", hits.Load(), auth.Load())
	}
}

func TestSendUnreachableAndCanceled(t *testing.T) {
	srv, _ := server(t, 200)
	url := srv.URL
	srv.Close()
	ch := open(t, notify.Config{"server": url, "topic": "mytopic", "token": "tk_secret"})
	err := ch.Send(context.Background(), notify.Message{Body: "x"})
	if !errors.Is(err, notify.ErrUnreachable) || strings.Contains(err.Error(), "mytopic") || strings.Contains(err.Error(), "tk_secret") {
		t.Errorf("closed: %v", err)
	}
	// Unresolvable host and a canceled context.
	ch = open(t, notify.Config{"server": "http://does-not-exist.invalid", "topic": "mytopic"})
	if err := ch.Send(context.Background(), notify.Message{Body: "x"}); !errors.Is(err, notify.ErrUnreachable) || strings.Contains(err.Error(), "mytopic") {
		t.Errorf("unresolvable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	srv2, got := server(t, 200)
	ch = open(t, notify.Config{"server": srv2.URL, "topic": "mytopic"})
	if err := ch.Send(ctx, notify.Message{Body: "x"}); err == nil || strings.Contains(err.Error(), "mytopic") {
		t.Errorf("canceled: %v", err)
	}
	if len(*got) != 0 {
		t.Error("sent with a canceled context")
	}
}

// A slow server does not block for ever.
func TestSendTimeout(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	defer srv.Close()
	defer close(block)
	client := ntfy.NewClient(true)
	if client.Timeout <= 0 || client.Timeout > time.Minute {
		t.Fatalf("timeout %v", client.Timeout)
	}
	client.Timeout = 100 * time.Millisecond
	ch, err := ntfy.Kind(client).Open(notify.Config{"server": srv.URL, "topic": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(context.Background(), notify.Message{Body: "x"}); !errors.Is(err, notify.ErrUnreachable) {
		t.Errorf("timeout: %v", err)
	}
}

func TestOpenValidates(t *testing.T) {
	_, err := ntfy.Kind(ntfy.NewClient(true)).Open(notify.Config{"topic": "a/b"})
	var fe *notify.FieldError
	if !errors.As(err, &fe) || fe.Field != "topic" {
		t.Errorf("Open: %v", err)
	}
	k := ntfy.Kind(nil)
	if k.ID != "ntfy" || len(k.Fields) != 3 {
		t.Errorf("kind %+v", k)
	}
	for _, f := range k.Fields {
		if f.Label == "" || (f.Name == ntfy.FieldToken) != f.Secret {
			t.Errorf("field %+v", f)
		}
	}
}
