package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/tarikdeveci/insider-notification-system/internal/observability/tracectx"
)

const (
	defaultTimeout = 5 * time.Second
	maxBodyBytes   = 64 << 10
)

// Message is one delivery request. ID travels only in the Idempotency-Key header.
type Message struct {
	ID, To, Channel, Content string
}

type requestBody struct {
	To      string `json:"to"`
	Channel string `json:"channel"`
	Content string `json:"content"`
}

// Client posts messages to the provider and classifies the outcome.
type Client struct {
	url      string
	http     *http.Client
	log      *slog.Logger
	unparsed prometheus.Counter
}

// Option customizes a Client.
type Option func(*Client)

// WithTimeout overrides the 5s request timeout (tests only).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// defaultMaxConns sizes the idle pool when WithMaxConns is not given: the
// default worker concurrency of 10 per channel across the 3 channels.
const defaultMaxConns = 30

// WithMaxConns sizes the transport's idle connection pool for n concurrent
// senders, so none of them reconnects after each request. n < 1 is ignored.
func WithMaxConns(n int) Option {
	return func(c *Client) {
		if n < 1 {
			return
		}
		if tr, ok := c.http.Transport.(*http.Transport); ok {
			tr.MaxIdleConnsPerHost = n
			tr.MaxIdleConns = n
			tr.MaxConnsPerHost = 0
		}
	}
}

// newTransport clones the default transport (proxy, dial, TLS and HTTP/2
// settings) and sizes the idle pool for the default concurrency.
func newTransport() *http.Transport {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = defaultMaxConns
	tr.MaxIdleConns = defaultMaxConns
	return tr
}

// WithLogger sets the logger; the default discards output.
func WithLogger(l *slog.Logger) Option {
	return func(c *Client) {
		if l != nil {
			c.log = l
		}
	}
}

// WithUnparsedCounter wires the provider_unparsed_body_total counter.
func WithUnparsedCounter(ctr prometheus.Counter) Option {
	return func(c *Client) {
		if ctr != nil {
			c.unparsed = ctr
		}
	}
}

var errInvalidURL = errors.New("provider: PROVIDER_URL must be an http(s) URL with a host and no userinfo")

// NewClient validates rawURL. The returned error never contains rawURL.
func NewClient(rawURL string, opts ...Option) (*Client, error) {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return nil, errInvalidURL
	}
	c := &Client{
		url: rawURL,
		http: &http.Client{
			Timeout:   defaultTimeout,
			Transport: newTransport(),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		log:      slog.New(slog.NewTextHandler(io.Discard, nil)),
		unparsed: prometheus.NewCounter(prometheus.CounterOpts{Name: "provider_unparsed_body_total"}),
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Timeout reports the per-request timeout.
func (c *Client) Timeout() time.Duration { return c.http.Timeout }

// Send delivers m once and classifies the outcome. It never returns an error:
// transport failures are results too.
func (c *Client) Send(ctx context.Context, m Message) Result {
	payload, err := json.Marshal(requestBody{To: m.To, Channel: m.Channel, Content: m.Content})
	if err != nil {
		return Result{Class: Permanent, ErrCode: CodeConnError, Detail: "request body could not be encoded"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return Result{Class: Permanent, ErrCode: CodeConnError, Detail: "request could not be built"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", m.ID)
	tracectx.Inject(ctx, req.Header) // W3C traceparent of the active span; a no-op without one

	resp, err := c.http.Do(req)
	if err != nil {
		return ClassifyError(err)
	}
	// A read error leaves a partial body, which fails to parse and is counted as unparsed.
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	res, unparsed := classifyAt(time.Now(), resp.StatusCode, resp.Header, body)
	if unparsed {
		c.unparsed.Inc()
		c.log.Warn("provider 2xx body had no messageId",
			slog.String("notification_id", m.ID), slog.Int("http_status", resp.StatusCode))
	}
	return res
}
