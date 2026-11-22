package ntfy_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify/ntfy"
)

func TestSend(t *testing.T) {
	var got *http.Request
	var body string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.WriteHeader(status)
	}))
	defer srv.Close()
	kind := ntfy.Kind(ntfy.NewClient(true))
	cfg, err := kind.Validate(notify.Config{"server": srv.URL + "/", "topic": "bikes_secret-1", "token": "tk_abc"})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := kind.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	err = ch.Send(context.Background(), notify.Message{
		Title: "Gravel: Prüfung", Body: "- Monatscheck überfällig", Priority: 4,
		Tags: []string{"warning"}, Link: "https://bikes.example/bikes/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Method != http.MethodPost || got.URL.Path != "/bikes_secret-1" || body != "- Monatscheck überfällig" {
		t.Errorf("request %s %s %q", got.Method, got.URL.Path, body)
	}
	if got.Header.Get("Authorization") != "Bearer tk_abc" || got.Header.Get("Priority") != "4" ||
		got.Header.Get("Tags") != "warning" || got.Header.Get("Click") != "https://bikes.example/bikes/1" ||
		!strings.HasPrefix(got.Header.Get("Title"), "=?utf-8?b?") {
		t.Errorf("headers %v", got.Header)
	}
	status = http.StatusForbidden
	err = ch.Send(context.Background(), notify.Message{Body: "x"})
	if !errors.Is(err, notify.ErrUnauthorized) || strings.Contains(err.Error(), "tk_abc") {
		t.Errorf("forbidden: %v", err)
	}
	srv.Close()
	err = ch.Send(context.Background(), notify.Message{Body: "x"})
	if !errors.Is(err, notify.ErrUnreachable) || strings.Contains(err.Error(), "bikes_secret") {
		t.Errorf("closed: %v", err)
	}
}

func TestValidate(t *testing.T) {
	for _, tt := range []struct {
		cfg   notify.Config
		field string
	}{
		{notify.Config{"topic": "ok"}, ""},
		{notify.Config{"server": "ftp://x", "topic": "ok"}, "server"},
		{notify.Config{"server": "https://u:p@x", "topic": "ok"}, "server"},
		{notify.Config{"server": "https://x?a=1", "topic": "ok"}, "server"},
		{notify.Config{"topic": "a/b"}, "topic"},
		{notify.Config{"topic": strings.Repeat("a", 65)}, "topic"},
		{notify.Config{"topic": "ok", "token": "a b"}, "token"},
	} {
		cfg, err := ntfy.Validate(tt.cfg)
		var fe *notify.FieldError
		switch {
		case tt.field == "" && (err != nil || cfg["server"] != ntfy.DefaultServer):
			t.Errorf("%v: %v %v", tt.cfg, cfg, err)
		case tt.field != "" && (!errors.As(err, &fe) || fe.Field != tt.field):
			t.Errorf("%v: got %v, want error on %s", tt.cfg, err, tt.field)
		}
	}
}
