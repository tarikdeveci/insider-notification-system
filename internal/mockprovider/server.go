// Package mockprovider is a configurable stand-in for the external notification
// provider. It dedups on Idempotency-Key, can fail, throttle or delay on demand,
// and records every arrival so e2e tests can inspect what the worker sent.
// All state lives in memory behind one mutex.
package mockprovider

import (
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const maxBody = 1 << 20

// Allowed response statuses. 1xx is excluded: Go follows a 1xx write with a final 200,
// so the client would see success while the arrivals log said failure.
const (
	MinStatus = 200
	MaxStatus = 599
)

// Config is the live behavior of the delivery endpoint.
type Config struct {
	FailureRate   float64 `json:"failure_rate"`
	FailureStatus int     `json:"failure_status"`
	LatencyMS     int     `json:"latency_ms"`
	Throttle      bool    `json:"throttle"`
	RetryAfterS   int     `json:"retry_after_s"`
}

// FieldError names the config field that failed validation, never its value.
type FieldError struct {
	Field string
	Msg   string
}

func (e *FieldError) Error() string { return e.Field + " " + e.Msg }

// Validate checks every range (NaN and Inf included); the error names the field only.
func (c Config) Validate() error {
	switch {
	case math.IsNaN(c.FailureRate) || math.IsInf(c.FailureRate, 0) || c.FailureRate < 0 || c.FailureRate > 1:
		return &FieldError{"failure_rate", "must be between 0 and 1"}
	case c.FailureStatus < MinStatus || c.FailureStatus > MaxStatus:
		return &FieldError{"failure_status", "must be between 200 and 599"}
	case c.LatencyMS < 0 || c.LatencyMS > 60000:
		return &FieldError{"latency_ms", "must be between 0 and 60000"}
	case c.RetryAfterS < 0 || c.RetryAfterS > 3600:
		return &FieldError{"retry_after_s", "must be between 0 and 3600"}
	}
	return nil
}

// Options configures New. Config is both the initial and the reset config.
type Options struct {
	Config Config
	Rand   func() float64 // returns [0,1); defaults to math/rand/v2
	Logger *slog.Logger   // defaults to slog.Default()
}

type arrival struct {
	Key          string `json:"idempotency_key"`
	To           string `json:"to"`
	Channel      string `json:"channel"`
	Status       int    `json:"status"`
	Deduplicated bool   `json:"deduplicated"`
	At           string `json:"at"`
}

// Server is the mock provider http.Handler.
type Server struct {
	defaults Config
	rnd      func() float64
	log      *slog.Logger
	mux      *http.ServeMux

	mu        sync.Mutex
	cfg       Config
	scripts   map[string][]int  // recipient -> remaining statuses
	delivered map[string]string // idempotency key -> messageId
	arrivals  []arrival
}

// New builds a Server.
func New(o Options) *Server {
	s := &Server{defaults: o.Config, rnd: o.Rand, log: o.Logger}
	if s.rnd == nil {
		s.rnd = rand.Float64
	}
	if s.log == nil {
		s.log = slog.Default()
	}
	s.resetLocked()
	s.mux = http.NewServeMux()
	s.mux.HandleFunc("POST /{$}", s.handleDeliver)
	s.mux.HandleFunc("GET /_control/config", s.handleGetConfig)
	s.mux.HandleFunc("PUT /_control/config", s.handlePutConfig)
	s.mux.HandleFunc("POST /_control/script", s.handleScript)
	s.mux.HandleFunc("GET /_control/arrivals", s.handleArrivals)
	s.mux.HandleFunc("POST /_control/reset", s.handleReset)
	return s
}

func (s *Server) resetLocked() {
	s.cfg = s.defaults
	s.scripts = map[string][]int{}
	s.delivered = map[string]string{}
	s.arrivals = nil
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// ServeHTTP logs method, matched route pattern, status and key only; never recipients or bodies.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	s.mux.ServeHTTP(rec, r)
	route := r.Pattern
	if route == "" {
		route = "unmatched"
	}
	s.log.Info("request", "method", r.Method, "route", route, "status", rec.status,
		"idempotency_key", r.Header.Get("Idempotency-Key"))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// decodeStrict reads exactly one JSON value into v, rejecting unknown fields and trailing data.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

func newMessageID() string {
	b := make([]byte, 12)
	_, _ = cryptorand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) handleDeliver(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		writeErr(w, http.StatusBadRequest, "missing Idempotency-Key")
		return
	}
	var req struct {
		To      string  `json:"to"`
		Channel string  `json:"channel"`
		Content *string `json:"content"`
	}
	if err := decodeStrict(r, &req); err != nil || req.To == "" || req.Channel == "" || req.Content == nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}

	status, messageID, cfg := s.decide(key, req.To, req.Channel)

	if cfg.LatencyMS > 0 {
		t := time.NewTimer(time.Duration(cfg.LatencyMS) * time.Millisecond)
		select {
		case <-t.C:
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}

	if status >= 200 && status < 300 {
		writeJSON(w, status, map[string]string{"messageId": messageID, "status": "accepted"})
		return
	}
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(cfg.RetryAfterS))
	}
	writeErr(w, status, "mock provider failure")
}

// decide picks the response under the lock and records the arrival, so the
// dedup map and arrivals stay consistent under concurrent requests.
func (s *Server) decide(key, to, channel string) (status int, messageID string, cfg Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg = s.cfg
	rec := arrival{Key: key, To: to, Channel: channel, At: time.Now().UTC().Format(time.RFC3339Nano)}

	if id, ok := s.delivered[key]; ok {
		rec.Status, rec.Deduplicated = http.StatusAccepted, true
		s.arrivals = append(s.arrivals, rec)
		return http.StatusAccepted, id, cfg
	}

	switch {
	case len(s.scripts[to]) > 0:
		status = s.scripts[to][0]
		s.scripts[to] = s.scripts[to][1:]
	case cfg.Throttle:
		status = http.StatusTooManyRequests
	case cfg.FailureRate > 0 && s.rnd() < cfg.FailureRate:
		status = cfg.FailureStatus
	default:
		status = http.StatusAccepted
	}

	if status >= 200 && status < 300 {
		messageID = newMessageID()
		s.delivered[key] = messageID
	}
	rec.Status = status
	s.arrivals = append(s.arrivals, rec)
	return status, messageID, cfg
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	cfg := s.cfg
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	// Omitted fields fall back to the neutral values below, so the PUT is a full replace.
	cfg := Config{FailureStatus: http.StatusServiceUnavailable, RetryAfterS: 1}
	if err := decodeStrict(r, &cfg); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	if err := cfg.Validate(); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) handleScript(w http.ResponseWriter, r *http.Request) {
	var req struct {
		To       string `json:"to"`
		Statuses []int  `json:"statuses"`
	}
	if err := decodeStrict(r, &req); err != nil || req.To == "" || len(req.Statuses) == 0 {
		writeErr(w, http.StatusBadRequest, "invalid body")
		return
	}
	for _, st := range req.Statuses {
		if st < MinStatus || st > MaxStatus {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("status %d out of range", st))
			return
		}
	}
	s.mu.Lock()
	s.scripts[req.To] = append(s.scripts[req.To], req.Statuses...)
	queued := len(s.scripts[req.To])
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]int{"queued": queued})
}

func (s *Server) handleArrivals(w http.ResponseWriter, r *http.Request) {
	to, channel := r.URL.Query().Get("to"), r.URL.Query().Get("channel")
	s.mu.Lock()
	out := make([]arrival, 0, len(s.arrivals))
	for _, a := range s.arrivals {
		if (to == "" || a.To == to) && (channel == "" || a.Channel == channel) {
			out = append(out, a)
		}
	}
	delivered := len(s.delivered)
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"arrivals": out, "delivered": delivered})
}

func (s *Server) handleReset(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	s.resetLocked()
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
}
