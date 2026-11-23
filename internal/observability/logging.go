package observability

import (
	"context"
	"io"
	"log/slog"
)

// NewLogger returns a JSON slog logger that adds correlation_id from the
// context to every record that has one. Unknown levels fall back to info.
func NewLogger(w io.Writer, level string) *slog.Logger {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(level)); err != nil {
		lvl = slog.LevelInfo
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(ctxHandler{h})
}

type ctxHandler struct{ slog.Handler }

func (h ctxHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := CorrelationID(ctx); id != "" {
		r.AddAttrs(slog.String("correlation_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

func (h ctxHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return ctxHandler{h.Handler.WithAttrs(attrs)}
}

func (h ctxHandler) WithGroup(name string) slog.Handler {
	return ctxHandler{h.Handler.WithGroup(name)}
}
