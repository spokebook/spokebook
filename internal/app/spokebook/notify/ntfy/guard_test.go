package ntfy_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
	"github.com/spokebook/spokebook/internal/app/spokebook/notify/ntfy"
)

func TestAllowed(t *testing.T) {
	for _, tc := range []struct {
		addr          string
		strict, local bool // allowed without and with allowPrivate
	}{
		{"1.1.1.1", true, true},
		{"159.69.1.1", true, true},
		{"2a01:4f8::1", true, true},

		{"127.0.0.1", false, true},
		{"127.1.2.3", false, true},
		{"::1", false, true},
		{"10.1.2.3", false, true},
		{"172.16.0.1", false, true},
		{"172.31.255.255", false, true},
		{"192.168.1.10", false, true},
		{"100.64.0.1", false, true},
		{"fd00::1", false, true},
		{"::ffff:192.168.1.1", false, true},

		{"172.32.0.1", true, true},
		{"100.128.0.1", true, true},

		{"0.0.0.0", false, false},
		{"0.1.2.3", false, false},
		{"::", false, false},
		{"169.254.169.254", false, false},
		{"::ffff:169.254.169.254", false, false},
		{"fe80::1", false, false},
		{"fe80::1%eth0", false, false},
		{"224.0.0.1", false, false},
		{"239.255.255.250", false, false},
		{"255.255.255.255", false, false},
		{"ff02::1", false, false},

		// Cloud metadata outside link-local, IETF assignments, site-local.
		{"fd00:ec2::254", false, false},
		{"fd00:ec2::253", false, true},
		{"100.100.100.200", false, false},
		{"100.100.100.201", false, true},
		{"192.0.0.170", false, false},
		{"fec0::1", false, false},
		{"168.63.129.16", false, false},
		{"fd20:ce::254", false, false},
		{"fd00:c1::a9fe:a9fe", false, false},

		// IPv6 that tunnels or translates to IPv4: 6to4, Teredo, SIIT.
		{"2002:a9fe:a9fe::1", false, false},
		{"2001:0:4136:e378:8000:63bf:3fff:fdd2", false, false},
		{"::ffff:0:a9fe:a9fe", false, false},
		{"2001:db8::1", true, true},

		// NAT64 and IPv4-compatible addresses count as the IPv4 address
		// they embed.
		{"64:ff9b::1.1.1.1", true, true},
		{"64:ff9b::127.0.0.1", false, true},
		{"64:ff9b::10.0.0.1", false, true},
		{"64:ff9b::169.254.169.254", false, false},
		{"64:ff9b::a9fe:a9fe", false, false},
		{"64:ff9b:1::1.1.1.1", false, true},
		{"::1.1.1.1", true, true},
		{"::127.0.0.1", false, true},
		{"::192.168.0.1", false, true},
		{"::169.254.169.254", false, false},
		{"::0.0.0.2", false, false},
		{"::ffff:127.0.0.1", false, true},
	} {
		a := netip.MustParseAddr(tc.addr)
		if got := ntfy.Allowed(a, false); got != tc.strict {
			t.Errorf("Allowed(%s, false) = %v", tc.addr, got)
		}
		if got := ntfy.Allowed(a, true); got != tc.local {
			t.Errorf("Allowed(%s, true) = %v", tc.addr, got)
		}
	}
	if ntfy.Allowed(netip.Addr{}, true) {
		t.Error("invalid address allowed")
	}
}

// Addresses that are never allowed are refused when saving; private ones
// may be allowed when sending, so they pass.
func TestValidateAddress(t *testing.T) {
	for _, s := range []string{
		"http://169.254.169.254", "http://[fe80::1]:80", "http://0.0.0.0:8080", "http://224.0.0.1",
		"http://[::]", "http://[::ffff:169.254.169.254]", "http://[fd00:ec2::254]", "http://100.100.100.200",
		"http://[64:ff9b::a9fe:a9fe]", "http://[::169.254.169.254]",
	} {
		_, err := ntfy.Validate(notify.Config{"server": s, "topic": "ok"})
		var fe *notify.FieldError
		if !errors.As(err, &fe) || fe.Field != ntfy.FieldServer || fe.Msg != "notify.ntfy.error.server_address" {
			t.Errorf("%s: %v", s, err)
		}
	}
	for _, s := range []string{"http://127.0.0.1:8080", "http://192.168.1.5", "http://[::1]", "https://ntfy.example.org", "http://localhost"} {
		if _, err := ntfy.Validate(notify.Config{"server": s, "topic": "ok"}); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
}

// The client checks the address it connects to, whatever the name is.
func TestClientBlocksPrivate(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	_, port, _ := net.SplitHostPort(u.Host)

	for _, server := range []string{srv.URL, "http://localhost:" + port} {
		ch, err := ntfy.Kind(ntfy.NewClient(false)).Open(notify.Config{"server": server, "topic": "secret_topic"})
		if err != nil {
			t.Fatal(err)
		}
		err = ch.Send(context.Background(), notify.Message{Body: "x"})
		if !errors.Is(err, notify.ErrBlocked) || notify.ErrorCode(err) != notify.ErrorBlocked {
			t.Errorf("%s: %v", server, err)
		}
		if err != nil && strings.Contains(err.Error(), "secret_topic") {
			t.Errorf("error names the topic: %v", err)
		}
	}
	if hits.Load() != 0 {
		t.Errorf("%d requests reached the server", hits.Load())
	}

	// Allowed by the operator.
	ch, err := ntfy.Kind(ntfy.NewClient(true)).Open(notify.Config{"server": srv.URL, "topic": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(context.Background(), notify.Message{Body: "x"}); err != nil {
		t.Fatal(err)
	}
	if hits.Load() != 1 {
		t.Errorf("%d requests", hits.Load())
	}
}

// A proxy from the environment would connect in the client's place, past
// the check.
func TestClientUsesNoProxy(t *testing.T) {
	tr, ok := ntfy.NewClient(false).Transport.(*http.Transport)
	if !ok || tr.Proxy != nil || tr.DialContext == nil {
		t.Errorf("transport %+v", tr)
	}
}
