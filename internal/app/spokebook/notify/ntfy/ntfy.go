// Package ntfy sends notifications to a topic of an ntfy server
// (https://ntfy.sh or a self-hosted one).
package ntfy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spokebook/spokebook/internal/app/spokebook/notify"
)

// ID is the kind of channel.
const ID = "ntfy"

// DefaultServer is the server of a new channel.
const DefaultServer = "https://ntfy.sh"

// Configuration fields.
const (
	FieldServer = "server"
	FieldTopic  = "topic"
	FieldToken  = "token"
)

const (
	timeout = 15 * time.Second
	// maxBody keeps a message below ntfy's limit of 4096 bytes, above
	// which it becomes an attachment.
	maxBody     = 4000
	maxURLLen   = 2000
	maxTokenLen = 512
	// maxResponse limits how much of the response is read.
	maxResponse = 64 << 10
)

var topicRE = regexp.MustCompile(`^[-_A-Za-z0-9]{1,64}$`)

// NewClient returns the HTTP client for sending: with a timeout, without
// following redirects or using a proxy, and connecting only to allowed
// addresses (see Allowed): with allowPrivate also to loopback and private
// ones. The address is checked when connecting, after resolving the name.
func NewClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: timeout, KeepAlive: 30 * time.Second, Control: control(allowPrivate)}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	// A proxy would connect instead, past the check of the address.
	transport.Proxy = nil
	transport.DialContext = dialer.DialContext
	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// Kind returns the kind of channel; client sends the messages (see
// NewClient).
func Kind(client *http.Client) notify.Kind {
	return notify.Kind{
		ID: ID,
		Fields: []notify.Field{
			{
				Name: FieldServer, Label: "notify.ntfy.server", Hint: "notify.ntfy.server_hint", Type: "url",
				Required: true, Default: DefaultServer,
			},
			{Name: FieldTopic, Label: "notify.ntfy.topic", Hint: "notify.ntfy.topic_hint", Type: "text", Required: true},
			{Name: FieldToken, Label: "notify.ntfy.token", Hint: "notify.ntfy.token_hint", Type: "password", Secret: true},
		},
		Validate: Validate,
		Open: func(cfg notify.Config) (notify.Channel, error) {
			cfg, err := Validate(cfg)
			if err != nil {
				return nil, err
			}
			return &channel{client: client, url: cfg[FieldServer] + "/" + cfg[FieldTopic], token: cfg[FieldToken]}, nil
		},
	}
}

// Validate checks and normalizes a configuration: an http or https
// server (default DefaultServer) without query and not at an address that
// is never allowed (e.g. link-local), a topic of 1 to 64
// letters, digits, "-" and "_", and an optional access token.
func Validate(cfg notify.Config) (notify.Config, error) {
	server := strings.TrimSuffix(strings.TrimSpace(cfg[FieldServer]), "/")
	if server == "" {
		server = DefaultServer
	}
	u, err := url.Parse(server)
	if err != nil || len(server) > maxURLLen || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
		u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil, &notify.FieldError{Field: FieldServer, Msg: "notify.ntfy.error.server"}
	}
	if neverAllowed(u.Hostname()) {
		return nil, &notify.FieldError{Field: FieldServer, Msg: "notify.ntfy.error.server_address"}
	}
	topic := strings.TrimSpace(cfg[FieldTopic])
	if !topicRE.MatchString(topic) {
		return nil, &notify.FieldError{Field: FieldTopic, Msg: "notify.ntfy.error.topic"}
	}
	out := notify.Config{FieldServer: server, FieldTopic: topic}
	if token := strings.TrimSpace(cfg[FieldToken]); token != "" {
		if len(token) > maxTokenLen || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r > '~' }) {
			return nil, &notify.FieldError{Field: FieldToken, Msg: "notify.ntfy.error.token"}
		}
		out[FieldToken] = token
	}
	return out, nil
}

type channel struct {
	client *http.Client
	// url is server and topic.
	url   string
	token string
}

// Send publishes the message. Errors contain neither topic nor token.
func (c *channel) Send(ctx context.Context, m notify.Message) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(truncate(m.Body, maxBody)))
	if err != nil {
		return errors.New("ntfy: invalid server")
	}
	req.Header.Set("Content-Type", "text/plain; charset=utf-8")
	if m.Title != "" {
		req.Header.Set("Title", header(m.Title))
	}
	if m.Priority >= 1 && m.Priority <= 5 {
		req.Header.Set("Priority", strconv.Itoa(m.Priority))
	}
	if len(m.Tags) > 0 {
		req.Header.Set("Tags", header(strings.Join(m.Tags, ",")))
	}
	if m.Link != "" {
		req.Header.Set("Click", header(m.Link))
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		// A url.Error names the URL with the topic; keep only its cause.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		if errors.Is(err, errBlocked) {
			return fmt.Errorf("%w: ntfy: %w", notify.ErrBlocked, err)
		}
		return fmt.Errorf("%w: ntfy: %w", notify.ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponse))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return fmt.Errorf("%w: ntfy: status %d", notify.ErrUnauthorized, resp.StatusCode)
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		return fmt.Errorf("%w: ntfy: status %d", notify.ErrRejected, resp.StatusCode)
	}
	return nil
}

// header encodes a header value as RFC 2047 if it is not plain ASCII
// (ntfy decodes it); line breaks become spaces.
func header(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return mime.BEncoding.Encode("utf-8", s)
}

// truncate shortens s to at most n bytes without splitting a character.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
